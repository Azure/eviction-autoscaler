package controllers

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TransformPodCache is the OOM mitigation: discard unused Pod data before informer
// storage, not merely from List results. Object count is still cluster-wide.
// No Pod writes remain: the node controller signals evictions on EAS.LastEviction,
// so Pod status and unused spec/metadata need not be retained.
// See docs/pod-cache-memory.md for the cache/read/write flow and rollout limits.
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
		Spec: corev1.PodSpec{NodeName: pod.Spec.NodeName},
	}, nil
}
