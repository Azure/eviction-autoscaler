package controllers

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestZeroSurgeOverrideForNamespace(t *testing.T) {
	override := intstr.FromString("25%")

	scope := func(ns ...string) map[string]struct{} {
		m := make(map[string]struct{}, len(ns))
		for _, n := range ns {
			m[n] = struct{}{}
		}
		return m
	}

	tests := []struct {
		name      string
		override  *intstr.IntOrString
		namespace string
		scope     map[string]struct{}
		wantSet   bool // whether a non-nil override is expected
	}{
		{name: "feature off returns nil (empty scope)", override: nil, namespace: "a", scope: nil, wantSet: false},
		{name: "feature off returns nil (with scope)", override: nil, namespace: "a", scope: scope("a"), wantSet: false},
		{name: "no scope is fleet-wide", override: &override, namespace: "anything", scope: nil, wantSet: true},
		{name: "empty scope is fleet-wide", override: &override, namespace: "anything", scope: scope(), wantSet: true},
		{name: "in scope applies", override: &override, namespace: "a", scope: scope("a", "b"), wantSet: true},
		{name: "out of scope does not apply", override: &override, namespace: "c", scope: scope("a", "b"), wantSet: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &EvictionAutoScalerReconciler{
				ZeroSurgeOverride:           tc.override,
				ZeroSurgeOverrideNamespaces: tc.scope,
			}
			got := r.zeroSurgeOverrideForNamespace(tc.namespace)
			if tc.wantSet {
				if got == nil {
					t.Fatalf("expected override, got nil")
				}
				if got.String() != override.String() {
					t.Fatalf("expected %q, got %q", override.String(), got.String())
				}
			} else if got != nil {
				t.Fatalf("expected nil, got %q", got.String())
			}
		})
	}
}
