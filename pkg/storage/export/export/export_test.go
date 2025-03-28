/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2022 Red Hat, Inc.
 *
 */
package export

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	routev1 "github.com/openshift/api/route/v1"
	k8sv1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"

	vsv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	framework "k8s.io/client-go/tools/cache/testing"
	virtv1 "kubevirt.io/api/core/v1"
	exportv1 "kubevirt.io/api/export/v1alpha1"
	snapshotv1 "kubevirt.io/api/snapshot/v1alpha1"
	kubevirtfake "kubevirt.io/client-go/generated/kubevirt/clientset/versioned/fake"
	"kubevirt.io/client-go/kubecli"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/kubevirt/pkg/certificates/bootstrap"
	"kubevirt.io/kubevirt/pkg/certificates/triple"
	certutil "kubevirt.io/kubevirt/pkg/certificates/triple/cert"
	virtcontroller "kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/testutils"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
	"kubevirt.io/kubevirt/pkg/virt-operator/resource/generate/components"
)

const (
	testNamespace = "default"
)

var (
	qemuGid int64 = 107
)

var _ = Describe("Export controller", func() {
	var (
		ctrl                       *gomock.Controller
		controller                 *VMExportController
		recorder                   *record.FakeRecorder
		pvcInformer                cache.SharedIndexInformer
		podInformer                cache.SharedIndexInformer
		cmInformer                 cache.SharedIndexInformer
		vmExportInformer           cache.SharedIndexInformer
		vmExportSource             *framework.FakeControllerSource
		serviceInformer            cache.SharedIndexInformer
		dvInformer                 cache.SharedIndexInformer
		vmSnapshotInformer         cache.SharedIndexInformer
		vmSnapshotContentInformer  cache.SharedIndexInformer
		secretInformer             cache.SharedIndexInformer
		vmInformer                 cache.SharedIndexInformer
		vmiInformer                cache.SharedIndexInformer
		kvInformer                 cache.SharedIndexInformer
		crdInformer                cache.SharedIndexInformer
		k8sClient                  *k8sfake.Clientset
		vmExportClient             *kubevirtfake.Clientset
		fakeVolumeSnapshotProvider *MockVolumeSnapshotProvider
		mockVMExportQueue          *testutils.MockWorkQueue
		routeCache                 cache.Store
		ingressCache               cache.Store
		certDir                    string
		certFilePath               string
		keyFilePath                string
		stop                       chan struct{}
	)

	syncCaches := func(stop chan struct{}) {
		go vmExportInformer.Run(stop)
		go pvcInformer.Run(stop)
		go podInformer.Run(stop)
		go dvInformer.Run(stop)
		go cmInformer.Run(stop)
		go serviceInformer.Run(stop)
		go secretInformer.Run(stop)
		go vmSnapshotInformer.Run(stop)
		go vmSnapshotContentInformer.Run(stop)
		go vmInformer.Run(stop)
		go vmiInformer.Run(stop)
		go crdInformer.Run(stop)
		go kvInformer.Run(stop)
		Expect(cache.WaitForCacheSync(
			stop,
			vmExportInformer.HasSynced,
			pvcInformer.HasSynced,
			podInformer.HasSynced,
			dvInformer.HasSynced,
			cmInformer.HasSynced,
			serviceInformer.HasSynced,
			secretInformer.HasSynced,
			vmSnapshotInformer.HasSynced,
			vmSnapshotContentInformer.HasSynced,
			vmInformer.HasSynced,
			vmiInformer.HasSynced,
			crdInformer.HasSynced,
			kvInformer.HasSynced,
		)).To(BeTrue())
	}

	BeforeEach(func() {
		stop = make(chan struct{})
		ctrl = gomock.NewController(GinkgoT())
		var err error
		certDir, err = os.MkdirTemp("", "certs")
		Expect(err).ToNot(HaveOccurred())
		certFilePath = filepath.Join(certDir, "tls.crt")
		keyFilePath = filepath.Join(certDir, "tls.key")
		writeCertsToDir(certDir)
		virtClient := kubecli.NewMockKubevirtClient(ctrl)
		pvcInformer, _ = testutils.NewFakeInformerFor(&k8sv1.PersistentVolumeClaim{})
		podInformer, _ = testutils.NewFakeInformerFor(&k8sv1.Pod{})
		cmInformer, _ = testutils.NewFakeInformerFor(&k8sv1.ConfigMap{})
		serviceInformer, _ = testutils.NewFakeInformerFor(&k8sv1.Service{})
		vmExportInformer, vmExportSource = testutils.NewFakeInformerWithIndexersFor(&exportv1.VirtualMachineExport{}, virtcontroller.GetVirtualMachineExportInformerIndexers())
		dvInformer, _ = testutils.NewFakeInformerFor(&cdiv1.DataVolume{})
		vmSnapshotInformer, _ = testutils.NewFakeInformerFor(&snapshotv1.VirtualMachineSnapshot{})
		vmSnapshotContentInformer, _ = testutils.NewFakeInformerFor(&snapshotv1.VirtualMachineSnapshotContent{})
		vmInformer, _ = testutils.NewFakeInformerFor(&virtv1.VirtualMachine{})
		vmiInformer, _ = testutils.NewFakeInformerFor(&virtv1.VirtualMachineInstance{})
		routeInformer, _ := testutils.NewFakeInformerFor(&routev1.Route{})
		routeCache = routeInformer.GetStore()
		ingressInformer, _ := testutils.NewFakeInformerFor(&networkingv1.Ingress{})
		ingressCache = ingressInformer.GetStore()
		secretInformer, _ = testutils.NewFakeInformerFor(&k8sv1.Secret{})
		kvInformer, _ = testutils.NewFakeInformerFor(&virtv1.KubeVirt{})
		crdInformer, _ = testutils.NewFakeInformerFor(&extv1.CustomResourceDefinition{})
		fakeVolumeSnapshotProvider = &MockVolumeSnapshotProvider{
			volumeSnapshots: []*vsv1.VolumeSnapshot{},
		}

		config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&virtv1.KubeVirtConfiguration{})
		k8sClient = k8sfake.NewSimpleClientset()
		vmExportClient = kubevirtfake.NewSimpleClientset()
		recorder = record.NewFakeRecorder(100)

		virtClient.EXPECT().CoreV1().Return(k8sClient.CoreV1()).AnyTimes()
		virtClient.EXPECT().VirtualMachineExport(testNamespace).
			Return(vmExportClient.ExportV1alpha1().VirtualMachineExports(testNamespace)).AnyTimes()

		controller = &VMExportController{
			Client:                    virtClient,
			Recorder:                  recorder,
			PVCInformer:               pvcInformer,
			PodInformer:               podInformer,
			ConfigMapInformer:         cmInformer,
			VMExportInformer:          vmExportInformer,
			ServiceInformer:           serviceInformer,
			DataVolumeInformer:        dvInformer,
			KubevirtNamespace:         "kubevirt",
			TemplateService:           services.NewTemplateService("a", 240, "b", "c", "d", "e", "f", "g", pvcInformer.GetStore(), virtClient, config, qemuGid, "h"),
			caCertManager:             bootstrap.NewFileCertificateManager(certFilePath, keyFilePath),
			RouteCache:                routeCache,
			IngressCache:              ingressCache,
			RouteConfigMapInformer:    cmInformer,
			SecretInformer:            secretInformer,
			VMSnapshotInformer:        vmSnapshotInformer,
			VMSnapshotContentInformer: vmSnapshotContentInformer,
			VolumeSnapshotProvider:    fakeVolumeSnapshotProvider,
			VMInformer:                vmInformer,
			VMIInformer:               vmiInformer,
			CRDInformer:               crdInformer,
			KubeVirtInformer:          kvInformer,
		}
		initCert = func(ctrl *VMExportController) {
			go controller.caCertManager.Start()
			// Give the thread time to read the certs.
			Eventually(func() *tls.Certificate {
				return controller.caCertManager.Current()
			}, time.Second, time.Millisecond).ShouldNot(BeNil())
		}

		controller.Init()
		mockVMExportQueue = testutils.NewMockWorkQueue(controller.vmExportQueue)
		controller.vmExportQueue = mockVMExportQueue

		Expect(
			cmInformer.GetStore().Add(&k8sv1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: controller.KubevirtNamespace,
					Name:      components.KubeVirtExportCASecretName,
				},
				Data: map[string]string{
					"ca-bundle": "replace me with ca cert",
				},
			}),
		).To(Succeed())

		Expect(
			kvInformer.GetStore().Add(&virtv1.KubeVirt{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: controller.KubevirtNamespace,
					Name:      "kv",
				},
				Spec: virtv1.KubeVirtSpec{
					CertificateRotationStrategy: virtv1.KubeVirtCertificateRotateStrategy{
						SelfSigned: &virtv1.KubeVirtSelfSignConfiguration{
							CA: &virtv1.CertConfig{
								Duration:    &metav1.Duration{Duration: 24 * time.Hour},
								RenewBefore: &metav1.Duration{Duration: 3 * time.Hour},
							},
							Server: &virtv1.CertConfig{
								Duration:    &metav1.Duration{Duration: 2 * time.Hour},
								RenewBefore: &metav1.Duration{Duration: 1 * time.Hour},
							},
						},
					},
				},
				Status: virtv1.KubeVirtStatus{
					Phase: virtv1.KubeVirtPhaseDeployed,
				},
			}),
		).To(Succeed())
	})

	AfterEach(func() {
		controller.caCertManager.Stop()
		Expect(os.RemoveAll(certDir)).To(Succeed())
	})

	generateCertFromTime := func(cn string, before, after *time.Time) string {
		defer GinkgoRecover()
		config := certutil.Config{
			CommonName: cn,
			NotBefore:  before,
			NotAfter:   after,
		}
		defer GinkgoRecover()
		caKeyPair, _ := triple.NewCA("kubevirt.io", time.Hour*24*7)

		intermediateKey, err := certutil.NewPrivateKey()
		Expect(err).ToNot(HaveOccurred())
		intermediateConfig := certutil.Config{
			CommonName: fmt.Sprintf("%s@%d", "intermediate", time.Now().Unix()),
			NotBefore:  before,
			NotAfter:   after,
		}
		intermediateConfig.Usages = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		intermediateCert, err := certutil.NewSignedCert(intermediateConfig, intermediateKey, caKeyPair.Cert, caKeyPair.Key, time.Hour)
		Expect(err).ToNot(HaveOccurred())

		key, err := certutil.NewPrivateKey()
		Expect(err).ToNot(HaveOccurred())

		config.AltNames.DNSNames = []string{"hahaha.wwoo", "*.apps-crc.testing", "fgdgd.dfsgdf"}
		config.Usages = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		cert, err := certutil.NewSignedCert(config, key, intermediateCert, intermediateKey, time.Hour)
		Expect(err).ToNot(HaveOccurred())
		pemOut := strings.Builder{}
		pem.Encode(&pemOut, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
		pem.Encode(&pemOut, &pem.Block{Type: "CERTIFICATE", Bytes: intermediateCert.Raw})
		pem.Encode(&pemOut, &pem.Block{Type: "CERTIFICATE", Bytes: caKeyPair.Cert.Raw})
		return strings.TrimSpace(pemOut.String())
	}

	generateFutureCert := func() string {
		before := time.Now().AddDate(0, 1, 0)
		after := before.AddDate(0, 0, 7)
		return generateCertFromTime("future cert", &before, &after)
	}

	generateExpiredCert := func() string {
		before := time.Now().AddDate(0, -1, 0)
		after := before.AddDate(0, 0, 7)
		return generateCertFromTime("expired cert", &before, &after)
	}

	generateExpectedCert := func() string {
		before := time.Now()
		after := before.AddDate(0, 0, 7)
		return generateCertFromTime("working cert", &before, &after)
	}

	var expectedPem = generateExpectedCert()

	generateOverlappingCert := func() string {
		before := time.Now().AddDate(0, 0, -3)
		after := before.AddDate(0, 0, 7)
		return generateCertFromTime("overlapping cert", &before, &after) + "\n" + expectedPem
	}

	generateRouteCert := func() string {
		key, err := certutil.NewPrivateKey()
		Expect(err).ToNot(HaveOccurred())

		config := certutil.Config{
			CommonName: "something else",
		}

		cert, err := certutil.NewSelfSignedCACert(config, key, time.Hour)
		Expect(err).ToNot(HaveOccurred())
		pemOut := strings.Builder{}
		Expect(pem.Encode(&pemOut, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})).To(Succeed())
		return strings.TrimSpace(pemOut.String()) + "\n" + expectedPem
	}

	createRouteConfigMapFromFunc := func(certFunc func() string) *k8sv1.ConfigMap {
		return &k8sv1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      routeCAConfigMapName,
				Namespace: controller.KubevirtNamespace,
			},
			Data: map[string]string{
				routeCaKey: certFunc(),
			},
		}
	}

	createFutureRouteConfigMap := func() *k8sv1.ConfigMap {
		return createRouteConfigMapFromFunc(generateFutureCert)
	}

	createExpiredRouteConfigMap := func() *k8sv1.ConfigMap {
		return createRouteConfigMapFromFunc(generateExpiredCert)
	}

	createOverlappingRouteConfigMap := func() *k8sv1.ConfigMap {
		return createRouteConfigMapFromFunc(generateOverlappingCert)
	}

	createRouteConfigMap := func() *k8sv1.ConfigMap {
		return createRouteConfigMapFromFunc(generateRouteCert)
	}

	validIngressDefaultBackend := func(serviceName string) *networkingv1.Ingress {
		return &networkingv1.Ingress{
			Spec: networkingv1.IngressSpec{
				DefaultBackend: &networkingv1.IngressBackend{
					Service: &networkingv1.IngressServiceBackend{
						Name: serviceName,
					},
				},
				Rules: []networkingv1.IngressRule{
					{
						Host: "backend-host",
					},
				},
			},
		}
	}

	ingressDefaultBackendNoService := func() *networkingv1.Ingress {
		return &networkingv1.Ingress{
			Spec: networkingv1.IngressSpec{
				DefaultBackend: &networkingv1.IngressBackend{},
				Rules: []networkingv1.IngressRule{
					{
						Host: "backend-host",
					},
				},
			},
		}
	}

	validIngressRules := func(serviceName string) *networkingv1.Ingress {
		return &networkingv1.Ingress{
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{
					{
						IngressRuleValue: networkingv1.IngressRuleValue{
							HTTP: &networkingv1.HTTPIngressRuleValue{
								Paths: []networkingv1.HTTPIngressPath{
									{
										Backend: networkingv1.IngressBackend{
											Service: &networkingv1.IngressServiceBackend{
												Name: serviceName,
											},
										},
									},
								},
							},
						},
						Host: "rule-host",
					},
				},
			},
		}
	}

	ingressRulesNoBackend := func() *networkingv1.Ingress {
		return &networkingv1.Ingress{
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{
					{
						IngressRuleValue: networkingv1.IngressRuleValue{
							HTTP: &networkingv1.HTTPIngressRuleValue{
								Paths: []networkingv1.HTTPIngressPath{
									{
										Backend: networkingv1.IngressBackend{},
									},
								},
							},
						},
						Host: "rule-host",
					},
				},
			},
		}
	}

	It("should add vmexport to queue if matching PVC is added", func() {
		vmExport := createPVCVMExport()
		pvc := &k8sv1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPVCName,
				Namespace: testNamespace,
			},
		}
		syncCaches(stop)
		mockVMExportQueue.ExpectAdds(1)
		vmExportInformer.GetStore().Add(vmExport)
		controller.handlePVC(pvc)
		mockVMExportQueue.Wait()
	})

	It("should add vmexport to queue if matching VMSnapshot is added", func() {
		vmExport := createSnapshotVMExport()
		vmSnapshot := &snapshotv1.VirtualMachineSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmsnapshotName,
				Namespace: testNamespace,
			},
			Status: &snapshotv1.VirtualMachineSnapshotStatus{
				ReadyToUse: pointer.BoolPtr(false),
			},
		}
		syncCaches(stop)
		mockVMExportQueue.ExpectAdds(1)
		vmExportInformer.GetStore().Add(vmExport)
		controller.handleVMSnapshot(vmSnapshot)
		mockVMExportQueue.Wait()
	})

	It("should add vmexport to queue if matching VM is added", func() {
		vmExport := createVMVMExport()
		vm := &virtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmName,
				Namespace: testNamespace,
			},
			Spec: virtv1.VirtualMachineSpec{
				Template: &virtv1.VirtualMachineInstanceTemplateSpec{
					Spec: virtv1.VirtualMachineInstanceSpec{
						Volumes: []virtv1.Volume{},
					},
				},
			},
		}
		syncCaches(stop)
		mockVMExportQueue.ExpectAdds(1)
		vmExportInformer.GetStore().Add(vmExport)
		controller.handleVM(vm)
		mockVMExportQueue.Wait()
	})

	It("should add vmexport to queue if matching VMI is added", func() {
		vmExport := createVMVMExport()
		vm := &virtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmName,
				Namespace: testNamespace,
			},
			Spec: virtv1.VirtualMachineSpec{
				Template: &virtv1.VirtualMachineInstanceTemplateSpec{
					Spec: virtv1.VirtualMachineInstanceSpec{
						Volumes: []virtv1.Volume{},
					},
				},
			},
		}
		vmi := &virtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmName,
				Namespace: testNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: virtv1.GroupVersion.String(),
						Kind:       "VirtualMachine",
						Name:       testVmName,
						Controller: pointer.BoolPtr(true),
					},
				},
			},
		}
		syncCaches(stop)
		mockVMExportQueue.ExpectAdds(1)
		vmExportInformer.GetStore().Add(vmExport)
		vmInformer.GetStore().Add(vm)
		controller.handleVMI(vmi)
		mockVMExportQueue.Wait()
	})

	It("should NOT add vmexport to queue if VMI is added without matching VM", func() {
		vmExport := createVMVMExport()
		vm := &virtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmName + "-other",
				Namespace: testNamespace,
			},
			Spec: virtv1.VirtualMachineSpec{
				Template: &virtv1.VirtualMachineInstanceTemplateSpec{
					Spec: virtv1.VirtualMachineInstanceSpec{
						Volumes: []virtv1.Volume{},
					},
				},
			},
		}
		vmi := &virtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmName,
				Namespace: testNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: virtv1.GroupVersion.String(),
						Kind:       "VirtualMachine",
						Name:       testVmName,
						Controller: pointer.BoolPtr(true),
					},
				},
			},
		}
		syncCaches(stop)
		mockVMExportQueue.ExpectAdds(1)
		vmExportSource.Add(vmExport)
		vmInformer.GetStore().Add(vm)
		controller.handleVMI(vmi)
		mockVMExportQueue.Wait()
	})

	It("should NOT add vmexport to queue if VMI is added without owner", func() {
		vmExport := createVMVMExport()
		vmi := &virtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmName,
				Namespace: testNamespace,
			},
		}
		syncCaches(stop)
		mockVMExportQueue.ExpectAdds(1)
		vmExportSource.Add(vmExport)
		controller.handleVMI(vmi)
		mockVMExportQueue.Wait()
	})

	DescribeTable("should add vmexport to queue if VMI (pvc) is added that matches PVC export", func(source virtv1.VolumeSource) {
		vmExport := createPVCVMExport()
		vmi := &virtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmName,
				Namespace: testNamespace,
			},
			Spec: virtv1.VirtualMachineInstanceSpec{
				Volumes: []virtv1.Volume{
					{
						Name:         "testVolume",
						VolumeSource: source,
					},
				},
			},
		}
		pvc := &k8sv1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPVCName,
				Namespace: testNamespace,
			},
		}
		syncCaches(stop)
		mockVMExportQueue.ExpectAdds(2)
		vmExportSource.Add(vmExport)
		controller.processVMExportWorkItem()
		pvcInformer.GetStore().Add(pvc)
		vmiInformer.GetStore().Add(vmi)
		controller.handleVMI(vmi)
		mockVMExportQueue.Wait()
	},
		Entry("PVC", virtv1.VolumeSource{
			PersistentVolumeClaim: &virtv1.PersistentVolumeClaimVolumeSource{
				PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: testPVCName,
				},
			},
		}),
		Entry("DV", virtv1.VolumeSource{
			DataVolume: &virtv1.DataVolumeSource{
				Name: testPVCName,
			},
		}),
	)

	It("should not add vmexport to queue if VMI (dv) is added that doesn't match a PVC export", func() {
		vmExport := createPVCVMExport()
		vmi := &virtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testVmName,
				Namespace: testNamespace,
			},
			Spec: virtv1.VirtualMachineInstanceSpec{
				Volumes: []virtv1.Volume{
					{
						Name: "testVolume",
						VolumeSource: virtv1.VolumeSource{
							DataVolume: &virtv1.DataVolumeSource{
								Name: testPVCName,
							},
						},
					},
				},
			},
		}
		syncCaches(stop)
		mockVMExportQueue.ExpectAdds(1)
		vmExportSource.Add(vmExport)
		controller.processVMExportWorkItem()
		vmiInformer.GetStore().Add(vmi)
		controller.handleVMI(vmi)
		mockVMExportQueue.Wait()
	})

	It("Should create a service based on the name of the VMExport", func() {
		var service *k8sv1.Service
		testVMExport := createPVCVMExport()
		k8sClient.Fake.PrependReactor("create", "services", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			create, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			service, ok = create.GetObject().(*k8sv1.Service)
			Expect(ok).To(BeTrue())
			service.Status.Conditions = make([]metav1.Condition, 1)
			service.Status.Conditions[0].Type = "test"
			Expect(service.GetName()).To(Equal(controller.getExportServiceName(testVMExport)))
			Expect(service.GetNamespace()).To(Equal(testNamespace))
			return true, service, nil
		})

		service, err := controller.getOrCreateExportService(testVMExport)
		Expect(err).ToNot(HaveOccurred())
		Expect(service).ToNot(BeNil())
		Expect(service.Status.Conditions[0].Type).To(Equal("test"))

		Expect(
			serviceInformer.GetStore().Add(&k8sv1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controller.getExportServiceName(testVMExport),
					Namespace: testVMExport.Namespace,
				},
				Spec: k8sv1.ServiceSpec{},
				Status: k8sv1.ServiceStatus{
					Conditions: []metav1.Condition{
						{
							Type: "test2",
						},
					},
				},
			}),
		).To(Succeed())
		service, err = controller.getOrCreateExportService(testVMExport)
		Expect(err).ToNot(HaveOccurred())
		Expect(service).ToNot(BeNil())
		Expect(service.Status.Conditions[0].Type).To(Equal("test2"))
	})

	It("Should create a pod based on the name of the VMExport", func() {
		testPVC := &k8sv1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPVCName,
				Namespace: testNamespace,
			},
		}
		testVMExport := createPVCVMExport()
		populateInitialVMExportStatus(testVMExport)
		err := controller.handleVMExportToken(testVMExport)
		Expect(testVMExport.Status.TokenSecretRef).ToNot(BeNil())
		Expect(err).ToNot(HaveOccurred())
		k8sClient.Fake.PrependReactor("create", "pods", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			create, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			pod, ok := create.GetObject().(*k8sv1.Pod)
			Expect(ok).To(BeTrue())
			Expect(pod.GetName()).To(Equal(fmt.Sprintf("%s-%s", exportPrefix, testVMExport.Name)))
			Expect(pod.GetNamespace()).To(Equal(testNamespace))
			return true, pod, nil
		})
		pod, err := controller.createExporterPod(testVMExport, []*k8sv1.PersistentVolumeClaim{testPVC})
		Expect(err).ToNot(HaveOccurred())
		Expect(pod).ToNot(BeNil())
		Expect(pod.Name).To(Equal(fmt.Sprintf("%s-%s", exportPrefix, testVMExport.Name)))
		Expect(pod.Spec.Volumes).To(HaveLen(3), "There should be 3 volumes, one pvc, and two secrets (token and certs)")
		certSecretName := ""
		for _, volume := range pod.Spec.Volumes {
			if volume.Name == certificates {
				certSecretName = volume.Secret.SecretName
			}
		}
		Expect(pod.Spec.Volumes).To(ContainElement(k8sv1.Volume{
			Name: testPVCName,
			VolumeSource: k8sv1.VolumeSource{
				PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: testPVCName,
				},
			},
		}))
		Expect(pod.Spec.Volumes).To(ContainElement(k8sv1.Volume{
			Name: certificates,
			VolumeSource: k8sv1.VolumeSource{
				Secret: &k8sv1.SecretVolumeSource{
					SecretName: certSecretName,
				},
			},
		}))
		Expect(pod.Spec.Volumes).To(ContainElement(k8sv1.Volume{
			Name: "token",
			VolumeSource: k8sv1.VolumeSource{
				Secret: &k8sv1.SecretVolumeSource{
					SecretName: "token",
				},
			},
		}))
		Expect(pod.Spec.Containers).To(HaveLen(1))
		Expect(pod.Spec.Containers[0].VolumeMounts).To(HaveLen(3))
		Expect(pod.Spec.Containers[0].VolumeMounts).To(ContainElement(k8sv1.VolumeMount{
			Name:      testPVCName,
			ReadOnly:  true,
			MountPath: fmt.Sprintf("%s/%s", fileSystemMountPath, testPVC.Name),
		}))
		Expect(pod.Spec.Containers[0].VolumeMounts).To(ContainElement(k8sv1.VolumeMount{
			Name:      certificates,
			MountPath: "/cert",
		}))
		Expect(pod.Spec.Containers[0].VolumeMounts).To(ContainElement(k8sv1.VolumeMount{
			Name:      *testVMExport.Status.TokenSecretRef,
			MountPath: "/token",
		}))
		Expect(pod.Annotations[annCertParams]).To(Equal("{\"Duration\":7200000000000,\"RenewBefore\":3600000000000}"))
	})

	It("Should create a secret based on the vm export", func() {
		cp := &CertParams{Duration: 24 * time.Hour, RenewBefore: 2 * time.Hour}
		scp, err := serializeCertParams(cp)
		Expect(err).ToNot(HaveOccurred())
		testVMExport := createPVCVMExport()
		populateInitialVMExportStatus(testVMExport)
		err = controller.handleVMExportToken(testVMExport)
		Expect(err).ToNot(HaveOccurred())
		testExportPod := &k8sv1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-export-pod",
				Annotations: map[string]string{
					annCertParams: scp,
				},
			},
			Spec: k8sv1.PodSpec{
				Volumes: []k8sv1.Volume{
					{
						Name: certificates,
						VolumeSource: k8sv1.VolumeSource{
							Secret: &k8sv1.SecretVolumeSource{
								SecretName: "test-secret",
							},
						},
					},
				},
			},
		}
		k8sClient.Fake.PrependReactor("create", "secrets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			create, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			secret, ok := create.GetObject().(*k8sv1.Secret)
			Expect(ok).To(BeTrue())
			Expect(secret.GetName()).To(Equal(controller.getExportSecretName(testExportPod)))
			Expect(secret.GetNamespace()).To(Equal(testNamespace))
			return true, secret, nil
		})
		err = controller.createCertSecret(testVMExport, testExportPod)
		Expect(err).ToNot(HaveOccurred())
		By("Creating again, and returning exists")
		k8sClient.Fake.PrependReactor("create", "secrets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			create, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			secret, ok := create.GetObject().(*k8sv1.Secret)
			Expect(ok).To(BeTrue())
			Expect(secret.GetName()).To(Equal(controller.getExportSecretName(testExportPod)))
			Expect(secret.GetNamespace()).To(Equal(testNamespace))
			secret.Name = "something"
			return true, secret, errors.NewAlreadyExists(schema.GroupResource{}, "already exists")
		})
		err = controller.createCertSecret(testVMExport, testExportPod)
		Expect(err).ToNot(HaveOccurred())
		k8sClient.Fake.PrependReactor("create", "secrets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			create, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			secret, ok := create.GetObject().(*k8sv1.Secret)
			Expect(ok).To(BeTrue())
			Expect(secret.GetName()).To(Equal(controller.getExportSecretName(testExportPod)))
			Expect(secret.GetNamespace()).To(Equal(testNamespace))
			return true, nil, fmt.Errorf("failure")
		})
		err = controller.createCertSecret(testVMExport, testExportPod)
		Expect(err).To(HaveOccurred())
	})

	It("handleVMExportToken should create the export secret if no TokenSecretRef is specified", func() {
		testVMExport := createPVCVMExportWithoutSecret()
		expectedName := getDefaultTokenSecretName(testVMExport)
		k8sClient.Fake.PrependReactor("create", "secrets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			create, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			secret, ok := create.GetObject().(*k8sv1.Secret)
			Expect(ok).To(BeTrue())
			Expect(secret.GetName()).To(Equal(expectedName))
			Expect(secret.GetNamespace()).To(Equal(testNamespace))
			return true, secret, nil
		})
		populateInitialVMExportStatus(testVMExport)
		err := controller.handleVMExportToken(testVMExport)
		Expect(err).ToNot(HaveOccurred())
		Expect(testVMExport.Status.TokenSecretRef).ToNot(BeNil())
		Expect(*testVMExport.Status.TokenSecretRef).To(Equal(expectedName))
		testutils.ExpectEvent(recorder, secretCreatedEvent)
	})

	It("handleVMExportToken should use the already specified secret if the status is already populated", func() {
		testVMExport := createPVCVMExportWithoutSecret()
		oldSecretRef := "oldToken"
		newSecretRef := getDefaultTokenSecretName(testVMExport)
		testVMExport.Status = &exportv1.VirtualMachineExportStatus{
			TokenSecretRef: &oldSecretRef,
		}
		k8sClient.Fake.PrependReactor("create", "secrets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			create, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			secret, ok := create.GetObject().(*k8sv1.Secret)
			Expect(ok).To(BeTrue())
			Expect(secret.GetName()).To(Equal(oldSecretRef))
			Expect(secret.GetNamespace()).To(Equal(testNamespace))
			return true, secret, nil
		})
		err := controller.handleVMExportToken(testVMExport)
		Expect(err).ToNot(HaveOccurred())
		Expect(testVMExport.Status.TokenSecretRef).ToNot(BeNil())
		Expect(*testVMExport.Status.TokenSecretRef).ToNot(Equal(newSecretRef))
		Expect(*testVMExport.Status.TokenSecretRef).To(Equal(oldSecretRef))
		testutils.ExpectEvent(recorder, secretCreatedEvent)
	})

	It("handleVMExportToken should use the user-specified secret if TokenSecretRef is specified", func() {
		testVMExport := createPVCVMExport()
		Expect(testVMExport.Spec.TokenSecretRef).ToNot(BeNil())
		expectedName := *testVMExport.Spec.TokenSecretRef
		populateInitialVMExportStatus(testVMExport)
		err := controller.handleVMExportToken(testVMExport)
		Expect(err).ToNot(HaveOccurred())
		Expect(testVMExport.Status.TokenSecretRef).ToNot(BeNil())
		Expect(*testVMExport.Status.TokenSecretRef).To(Equal(expectedName))
	})

	It("Should completely clean up VM export, when TTL is reached", func() {
		var deleted bool
		testVMExport := createPVCVMExport()
		ttl := &metav1.Duration{Duration: time.Minute}
		testVMExport.Spec.TTLDuration = ttl
		// Artificially reach TTL expiration time
		testVMExport.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-1 * ttl.Duration)))
		pvc := &k8sv1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPVCName,
				Namespace: testNamespace,
			},
			Status: k8sv1.PersistentVolumeClaimStatus{
				Phase: k8sv1.ClaimBound,
			},
		}
		Expect(controller.PVCInformer.GetStore().Add(pvc)).To(Succeed())

		vmExportClient.Fake.PrependReactor("delete", "virtualmachineexports", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			delete, ok := action.(testing.DeleteAction)
			Expect(ok).To(BeTrue())
			Expect(delete.GetName()).To(Equal(testVMExport.GetName()))
			deleted = true
			return true, nil, nil
		})
		retry, err := controller.updateVMExport(testVMExport)
		Expect(deleted).To(BeTrue())
		// Status update fails (call UPDATE on deleted VMExport), but its fine in real world
		// since requeue will back out of the reconcile loop if a deletion timestamp is set
		Expect(err).To(HaveOccurred())
		Expect(retry).To(BeEquivalentTo(0))
	})

	DescribeTable("Should ignore invalid VMExports kind/api combinations", func(kind, apigroup string) {
		testVMExport := createPVCVMExport()
		testVMExport.Spec.Source.Kind = kind
		testVMExport.Spec.Source.APIGroup = &apigroup
		retry, err := controller.updateVMExport(testVMExport)
		Expect(err).ToNot(HaveOccurred())
		Expect(retry).To(BeEquivalentTo(0))
	},
		Entry("VirtualMachineSnapshot kind blank apigroup", "VirtualMachineSnapshot", ""),
		Entry("VirtualMachineSnapshot kind invalid apigroup", "VirtualMachineSnapshot", "invalid"),
		Entry("PersistentVolumeClaim kind invalid apigroup", "PersistentVolumeClaim", "invalid"),
		Entry("PersistentVolumeClaim kind VMSnapshot apigroup", "PersistentVolumeClaim", snapshotv1.SchemeGroupVersion.Group),
	)

	DescribeTable("should find host when Ingress is defined", func(ingress *networkingv1.Ingress, hostname string) {
		Expect(controller.IngressCache.Add(ingress)).To(Succeed())
		host, _ := controller.getExternalLinkHostAndCert()
		Expect(hostname).To(Equal(host))
	},
		Entry("ingress with default backend host", validIngressDefaultBackend(components.VirtExportProxyServiceName), "backend-host"),
		Entry("ingress with default backend host different service", validIngressDefaultBackend("other-service"), ""),
		Entry("ingress with rules host", validIngressRules(components.VirtExportProxyServiceName), "rule-host"),
		Entry("ingress with rules host different service", validIngressRules("other-service"), ""),
		Entry("ingress with no default backend service", ingressDefaultBackendNoService(), ""),
		Entry("ingress with rules no backend service", ingressRulesNoBackend(), ""),
	)

	DescribeTable("should find host when route is defined", func(createCMFunc func() *k8sv1.ConfigMap, route *routev1.Route, hostname, expectedCert string) {
		controller.RouteCache.Add(route)
		controller.RouteConfigMapInformer.GetStore().Add(createCMFunc())
		host, cert := controller.getExternalLinkHostAndCert()
		Expect(host).To(Equal(hostname))
		Expect(cert).To(Equal(expectedCert))
	},
		Entry("route with service and host", createRouteConfigMap, routeToHostAndService(components.VirtExportProxyServiceName), "virt-exportproxy-kubevirt.apps-crc.testing", expectedPem),
		Entry("route with different service and host", createRouteConfigMap, routeToHostAndService("other-service"), "", ""),
		Entry("route with service and no ingress", createRouteConfigMap, routeToHostAndNoIngress(), "", ""),
		Entry("should not find route cert if in future", createFutureRouteConfigMap, routeToHostAndService(components.VirtExportProxyServiceName), "virt-exportproxy-kubevirt.apps-crc.testing", ""),
		Entry("should not find route cert if expired", createExpiredRouteConfigMap, routeToHostAndService(components.VirtExportProxyServiceName), "virt-exportproxy-kubevirt.apps-crc.testing", ""),
		Entry("should find correct route cert if overlapping exists", createOverlappingRouteConfigMap, routeToHostAndService(components.VirtExportProxyServiceName), "virt-exportproxy-kubevirt.apps-crc.testing", expectedPem),
	)

	It("should pick ingress over route if both exist", func() {
		Expect(
			controller.IngressCache.Add(validIngressDefaultBackend(components.VirtExportProxyServiceName)),
		).To(Succeed())
		Expect(controller.RouteCache.Add(routeToHostAndService(components.VirtExportProxyServiceName))).To(Succeed())
		host, _ := controller.getExternalLinkHostAndCert()
		Expect("backend-host").To(Equal(host))
	})
})

