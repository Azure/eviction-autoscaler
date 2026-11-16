package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	runtimeelection "sigs.k8s.io/controller-runtime/pkg/leaderelection"
)

func TestReconciliationManagerOptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	options := reconciliationManagerOptions(ctx, true)
	if options.LeaderElection || options.Metrics.BindAddress != "0" || options.HealthProbeBindAddress != "0" {
		t.Fatal("term manager must not own election or persistent endpoints")
	}
	if options.GracefulShutdownTimeout == nil || *options.GracefulShutdownTimeout >= 0 {
		t.Fatal("term manager must wait for all workers to stop")
	}
	if options.Controller.SkipNameValidation == nil || !*options.Controller.SkipNameValidation {
		t.Fatal("term manager must allow rebuilding the same controller names")
	}
	cancel()
	if options.BaseContext().Err() != context.Canceled {
		t.Fatal("controller contexts must be canceled when leadership is lost")
	}
}

func defaultElectionOptions(t *testing.T) leaderElectionOptions {
	t.Helper()
	var options leaderElectionOptions
	options.bindFlags(flag.NewFlagSet(t.Name(), flag.ContinueOnError))
	return options
}

func TestLeaderElectionDefaults(t *testing.T) {
	options := defaultElectionOptions(t)
	if options.leaseDuration != 60*time.Second || options.renewDeadline != 40*time.Second ||
		options.retryPeriod != 2*time.Second {
		t.Fatalf("unexpected defaults: %+v", options)
	}
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	lock, err := runtimeelection.NewResourceLock(&rest.Config{Host: server.URL}, testRecorder{}, runtimeelection.Options{
		LeaderElection: true, LeaderElectionID: leaderElectionID, LeaderElectionNamespace: "kube-system",
		RenewDeadline: options.renewDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := lock.Get(ctx); err == nil {
		t.Fatal("expected unavailable API server error")
	}
	request := <-requests
	if got := request.URL.Query().Get("timeout"); got != "20s" {
		t.Fatalf("election request timeout=%q, want 20s", got)
	}
}

func TestLeaderElectionAllowsRequestsSlowerThanFiveSeconds(t *testing.T) {
	const delay = 6 * time.Second
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("timeout"); got != "20s" {
			t.Errorf("election request timeout=%q, want 20s", got)
		}
		if r.Method == http.MethodPut {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&coordinationv1.Lease{
			TypeMeta:   metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
			ObjectMeta: metav1.ObjectMeta{Name: leaderElectionID, Namespace: "kube-system"},
		}); err != nil {
			t.Errorf("write lease response: %v", err)
		}
	}))
	defer server.Close()
	options := defaultElectionOptions(t)
	lock, err := runtimeelection.NewResourceLock(&rest.Config{Host: server.URL}, testRecorder{}, runtimeelection.Options{
		LeaderElection: true, LeaderElectionID: leaderElectionID, LeaderElectionNamespace: "kube-system",
		RenewDeadline: options.renewDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, _, err := lock.Get(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := lock.Update(ctx, resourcelock.LeaderElectionRecord{
		HolderIdentity: lock.Identity(), LeaseDurationSeconds: 60, RenewTime: metav1.Now(),
	}); err != nil {
		t.Fatalf("default election client rejected a six-second renewal: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("did not exercise a slow renewal: %s", elapsed)
	}
}

type testRecorder struct{}

func (testRecorder) GetEventRecorderFor(string) record.EventRecorder {
	return record.NewFakeRecorder(10)
}

type testLeaseLock struct {
	mu      sync.Mutex
	record  *resourcelock.LeaderElectionRecord
	blocked atomic.Bool
}

func (l *testLeaseLock) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	if l.blocked.Load() {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.record == nil {
		return nil, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "leases"}, leaderElectionID)
	}
	copy := *l.record
	raw, err := json.Marshal(copy)
	return &copy, raw, err
}

func (l *testLeaseLock) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	return l.Update(ctx, record)
}

func (l *testLeaseLock) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	if l.blocked.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record = &record
	return nil
}

func (*testLeaseLock) RecordEvent(string) {}
func (*testLeaseLock) Identity() string   { return "test-controller" }
func (*testLeaseLock) Describe() string   { return "test/lease" }

