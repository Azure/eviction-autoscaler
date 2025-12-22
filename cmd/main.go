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
	"crypto/tls"
	"flag"
	"os"
	"strconv"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appsv1 "github.com/azure/eviction-autoscaler/api/v1"
	controllers "github.com/azure/eviction-autoscaler/internal/controller"
	_ "github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
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

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metric endpoint binds to. "+
		"Use the port :8080. If not set, it will be 0 in order to disable the metrics server")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics servers")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "d482b936.azure.com",
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

	// Parse ENABLED_BY_DEFAULT environment variable
	// Controls default behavior for namespaces not in ACTIONED_NAMESPACES
	// ENABLED_BY_DEFAULT=false (default): namespaces disabled by default, need explicit enable
	// ENABLED_BY_DEFAULT=true: namespaces enabled by default, can explicitly disable
	enabledByDefaultStr := os.Getenv("ENABLED_BY_DEFAULT")
	enabledByDefault := false // default behavior: namespaces disabled by default
	if enabledByDefaultStr != "" {
		var err error
		enabledByDefault, err = strconv.ParseBool(enabledByDefaultStr)
		if err != nil {
			setupLog.Error(err, "Failed to parse ENABLED_BY_DEFAULT env variable")
			os.Exit(1)
		}
	}
	// disabledByDefault parameter: inverse of ENABLED_BY_DEFAULT
	// When ENABLED_BY_DEFAULT=false, disabledByDefault=true (disabled by default)
	// When ENABLED_BY_DEFAULT=true, disabledByDefault=false (enabled by default)
	disabledByDefault := !enabledByDefault

	// Parse ACTIONED_NAMESPACES environment variable (comma-separated list)
	// These namespaces will be enabled when disabledByDefault=true and will be ignored when disabledByDefault=false
	actionedNamespacesStr := os.Getenv("ACTIONED_NAMESPACES")
	actionedNamespacesList := strings.Split(actionedNamespacesStr, ",")
	// Trim whitespace from each namespace
	for i := range actionedNamespacesList {
		actionedNamespacesList[i] = strings.TrimSpace(actionedNamespacesList[i])
	}

	// Create namespace filter
	nsfilter := namespacefilter.New(actionedNamespacesList, disabledByDefault)

	setupLog.Info("Eviction autoscaler configuration",
		"disabledByDefault", disabledByDefault,
		"enabledByDefault", enabledByDefault,
		"actionedNamespaces", actionedNamespacesList)

	// Parse PDB_CREATE environment variable (defaults to false if not set)
	pdbCreateStr := os.Getenv("PDB_CREATE")
	pdbCreate := false
	if pdbCreateStr != "" {
		var err error
		pdbCreate, err = strconv.ParseBool(pdbCreateStr)
		if err != nil {
			setupLog.Error(err, "Failed to parse PDB_CREATE env variable")
			os.Exit(1)
		}
	}
	setupLog.Info("PDB creation configuration", "pdbCreate", pdbCreate)

	if err = (&controllers.EvictionAutoScalerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Filter: nsfilter,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EvictionAutoScaler")
		os.Exit(1)
	}
	setupLog.Info("EvictionAutoScalerReconciler setup completed")

	if pdbCreate {
		if err = (&controllers.DeploymentToPDBReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Filter: nsfilter,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "DeploymentToPDBReconciler")
			os.Exit(1)
		}
		setupLog.Info("DeploymentToPDBReconciler setup completed")
	}

	if err = (&controllers.PDBToEvictionAutoScalerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Filter: nsfilter,
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
