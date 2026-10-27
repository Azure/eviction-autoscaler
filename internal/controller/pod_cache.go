package controllers

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TransformPodCache retains only the fields used to match, locate, and count Pods.
// Retain our condition to preserve transition times and skip unchanged patches.
func TransformPodCache(obj interface{}) (interface{}, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	compact := &corev1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name: pod.Name, Namespace: pod.Namespace,
			UID: pod.UID, ResourceVersion: pod.ResourceVersion,
			Labels: pod.Labels, OwnerReferences: pod.OwnerReferences,
		},
		Spec: corev1.PodSpec{NodeName: pod.Spec.NodeName},
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.DisruptionTarget {
			compact.Status.Conditions = []corev1.PodCondition{condition}
			break
		}
	}
	return compact, nil
}
