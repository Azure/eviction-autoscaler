package controllers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	if !reflect.DeepEqual(compact.Status, pod.Status) {
		t.Fatal("projection did not retain the complete Pod status")
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