func verifyLinksEmpty(vmExport *exportv1.VirtualMachineExport) {
	Expect(vmExport.Status).ToNot(BeNil())
	Expect(vmExport.Status.Links).ToNot(BeNil())
	Expect(vmExport.Status.Links.Internal).To(BeNil())
	Expect(vmExport.Status.Links.External).To(BeNil())
}

func verifyLinksInternal(vmExport *exportv1.VirtualMachineExport, expectedVolumeFormats ...exportv1.VirtualMachineExportVolumeFormat) {
	Expect(vmExport.Status).ToNot(BeNil())
	Expect(vmExport.Status.Links).ToNot(BeNil())
	Expect(vmExport.Status.Links.Internal).NotTo(BeNil())
	Expect(vmExport.Status.Links.Internal.Cert).NotTo(BeEmpty())
	Expect(vmExport.Status.Links.Internal.Volumes).To(HaveLen(len(expectedVolumeFormats) / 2))
	for _, volume := range vmExport.Status.Links.Internal.Volumes {
		Expect(volume.Formats).To(HaveLen(2))
		Expect(expectedVolumeFormats).To(ContainElements(volume.Formats))
	}
}

func verifyLinksExternal(vmExport *exportv1.VirtualMachineExport, link1Format exportv1.ExportVolumeFormat, link1Url string, link2Format exportv1.ExportVolumeFormat, link2Url string) {
	Expect(vmExport.Status.Links.External).ToNot(BeNil())
	Expect(vmExport.Status.Links.External.Cert).To(BeEmpty())
	Expect(vmExport.Status.Links.External.Volumes).To(HaveLen(1))
	Expect(vmExport.Status.Links.External.Volumes[0].Formats).To(HaveLen(2))
	Expect(vmExport.Status.Links.External.Volumes[0].Formats).To(ContainElements(exportv1.VirtualMachineExportVolumeFormat{
		Format: link1Format,
		Url:    link1Url,
	}, exportv1.VirtualMachineExportVolumeFormat{
		Format: link2Format,
		Url:    link2Url,
	}))
}

