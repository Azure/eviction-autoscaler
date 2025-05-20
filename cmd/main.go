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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	appsv1 "github.com/azure/eviction-autoscaler/api/v1"
	controllers "github.com/azure/eviction-autoscaler/internal/controller"
	evictinwebhook "github.com/azure/eviction-autoscaler/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(appsv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var evictionWebhook bool
	var installCRDs bool
	var crdPath string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metric endpoint binds to. "+
		"Use the port :8080. If not set, it will be 0 in order to disable the metrics server")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&evictionWebhook, "eviction-webhook", false,
		"create a webhook that intercepts evictions and updates the EvictionAutoScaler, "+
			"if false will rely on node cordon for signal")
	flag.BoolVar(&installCRDs, "install-crds", false,
		"If set, the controller will install its CRDs at startup")
	flag.StringVar(&crdPath, "crd-path", "config/crd/bases", 
		"Path to directory containing CRD files to install (only used if --install-crds=true)")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// If installCRDs is enabled, apply the CRDs before starting the manager
	if installCRDs {
		if err := installCRDsFromDir(crdPath); err != nil {
			setupLog.Error(err, "unable to install CRDs")
			os.Exit(1)
		}
		setupLog.Info("Successfully installed CRDs")
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Configure the webhook server
	hookServer := webhook.NewServer(webhook.Options{
		Port:    9443,
		CertDir: "/etc/webhook/tls",
		TLSOpts: tlsOpts,
	})
	shutdown := time.Duration(-1) //wait until pod termination grace period sends sig kill or webhook shuts down
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		},
		//WebhookServer:           hookServer,
		GracefulShutdownTimeout: &shutdown,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "d482b936.azure.com",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.EvictionAutoScalerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EvictionAutoScaler")
		os.Exit(1)
	}
	setupLog.Info("EvictionAutoScalerReconciler  setup completed")

	if err = (&controllers.DeploymentToPDBReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DeploymentToPDBReconciler")
		os.Exit(1)
	}
	setupLog.Info("DeploymentToPDBReconciler  setup completed")

	if err = (&controllers.PDBToEvictionAutoScalerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PDBToEvictionAutoScalerReconciler")
		os.Exit(1)
	}
	setupLog.Info("PDBToEvictionAutoScalerReconciler  setup completed")

	if err = (&controllers.NodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EvictionAutoScaler")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Register the webhook handler
	if evictionWebhook {
		hookServer.Register("/validate-eviction", &admission.Webhook{
			Handler: &evictinwebhook.EvictionHandler{
				Client: mgr.GetClient(),
			},
		})
		// Add the webhook server to the manager
		if err := mgr.Add(hookServer); err != nil {
			log.Printf("Unable to add webhook server to manager: %v", err)
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=create;get;list;update;patch

// installCRDsFromDir installs all CRD files from the specified directory
func installCRDsFromDir(crdDir string) error {
	config := ctrl.GetConfigOrDie()
	extClient, err := clientset.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Find CRD files in the directory
	crdFiles, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		return fmt.Errorf("failed to find CRD files: %w", err)
	}

	if len(crdFiles) == 0 {
		return fmt.Errorf("no CRD files found in %s", crdDir)
	}

	// Install each CRD file found
	for _, crdFile := range crdFiles {
		setupLog.Info("Installing CRD", "file", crdFile)
		
		// Read CRD file
		crdBytes, err := os.ReadFile(crdFile)
		if err != nil {
			return fmt.Errorf("failed to read CRD file %s: %w", crdFile, err)
		}

		// Decode YAML to CustomResourceDefinition
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(crdBytes, crd); err != nil {
			return fmt.Errorf("failed to unmarshal CRD from file %s: %w", crdFile, err)
		}

		// Create or update CRD
		_, err = extClient.ApiextensionsV1().CustomResourceDefinitions().Create(
			ctrl.LoggerInto(context.Background(), setupLog.WithValues("crd", crd.Name)),
			crd, 
			metav1.CreateOptions{},
		)
		if err != nil {
			if errors.IsAlreadyExists(err) {
				setupLog.Info("CRD already exists, updating", "name", crd.Name)
				
				// If CRD exists, update it
				existing, err := extClient.ApiextensionsV1().CustomResourceDefinitions().Get(
					ctrl.LoggerInto(context.Background(), setupLog.WithValues("crd", crd.Name)),
					crd.Name, 
					metav1.GetOptions{},
				)
				if err != nil {
					return fmt.Errorf("failed to get existing CRD %s: %w", crd.Name, err)
				}

				// Update resource version to ensure proper update
				crd.ResourceVersion = existing.ResourceVersion
				
				_, err = extClient.ApiextensionsV1().CustomResourceDefinitions().Update(
					ctrl.LoggerInto(context.Background(), setupLog.WithValues("crd", crd.Name)),
					crd, 
					metav1.UpdateOptions{},
				)
				if err != nil {
					return fmt.Errorf("failed to update CRD %s: %w", crd.Name, err)
				}
			} else {
				return fmt.Errorf("failed to create CRD %s: %w", crd.Name, err)
			}
		}
		
		setupLog.Info("Successfully installed CRD", "name", crd.Name)
	}
	
	return nil
}
