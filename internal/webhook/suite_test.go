/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	appsv1 "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
	controllers "github.com/paulgmiller/k8s-pdb-autoscaler/internal/controller"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var ctx context.Context
var cancel context.CancelFunc

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("bootstrapping test environment")
	// Load Webhook Configuration YAML
	//yamlPath := filepath.Join(filepath.Join("..", "..", "config", "webhook", "manifests", "webhook_configuration.yaml"))
	//yamlData, err := os.ReadFile(yamlPath)
	//Expect(err).NotTo(HaveOccurred())
	yamlData := []byte(`
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: eviction-webhook
webhooks:
  - name: eviction.mydomain.com
    clientConfig:
      service:
        name: eviction-webhook
        namespace: default
        path: /validate-eviction
      caBundle: ""
    rules:
      - operations: ["CREATE"]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["pods/eviction"]
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
`)

	// Decode YAML into a WebhookConfiguration object
	webhookConfig := &admissionv1.ValidatingWebhookConfiguration{}
	err := yaml.Unmarshal(yamlData, webhookConfig)
	Expect(err).NotTo(HaveOccurred(), string(yamlData))
	Expect(webhookConfig.Name).To(Equal("eviction-webhook"), string(yamlData))
	Expect(webhookConfig.Webhooks).To(HaveLen(1))
	Expect(webhookConfig.Webhooks[0].Name).To(Equal("eviction.mydomain.com"))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,

		// The BinaryAssetsDirectory is only required if you want to run the tests directly
		// without call the makefile target test. If not informed it will look for the
		// default path defined in controller-runtime which is /usr/local/kubebuilder/.
		// Note that you must have the required binaries setup under the bin directory to perform
		// the tests directly. When we run make test it will be setup and used automatically.
		BinaryAssetsDirectory: filepath.Join("..", "..", "bin", "k8s",
			fmt.Sprintf("1.30.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			ValidatingWebhooks: []*admissionv1.ValidatingWebhookConfiguration{
				webhookConfig,
			},
		},
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = appsv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())
	err = policyv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	Expect(err).NotTo(HaveOccurred())

	// Add your controller
	err = (&controllers.PDBWatcherReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:builder

	// Configure the webhook server
	hookServer := webhook.NewServer(webhook.Options{
		Port:    testEnv.WebhookInstallOptions.LocalServingPort,
		CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		//TLSOpts: tlsOpts,
	})

	// Register the webhook handler
	hookServer.Register("/validate-eviction", &admission.Webhook{
		Handler: &EvictionHandler{
			Client: mgr.GetClient(),
		},
	})

	// Add the webhook server to the manager
	if err := mgr.Add(hookServer); err != nil {
		log.Printf("Unable to add webhook server to manager: %v", err)
		os.Exit(1)
	}
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		mgr.Start(ctx)

	}()

	// Wait for the cache to sync
	synced := mgr.GetCache().WaitForCacheSync(ctx)
	Expect(synced).Should(BeTrue())

})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