func verifyKubevirtInternal(vmExport *exportv1.VirtualMachineExport, exportName, namespace string, volumeNames ...string) {
	exportVolumeFormats := make([]exportv1.VirtualMachineExportVolumeFormat, 0)
	for _, volumeName := range volumeNames {
		exportVolumeFormats = append(exportVolumeFormats, exportv1.VirtualMachineExportVolumeFormat{
			Format: exportv1.KubeVirtRaw,
			Url:    fmt.Sprintf("https://%s.%s.svc/volumes/%s/disk.img", fmt.Sprintf("%s-%s", exportPrefix, exportName), namespace, volumeName),
		})
		exportVolumeFormats = append(exportVolumeFormats, exportv1.VirtualMachineExportVolumeFormat{
			Format: exportv1.KubeVirtGz,
			Url:    fmt.Sprintf("https://%s.%s.svc/volumes/%s/disk.img.gz", fmt.Sprintf("%s-%s", exportPrefix, exportName), namespace, volumeName),
		})
	}
	verifyLinksInternal(vmExport, exportVolumeFormats...)
}

func verifyKubevirtExternal(vmExport *exportv1.VirtualMachineExport, exportName, namespace, volumeName string) {
	verifyLinksExternal(vmExport,
		exportv1.KubeVirtRaw,
		fmt.Sprintf("https://virt-exportproxy-kubevirt.apps-crc.testing/api/export.kubevirt.io/v1alpha1/namespaces/%s/virtualmachineexports/%s/volumes/%s/disk.img", namespace, exportName, volumeName),
		exportv1.KubeVirtGz,
		fmt.Sprintf("https://virt-exportproxy-kubevirt.apps-crc.testing/api/export.kubevirt.io/v1alpha1/namespaces/%s/virtualmachineexports/%s/volumes/%s/disk.img.gz", namespace, exportName, volumeName))
}

