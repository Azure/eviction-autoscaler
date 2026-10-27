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

func TestCompactPodCacheCordonDoesNotReadOrWritePodStatus(t *testing.T) {
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
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(compact, node, eas, pdb).
		WithIndex(&corev1.Pod{}, NodeNameIndex, func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).Build()
	r := &NodeReconciler{Client: &noPodStatusClient{Client: cached, t: t}, PodCacheEnabled: true}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)}); err != nil {
		t.Fatal(err)
	}
	var got easv1.EvictionAutoScaler
	if err := cached.Get(ctx, client.ObjectKeyFromObject(eas), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.LastEviction.PodName != pod.Name || got.Spec.LastEviction.EvictionTime.IsZero() {
		t.Fatal("cordon did not record LastEviction")
	}
}

type noPodStatusClient struct {
	client.Client
	t *testing.T
}

func (c *noPodStatusClient) Status() client.SubResourceWriter {
	c.t.Fatal("unexpected status write")
	return nil
}

func (c *noPodStatusClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		c.t.Fatal("unexpected Pod GET")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