func testSupervisor(t *testing.T, run func(context.Context) error) (*leadershipSupervisor, *testLeaseLock) {
	t.Helper()
	lock := &testLeaseLock{}
	supervisor, err := newLeadershipSupervisor(lock, leaderElectionOptions{
		leaseDuration: time.Second, renewDeadline: 60 * time.Millisecond, retryPeriod: 5 * time.Millisecond,
	}, run)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.shutdownTimeout = time.Second
	return supervisor, lock
}

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
	}
}

func awaitSupervisor(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for supervisor")
		return nil
	}
}

func TestLeadershipRecoveryWaitsForOldWorkers(t *testing.T) {
	started := make(chan struct{}, 2)
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var terms, workers atomic.Int32
	supervisor, lock := testSupervisor(t, func(ctx context.Context) error {
		if workers.Add(1) != 1 {
			t.Error("leadership terms overlapped")
		}
		defer workers.Add(-1)
		term := terms.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		if term == 1 {
			close(canceled)
			<-release
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Start(ctx) }()
	defer func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
	}()
	awaitSignal(t, started)
	if err := supervisor.Check(nil); err != nil {
		t.Fatal(err)
	}
	lock.blocked.Store(true)
	awaitSignal(t, canceled)
	// Cancellation precedes the synchronous stopped-leading callback.
	deadline := time.Now().Add(time.Second)
	for supervisor.Check(nil) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if supervisor.Check(nil) == nil || testutil.ToFloat64(metrics.LeaderActive) != 0 {
		t.Fatal("paused controller still reports active leadership")
	}
	lock.blocked.Store(false)
	select {
	case <-started:
		t.Fatal("rejoined election before old workers stopped")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	awaitSignal(t, started)
	if terms.Load() != 2 {
		t.Fatalf("expected a fresh leadership term, got %d", terms.Load())
	}
	cancel()
	if err := awaitSupervisor(t, done); err != nil {
		t.Fatal(err)
	}
	if workers.Load() != 0 || supervisor.Check(nil) == nil {
		t.Fatal("shutdown left reconciliation active")
	}
}

func TestLeadershipShutdownTimeoutIsFatal(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	stopped := make(chan struct{})
	var terms atomic.Int32
	supervisor, lock := testSupervisor(t, func(ctx context.Context) error {
		terms.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		<-release
		close(stopped)
		return nil
	})
	supervisor.shutdownTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		close(release)
		awaitSignal(t, stopped)
	}()
	done := make(chan error, 1)
	go func() { done <- supervisor.Start(ctx) }()
	awaitSignal(t, started)
	lock.blocked.Store(true)
	err := awaitSupervisor(t, done)
	if err == nil || !strings.Contains(err.Error(), "refusing to rejoin election") {
		t.Fatalf("expected fatal shutdown timeout, got %v", err)
	}
	if terms.Load() != 1 {
		t.Fatal("started another term after incomplete shutdown")
	}
}

func TestLeadershipManagerFailuresRemainFatal(t *testing.T) {
	managerErr := errors.New("manager failed")
	for _, test := range []struct {
		name string
		err  error
	}{
		{"runtime error", managerErr},
		{"unexpected clean return", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var terms atomic.Int32
			supervisor, _ := testSupervisor(t, func(context.Context) error {
				terms.Add(1)
				return test.err
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := supervisor.Start(ctx)
			if err == nil || (test.err != nil && !errors.Is(err, test.err)) {
				t.Fatalf("expected fatal manager error, got %v", err)
			}
			if terms.Load() != 1 {
				t.Fatal("retried a fatal manager error")
			}
		})
	}
}

func TestLeadershipShutdownBeforeAcquisition(t *testing.T) {
	supervisor, lock := testSupervisor(t, func(context.Context) error {
		t.Error("started reconciliation without acquiring leadership")
		return nil
	})
	lock.blocked.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Start(ctx) }()
	cancel()
	if err := awaitSupervisor(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidLeaderElectionOptions(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*leaderElectionOptions)
	}{
		{"lease below deadline", func(o *leaderElectionOptions) { o.leaseDuration = o.renewDeadline }},
		{"deadline below retry", func(o *leaderElectionOptions) { o.renewDeadline = o.retryPeriod }},
		{"negative retry", func(o *leaderElectionOptions) { o.retryPeriod = -time.Second }},
		{"zero lease", func(o *leaderElectionOptions) { o.leaseDuration = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := defaultElectionOptions(t)
			test.change(&options)
			if _, err := newLeadershipSupervisor(&testLeaseLock{}, options, nil); err == nil {
				t.Fatal("accepted invalid election settings")
			}
		})
	}
}