func verifyArchiveInternal(vmExport *exportv1.VirtualMachineExport, exportName, namespace, volumeName string) {
	verifyLinksInternal(vmExport,
		exportv1.VirtualMachineExportVolumeFormat{
			Format: exportv1.Dir,
			Url:    fmt.Sprintf("https://%s.%s.svc/volumes/%s/dir", fmt.Sprintf("%s-%s", exportPrefix, exportName), namespace, volumeName),
		}, exportv1.VirtualMachineExportVolumeFormat{
			Format: exportv1.ArchiveGz,
			Url:    fmt.Sprintf("https://%s.%s.svc/volumes/%s/disk.tar.gz", fmt.Sprintf("%s-%s", exportPrefix, exportName), namespace, volumeName),
		})
}

func routeToHostAndService(serviceName string) *routev1.Route {
	return &routev1.Route{
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Name: serviceName,
			},
		},
		Status: routev1.RouteStatus{
			Ingress: []routev1.RouteIngress{
				{
					Host: fmt.Sprintf("%s-kubevirt.apps-crc.testing", components.VirtExportProxyServiceName),
				},
			},
		},
	}
}

func routeToHostAndNoIngress() *routev1.Route {
	return &routev1.Route{
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Name: components.VirtExportProxyServiceName,
			},
		},
		Status: routev1.RouteStatus{
			Ingress: []routev1.RouteIngress{},
		},
	}
}

