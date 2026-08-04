package controllers

import (
	"testing"

	"github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

func panicCountFor(t *testing.T, namespace, target string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(metrics.PanicCounter); err != nil {
		t.Fatalf("register panic counter: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "eviction_autoscaler_panics_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			var gotNamespace, gotTarget, gotController string
			for _, label := range metric.GetLabel() {
				switch label.GetName() {
				case "namespace":
					gotNamespace = label.GetValue()
				case "target_name":
					gotTarget = label.GetValue()
				case "controller":
					gotController = label.GetValue()
				}
			}
			if gotNamespace == namespace && gotTarget == target && gotController == "evictionautoscaler" {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestRecordPanicCountsWorkloadAndRepanics(t *testing.T) {
	metrics.PanicCounter.Reset()
	target := "nginx"

	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Fatal("expected recordPanic to re-panic so controller-runtime still handles it")
			}
		}()
		defer recordPanic("evictionautoscaler", "team-a", &target)
		panic("boom")
	}()

	if got := panicCountFor(t, "team-a", target); got != 1 {
		t.Fatalf("expected 1 panic recorded for team-a/nginx, got %v", got)
	}
}

func TestRecordPanicIgnoresNormalReturn(t *testing.T) {
	metrics.PanicCounter.Reset()

	func() {
		defer recordPanic("evictionautoscaler", "team-a", nil)
	}()

	if got := panicCountFor(t, "team-a", ""); got != 0 {
		t.Fatalf("expected no panic recorded when nothing panicked, got %v", got)
	}
}
