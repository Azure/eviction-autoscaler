package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	easv1 "github.com/azure/eviction-autoscaler/api/v1"
	controllers "github.com/azure/eviction-autoscaler/internal/controller"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestLeadershipRecoveryRebuildsManagers(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: filepath.Join("..", "bin", "k8s",
			fmt.Sprintf("1.32.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Error(err)
		}
	})
	api, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	host, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 2)
	paused := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	var terms atomic.Int32
	supervisor, lock := testSupervisor(t, func(ctx context.Context) error {
		term := terms.Add(1)
		mgr, err := ctrl.NewManager(cfg, reconciliationManagerOptions(ctx, true))
		if err != nil {
			return err
		}
		node := &controllers.NodeReconciler{Client: mgr.GetClient(), Scheme: scheme, PodCacheEnabled: true}
		if err := node.SetupWithManager(mgr); err != nil {
			return err
		}
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return nil
		})); err != nil {
			return err
		}
		err = mgr.Start(ctx)
		if term == 1 {
			close(paused)
			<-resume
		}
		return err
	})
	supervisor.shutdownTimeout = 15 * time.Second
	if err := host.Add(supervisor); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/healthz", &healthz.Handler{Checks: map[string]healthz.Checker{"healthz": healthz.Ping}})
	mux.Handle("/readyz", &healthz.Handler{Checks: map[string]healthz.Checker{"readyz": supervisor.Check}})
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	if err := host.Add(&manager.Server{
		Listener: listener, Server: &http.Server{Handler: mux, ReadHeaderTimeout: time.Second},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		resumeOnce.Do(func() { close(resume) })
		if err := awaitSupervisor(t, done); err != nil {
			t.Error(err)
		}
	})
	awaitSignal(t, started)
	lock.blocked.Store(true)
	awaitSignal(t, paused)
	checkPausedEndpoints(t, "http://"+listener.Addr().String())

	for _, obj := range []client.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "paused-node"},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "created-while-paused", Namespace: "default", Labels: map[string]string{"app": "demo"},
			},
			Spec: corev1.PodSpec{
				NodeName: "paused-node", Containers: []corev1.Container{{Name: "app", Image: "demo"}},
			},
		},
		&easv1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
			Spec:       easv1.EvictionAutoScalerSpec{TargetName: "demo", TargetKind: "deployment"},
		},
		&policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo"}},
			},
		},
	} {
		if err := api.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	lock.blocked.Store(false)
	resumeOnce.Do(func() { close(resume) })
	awaitSignal(t, started)
	err = wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			var autoscaler easv1.EvictionAutoScaler
			if err := api.Get(ctx, client.ObjectKey{Name: "demo", Namespace: "default"}, &autoscaler); err != nil {
				return false, err
			}
			return autoscaler.Spec.LastEviction.PodName == "created-while-paused", nil
		})
	if err != nil {
		t.Fatalf("new informer did not rediscover resources created while paused: %v", err)
	}
	if terms.Load() != 2 {
		t.Fatalf("expected exactly two manager instances, got %d", terms.Load())
	}
}

func checkPausedEndpoints(t *testing.T, baseURL string) {
	t.Helper()
	for _, endpoint := range []struct {
		path   string
		status int
	}{
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusInternalServerError},
		{"/metrics", http.StatusOK},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+endpoint.path, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		cancel()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read endpoint: %v; close: %v", readErr, closeErr)
		}
		if response.StatusCode != endpoint.status {
			t.Fatalf("%s returned %d, want %d: %s", endpoint.path, response.StatusCode, endpoint.status, body)
		}
		if endpoint.path == "/metrics" && !strings.Contains(string(body), "eviction_autoscaler_leader_active 0") {
			t.Fatalf("metrics do not report paused leadership: %s", body)
		}
	}
}