func verifyArchiveExternal(vmExport *exportv1.VirtualMachineExport, exportName, namespace, volumeName string) {
	verifyLinksExternal(vmExport,
		exportv1.Dir,
		fmt.Sprintf("https://virt-exportproxy-kubevirt.apps-crc.testing/api/export.kubevirt.io/v1alpha1/namespaces/%s/virtualmachineexports/%s/volumes/%s/dir", namespace, exportName, volumeName),
		exportv1.ArchiveGz,
		fmt.Sprintf("https://virt-exportproxy-kubevirt.apps-crc.testing/api/export.kubevirt.io/v1alpha1/namespaces/%s/virtualmachineexports/%s/volumes/%s/disk.tar.gz", namespace, exportName, volumeName))
}

func writeCertsToDir(dir string) {
	caKeyPair, _ := triple.NewCA("kubevirt.io", time.Hour*24*7)
	crt := certutil.EncodeCertPEM(caKeyPair.Cert)
	key := certutil.EncodePrivateKeyPEM(caKeyPair.Key)
	Expect(os.WriteFile(filepath.Join(dir, bootstrap.CertBytesValue), crt, 0777)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(dir, bootstrap.KeyBytesValue), key, 0777)).To(Succeed())
}

func createPVCVMExport() *exportv1.VirtualMachineExport {
	return &exportv1.VirtualMachineExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.Now(),
		},
		Spec: exportv1.VirtualMachineExportSpec{
			Source: k8sv1.TypedLocalObjectReference{
				APIGroup: &k8sv1.SchemeGroupVersion.Group,
				Kind:     "PersistentVolumeClaim",
				Name:     testPVCName,
			},
			TokenSecretRef: pointer.StringPtr("token"),
		},
	}
}

