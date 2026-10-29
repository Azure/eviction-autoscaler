package controllers

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	easv1 "github.com/azure/eviction-autoscaler/api/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNodeEASUpdateDeduplication(t *testing.T) {
	ctx := context.Background()
	backend := newNodeLookupClient(t, true)
	second := client.ObjectKey{Name: "second", Namespace: "namespace-a"}
	for _, obj := range []client.Object{
		&easv1.EvictionAutoScaler{ObjectMeta: metav1.ObjectMeta{Name: second.Name, Namespace: second.Namespace}},
		&policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: second.Name, Namespace: second.Namespace},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "second"}},
			},
		},
	} {
		if err := backend.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a-3", "a-4"} {
		if err := backend.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: second.Namespace, Labels: map[string]string{"app": "second"}},
			Spec:       corev1.PodSpec{NodeName: "node"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	updates := make(map[client.ObjectKey]int)
	c := interceptor.NewClient(backend, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*corev1.PodList); ok {
				slices.SortFunc(pods.Items, func(a, b corev1.Pod) int {
					return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
				})
			}
			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*easv1.EvictionAutoScaler); ok {
				updates[client.ObjectKeyFromObject(obj)]++
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	reconciler := &NodeReconciler{Client: c}
	keyA := client.ObjectKey{Name: "demo", Namespace: "namespace-a"}
	keyB := client.ObjectKey{Name: "demo", Namespace: "namespace-b"}
	for pass := 1; pass <= 2; pass++ {
		result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "node"}})
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter != cooldown {
			t.Fatalf("requeue=%v, want %v", result.RequeueAfter, cooldown)
		}
		want := map[client.ObjectKey]int{keyA: pass, keyB: pass, second: pass}
		if !reflect.DeepEqual(updates, want) {
			t.Fatalf("pass %d: updates=%v, want %v", pass, updates, want)
		}
		for key, podName := range map[client.ObjectKey]string{keyA: "a-1", keyB: "b-1", second: "a-3"} {
			var autoscaler easv1.EvictionAutoScaler
			if err := backend.Get(ctx, key, &autoscaler); err != nil {
				t.Fatal(err)
			}
			if autoscaler.Spec.LastEviction.PodName != podName ||
				autoscaler.Spec.LastEviction.EvictionTime.IsZero() {
				t.Fatalf("unexpected LastEviction for %v: %+v", key, autoscaler.Spec.LastEviction)
			}
		}
	}
}

func TestNodeEASUpdateFailureRetries(t *testing.T) {
	ctx := context.Background()
	backend := newNodeLookupClient(t, true)
	updateErr := errors.New("EAS update failed")
	failNext := true
	c := interceptor.NewClient(backend, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*easv1.EvictionAutoScaler); ok && failNext {
				failNext = false
				return updateErr
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	reconciler := &NodeReconciler{Client: c}
	request := ctrl.Request{NamespacedName: client.ObjectKey{Name: "node"}}
	if _, err := reconciler.Reconcile(ctx, request); !errors.Is(err, updateErr) {
		t.Fatalf("expected update failure, got %v", err)
	}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != cooldown {
		t.Fatalf("requeue=%v, want %v", result.RequeueAfter, cooldown)
	}
	for _, namespace := range []string{"namespace-a", "namespace-b"} {
		var autoscaler easv1.EvictionAutoScaler
		if err := backend.Get(ctx, client.ObjectKey{Name: "demo", Namespace: namespace}, &autoscaler); err != nil {
			t.Fatal(err)
		}
		if autoscaler.Spec.LastEviction.EvictionTime.IsZero() {
			t.Fatalf("retry did not record eviction for namespace %s", namespace)
		}
	}
}
