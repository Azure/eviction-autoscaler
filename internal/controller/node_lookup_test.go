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
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newNodeLookupClient(t *testing.T, withAutoscalers bool) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, policyv1.AddToScheme, easv1.AddToScheme,
	} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := []client.Object{&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}}
	for _, pod := range []struct{ name, namespace string }{
		{"a-1", "namespace-a"}, {"a-2", "namespace-a"}, {"b-1", "namespace-b"},
	} {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: pod.name, Namespace: pod.namespace, Labels: map[string]string{"app": "demo"},
			},
			Spec: corev1.PodSpec{NodeName: "node"},
		})
	}
	if withAutoscalers {
		for _, namespace := range []string{"namespace-a", "namespace-b"} {
			objects = append(objects,
				&easv1.EvictionAutoScaler{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: namespace}},
				&policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: namespace},
					Spec: policyv1.PodDisruptionBudgetSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo"}},
					},
				},
			)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithIndex(&corev1.Pod{}, NodeNameIndex, func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).Build()
}

func TestNodeEASNamespaceLookup(t *testing.T) {
	keyA := client.ObjectKey{Name: "demo", Namespace: "namespace-a"}
	keyB := client.ObjectKey{Name: "demo", Namespace: "namespace-b"}
	for _, test := range []struct {
		name        string
		autoscalers bool
		failNS      string
		wantLists   map[string]int
		wantUpdates map[client.ObjectKey]int
	}{
		{
			name: "reuse lists and refreshed versions", autoscalers: true,
			wantLists:   map[string]int{"namespace-a": 1, "namespace-b": 1},
			wantUpdates: map[client.ObjectKey]int{keyA: 2, keyB: 1},
		},
		{
			name:        "remember empty results",
			wantLists:   map[string]int{"namespace-a": 1, "namespace-b": 1},
			wantUpdates: map[client.ObjectKey]int{},
		},
		{
			name: "first namespace error", autoscalers: true, failNS: "namespace-a",
			wantLists: map[string]int{"namespace-a": 1}, wantUpdates: map[client.ObjectKey]int{},
		},
		{
			name: "later namespace error", autoscalers: true, failNS: "namespace-b",
			wantLists:   map[string]int{"namespace-a": 1, "namespace-b": 1},
			wantUpdates: map[client.ObjectKey]int{keyA: 2},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			backend := newNodeLookupClient(t, test.autoscalers)
			listCalls := make(map[string]int)
			updates := make(map[client.ObjectKey]int)
			versions := make(map[client.ObjectKey][]string)
			podLists := 0
			listErr := errors.New("EAS list failed")
			c := interceptor.NewClient(backend, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*easv1.EvictionAutoScalerList); ok {
						options := &client.ListOptions{}
						for _, opt := range opts {
							opt.ApplyToList(options)
						}
						listCalls[options.Namespace]++
						if options.Namespace == "namespace-b" && test.autoscalers && updates[keyA] == 0 {
							t.Fatal("loaded another namespace before processing the first")
						}
						if options.Namespace == test.failNS {
							return listErr
						}
					}
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if pods, ok := list.(*corev1.PodList); ok {
						podLists++
						slices.SortFunc(pods.Items, func(a, b corev1.Pod) int {
							return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
						})
					}
					return nil
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*easv1.EvictionAutoScaler); ok {
						key := client.ObjectKeyFromObject(obj)
						updates[key]++
						versions[key] = append(versions[key], obj.GetResourceVersion())
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			reconciler := &NodeReconciler{Client: c}
			result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "node"}})
			if test.failNS != "" {
				if !errors.Is(err, listErr) {
					t.Fatalf("expected list failure, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if podLists != 1 || !reflect.DeepEqual(listCalls, test.wantLists) ||
				!reflect.DeepEqual(updates, test.wantUpdates) {
				t.Fatalf("Pod lists=%d, EAS lists=%v, updates=%v; want lists=%v updates=%v",
					podLists, listCalls, updates, test.wantLists, test.wantUpdates)
			}
			if len(versions[keyA]) == 2 && versions[keyA][0] == versions[keyA][1] {
				t.Fatal("reused a stale EAS resource version")
			}
			if test.failNS == "" {
				wantRequeue := cooldown
				if !test.autoscalers {
					wantRequeue = 0
				}
				if result.RequeueAfter != wantRequeue {
					t.Fatalf("requeue=%v, want %v", result.RequeueAfter, wantRequeue)
				}
			}
		})
	}
}