func createPVCVMExportWithoutSecret() *exportv1.VirtualMachineExport {
	return &exportv1.VirtualMachineExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-no-secret",
			Namespace: testNamespace,
		},
		Spec: exportv1.VirtualMachineExportSpec{
			Source: k8sv1.TypedLocalObjectReference{
				APIGroup: &k8sv1.SchemeGroupVersion.Group,
				Kind:     "PersistentVolumeClaim",
				Name:     testPVCName,
			},
		},
	}
}

func createSnapshotVMExport() *exportv1.VirtualMachineExport {
	return &exportv1.VirtualMachineExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			Namespace:         testNamespace,
			UID:               "11111-22222-33333",
			CreationTimestamp: metav1.Now(),
		},
		Spec: exportv1.VirtualMachineExportSpec{
			Source: k8sv1.TypedLocalObjectReference{
				APIGroup: &snapshotv1.SchemeGroupVersion.Group,
				Kind:     "VirtualMachineSnapshot",
				Name:     testVmsnapshotName,
			},
			TokenSecretRef: pointer.StringPtr("token"),
		},
	}
}

func createVMVMExport() *exportv1.VirtualMachineExport {
	return &exportv1.VirtualMachineExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			Namespace:         testNamespace,
			UID:               "44444-555555-666666",
			CreationTimestamp: metav1.Now(),
		},
		Spec: exportv1.VirtualMachineExportSpec{
			Source: k8sv1.TypedLocalObjectReference{
				APIGroup: &virtv1.SchemeGroupVersion.Group,
				Kind:     "VirtualMachine",
				Name:     testVmName,
			},
			TokenSecretRef: pointer.StringPtr("token"),
		},
	}
}

