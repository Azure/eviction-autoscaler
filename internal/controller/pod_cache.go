package controllers

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TransformPodCache drops unused spec and metadata while retaining complete status.
// Projected Pods require status patches: Update can clear discarded metadata.
func TransformPodCache(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod == nil {
		return nil, fmt.Errorf("Pod cache transform expected a non-nil *v1.Pod, got %T", obj)
	}
	return &corev1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			UID:             pod.UID,
			ResourceVersion: pod.ResourceVersion,
			Labels:          pod.Labels,
			OwnerReferences: pod.OwnerReferences,
		},
		Spec:   corev1.PodSpec{NodeName: pod.Spec.NodeName},
		Status: pod.Status,
	}, nil
}
