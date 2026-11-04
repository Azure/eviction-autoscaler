package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azure/eviction-autoscaler/internal/metrics"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	leaderElectionID      = "d482b936.azure.com"
	leaderShutdownTimeout = 10 * time.Second
)

type leaderElectionOptions struct {
	leaseDuration time.Duration
	renewDeadline time.Duration
	retryPeriod   time.Duration
}

func (o *leaderElectionOptions) bindFlags(flags *flag.FlagSet) {
	flags.DurationVar(&o.leaseDuration, "leader-election-lease-duration", 60*time.Second,
		"Duration candidates wait before taking over a leader's lease.")
	flags.DurationVar(&o.renewDeadline, "leader-election-renew-deadline", 40*time.Second,
		"Time allowed to renew leadership; election requests time out after half this duration.")
	flags.DurationVar(&o.retryPeriod, "leader-election-retry-period", 2*time.Second,
		"Interval between election attempts.")
}

type leadershipSupervisor struct {
	election        leaderelection.LeaderElectionConfig
	run             func(context.Context) error
	shutdownTimeout time.Duration
	active          atomic.Bool
}

func newLeadershipSupervisor(
	lock resourcelock.Interface, options leaderElectionOptions, run func(context.Context) error,
) (*leadershipSupervisor, error) {
	election := leaderelection.LeaderElectionConfig{
		Lock:          lock,
		Name:          leaderElectionID,
		LeaseDuration: options.leaseDuration,
		RenewDeadline: options.renewDeadline,
		RetryPeriod:   options.retryPeriod,
		// Never release a lease before reconciliation has stopped.
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {},
			OnStoppedLeading: func() {},
		},
	}
	if _, err := leaderelection.NewLeaderElector(election); err != nil {
		return nil, fmt.Errorf("invalid leader election configuration: %w", err)
	}
	return &leadershipSupervisor{
		election: election, run: run, shutdownTimeout: leaderShutdownTimeout,
	}, nil
}

func (s *leadershipSupervisor) NeedLeaderElection() bool { return false }

func (s *leadershipSupervisor) Check(_ *http.Request) error {
	if !s.active.Load() {
		return errors.New("reconciliation paused: waiting for leadership")
	}
	return nil
}

func (s *leadershipSupervisor) setActive(active bool) {
	s.active.Store(active)
	if active {
		metrics.LeaderActive.Set(1)
	} else {
		metrics.LeaderActive.Set(0)
	}
}

func (s *leadershipSupervisor) Start(ctx context.Context) error {
	defer s.setActive(false)
	for ctx.Err() == nil {
		if err := s.runElection(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		ctrl.LoggerFrom(ctx).Info("Reconciliation stopped; retrying leader election")
		timer := time.NewTimer(wait.Jitter(s.election.RetryPeriod, 0.2))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func (s *leadershipSupervisor) runElection(ctx context.Context) error {
	electionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	var mu sync.Mutex
	var started, stopped bool
	election := s.election
	election.Callbacks = leaderelection.LeaderCallbacks{
		OnStartedLeading: func(leaderCtx context.Context) {
			// The callback runs asynchronously and may be scheduled after lease loss.
			mu.Lock()
			if stopped {
				mu.Unlock()
				return
			}
			started = true
			if leaderCtx.Err() != nil {
				mu.Unlock()
				done <- nil
				return
			}
			s.setActive(true)
			mu.Unlock()
			err := s.run(leaderCtx)
			if leaderCtx.Err() == nil && err == nil {
				err = errors.New("reconciliation manager stopped while still leader")
			}
			if leaderCtx.Err() != nil && errors.Is(err, context.Canceled) {
				err = nil
			}
			s.setActive(false)
			done <- err
			cancel()
		},
		OnStoppedLeading: func() {
			mu.Lock()
			defer mu.Unlock()
			stopped = true
			s.setActive(false)
			if !started {
				done <- nil
			} else if ctx.Err() == nil {
				ctrl.LoggerFrom(ctx).Info("Leadership lost; pausing reconciliation")
			}
		},
	}
	elector, err := leaderelection.NewLeaderElector(election)
	if err != nil {
		return fmt.Errorf("create leader elector: %w", err)
	}
	elector.Run(electionCtx)
	timer := time.NewTimer(s.shutdownTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("reconciliation did not stop within %s; refusing to rejoin election", s.shutdownTimeout)
	}
}