func createPVC(name, contentType string) *k8sv1.PersistentVolumeClaim {
	return &k8sv1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Annotations: map[string]string{
				annContentType: contentType,
			},
		},
		Status: k8sv1.PersistentVolumeClaimStatus{
			Phase: k8sv1.ClaimBound,
		},
	}
}

func expectExporterCreate(k8sClient *k8sfake.Clientset, phase k8sv1.PodPhase) {
	k8sClient.Fake.PrependReactor("create", "pods", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
		create, ok := action.(testing.CreateAction)
		Expect(ok).To(BeTrue())
		exportPod, ok := create.GetObject().(*k8sv1.Pod)
		Expect(ok).To(BeTrue())
		exportPod.Status = k8sv1.PodStatus{
			Phase: phase,
		}
		return true, exportPod, nil
	})
}

func expectExporterDelete(k8sClient *k8sfake.Clientset, expectedName string) {
	k8sClient.Fake.PrependReactor("delete", "pods", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
		delete, ok := action.(testing.DeleteAction)
		Expect(ok).To(BeTrue())
		Expect(delete.GetName()).To(Equal(expectedName))
		return true, nil, nil
	})
}

// A mock to implement volumeSnapshotProvider interface
type MockVolumeSnapshotProvider struct {
	volumeSnapshots []*vsv1.VolumeSnapshot
}

func (v *MockVolumeSnapshotProvider) GetVolumeSnapshot(namespace, name string) (*vsv1.VolumeSnapshot, error) {
	if len(v.volumeSnapshots) == 0 {
		return nil, nil
	}
	vs := v.volumeSnapshots[0]
	v.volumeSnapshots = v.volumeSnapshots[1:]
	return vs, nil
}

func (v *MockVolumeSnapshotProvider) Add(s *vsv1.VolumeSnapshot) {
	v.volumeSnapshots = append(v.volumeSnapshots, s)
}
