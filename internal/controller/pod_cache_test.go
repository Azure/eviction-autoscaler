package controllers

import (
	"context"
	"encoding/json"
	"reflect"
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
)

func TestTransformPodCache(t *testing.T) {
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod", Namespace: "workloads", UID: "pod-uid", ResourceVersion: "123",
			Labels:          map[string]string{"app": "demo"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "demo", UID: "owner"}},
			Annotations:     map[string]string{"large": strings.Repeat("annotation", 1024)},
			Finalizers:      []string{"example.com/finalizer"},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "test", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{}}`)},
			}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Containers: []corev1.Container{{
				Name: "app", Image: "demo",
				Env: []corev1.EnvVar{{Name: "LARGE", Value: strings.Repeat("value", 1024)}},
			}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			}}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.0.0.1", HostIP: "192.168.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Image: "demo", ImageID: "image-id", Ready: true, RestartCount: 3,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	before := pod.DeepCopy()
	obj, err := TransformPodCache(pod)
	if err != nil {
		t.Fatal(err)
	}
	compact, ok := obj.(*corev1.Pod)
	if !ok {
		t.Fatalf("unexpected transformed type %T", obj)
	}
	expectedMetadata := metav1.ObjectMeta{
		Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID, ResourceVersion: pod.ResourceVersion,
		Labels: pod.Labels, OwnerReferences: pod.OwnerReferences,
	}
	if compact.TypeMeta != pod.TypeMeta || !reflect.DeepEqual(compact.ObjectMeta, expectedMetadata) {
		t.Fatal("projection did not retain exactly the required metadata")
	}
	if !reflect.DeepEqual(compact.Spec, corev1.PodSpec{NodeName: pod.Spec.NodeName}) {
		t.Fatal("projection retained unused Pod spec fields or lost the node name")
	}
	if !reflect.DeepEqual(compact.Status, corev1.PodStatus{}) {
		t.Fatal("projection retained unused Pod status")
	}
	if !reflect.DeepEqual(pod, before) {
		t.Fatal("transform mutated the incoming Pod")
	}
	fullJSON, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	compactJSON, err := json.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	if len(compactJSON) >= len(fullJSON) {
		t.Fatal("projection did not reduce the representative Pod's serialized size")
	}
}

func TestTransformPodCacheRejectsInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name string
		obj  any
	}{
		{name: "nil"},
		{name: "nil Pod", obj: (*corev1.Pod)(nil)},
		{name: "wrong type", obj: &corev1.Node{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			obj, err := TransformPodCache(test.obj)
			if err == nil || obj != nil {
				t.Fatal("expected an explicit transform error for invalid input")
			}
		})
	}
}

func TestCompactPodReconcileDoesNotReadOrWritePodStatus(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, policyv1.AddToScheme, easv1.AddToScheme,
	} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod", Namespace: "workloads", Labels: map[string]string{"app": "demo"},
			Annotations: map[string]string{"keep": "unchanged"},
		},
		Spec: corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "app", Image: "demo"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	autoscaler := &easv1.EvictionAutoScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: pod.Namespace},
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: autoscaler.Name, Namespace: autoscaler.Namespace},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: pod.Labels}},
	}
	backend := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, node, autoscaler, pdb).
		WithIndex(&corev1.Pod{}, NodeNameIndex, func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).Build()
	var before corev1.Pod
	if err := backend.Get(ctx, client.ObjectKeyFromObject(pod), &before); err != nil {
		t.Fatal(err)
	}
	reconciler := &NodeReconciler{Client: &readOnlyPodClient{Client: backend, t: t}, PodCacheEnabled: true}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != cooldown {
		t.Fatalf("requeue = %v, want %v", result.RequeueAfter, cooldown)
	}
	var storedEAS easv1.EvictionAutoScaler
	if err := backend.Get(ctx, client.ObjectKeyFromObject(autoscaler), &storedEAS); err != nil {
		t.Fatal(err)
	}
	if storedEAS.Spec.LastEviction.PodName != pod.Name || storedEAS.Spec.LastEviction.EvictionTime.IsZero() {
		t.Fatal("expected the EAS LastEviction signal to be recorded")
	}
	var after corev1.Pod
	if err := backend.Get(ctx, client.ObjectKeyFromObject(pod), &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("node reconciliation changed the Pod")
	}
}

type readOnlyPodClient struct {
	client.Client
	t *testing.T
}

func (c *readOnlyPodClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if pods, ok := list.(*corev1.PodList); ok {
		for i := range pods.Items {
			projected, err := TransformPodCache(&pods.Items[i])
			if err != nil {
				return err
			}
			pods.Items[i] = *projected.(*corev1.Pod)
		}
	}
	return nil
}

func (c *readOnlyPodClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		c.t.Fatal("unexpected per-Pod GET")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *readOnlyPodClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		c.t.Fatal("unexpected Pod UPDATE")
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *readOnlyPodClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		c.t.Fatal("unexpected Pod PATCH")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *readOnlyPodClient) Status() client.SubResourceWriter {
	c.t.Fatal("unexpected status write during node reconciliation")
	return nil
}
