package controllers

import (
	"context"
	"reflect"
	"testing"

	easv1 "github.com/azure/eviction-autoscaler/api/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCompactPodCachePreservesStatusOnCordon(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, policyv1.AddToScheme, easv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod", Namespace: "workloads", UID: "original", ResourceVersion: "1",
			Labels:          map[string]string{"app": "demo"},
			Annotations:     map[string]string{"large": "discard"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "demo-rs", UID: "rs"}},
		},
		Spec: corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "app", Image: "demo"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	obj, err := TransformPodCache(pod.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	compact := obj.(*corev1.Pod)
	if len(compact.Spec.Containers) != 0 || len(compact.Annotations) != 0 || !reflect.DeepEqual(compact.Status, corev1.PodStatus{}) {
		t.Fatal("cache retained unused Pod fields")
	}
	if compact.Name != pod.Name || compact.Namespace != pod.Namespace || compact.UID != pod.UID ||
		compact.ResourceVersion != pod.ResourceVersion || compact.Spec.NodeName != pod.Spec.NodeName ||
		!reflect.DeepEqual(compact.Labels, pod.Labels) || !reflect.DeepEqual(compact.OwnerReferences, pod.OwnerReferences) {
		t.Fatal("cache lost fields required for matching, indexing, or deployment discovery")
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: corev1.NodeSpec{Unschedulable: true}}
	eas := &easv1.EvictionAutoScaler{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: pod.Namespace}}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: eas.Name, Namespace: pod.Namespace},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: pod.Labels}},
	}
	api := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).WithObjects(pod).Build()
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(compact, node, eas, pdb).
		WithIndex(&corev1.Pod{}, NodeNameIndex, func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).Build()
	r := &NodeReconciler{Client: cached, APIReader: api, PodCacheEnabled: true}
	// Reads come from the compact cache; status writes go to the API server.
	r.Client = &podStatusClient{Client: cached, api: api}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)}); err != nil {
		t.Fatal(err)
	}
	var got corev1.Pod
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != pod.Status.Phase || got.Status.PodIP != pod.Status.PodIP ||
		len(got.Status.Conditions) != 2 || !reflect.DeepEqual(got.Status.Conditions[0], pod.Status.Conditions[0]) ||
		got.Status.Conditions[1].Type != corev1.DisruptionTarget {
		t.Fatalf("status update did not preserve existing status: %+v", got.Status)
	}
}

type podStatusClient struct {
	client.Client
	api client.Client
}

func (c *podStatusClient) Status() client.SubResourceWriter { return c.api.Status() }
