/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodLister is a minimal interface for listing pods, making it easy to mock for testing
type PodLister interface {
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// AnalyzePodAgeFailurePattern analyzes the failure pattern of pods based on their age
func AnalyzePodAgeFailurePattern(ctx context.Context, lister PodLister, pdb *policyv1.PodDisruptionBudget) (string, error) {
	// Convert PDB label selector to Kubernetes selector
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return UnknownPattern, err
	}

	// List all pods matching the PDB selector
	var podList corev1.PodList
	err = lister.List(ctx, &podList, &client.ListOptions{
		Namespace:     pdb.Namespace,
		LabelSelector: selector,
	})
	if err != nil {
		return UnknownPattern, err
	}

	if len(podList.Items) == 0 {
		return AbandonedPDBPattern, nil
	}

	// If only 1 pod and it's being blocked, that means minAvailable=1
	// We can't determine age-based failure pattern with only 1 pod
	if len(podList.Items) == 1 {
		if !IsPodHealthy(&podList.Items[0]) {
			// Single unhealthy pod - can't determine age pattern
			return RandomFailingPattern, nil
		}
		return AllHealthyPattern, nil
	}

	// Sort pods by creation time
	sort.Slice(podList.Items, func(i, j int) bool {
		return podList.Items[i].CreationTimestamp.Before(&podList.Items[j].CreationTimestamp)
	})

	// Categorize pods as unhealthy
	var unhealthyPods []corev1.Pod
	for _, pod := range podList.Items {
		if !IsPodHealthy(&pod) {
			unhealthyPods = append(unhealthyPods, pod)
		}
	}

	// If all pods are healthy
	if len(unhealthyPods) == 0 {
		return AllHealthyPattern, nil
	}

	// Analyze failure pattern (we know we have >= 2 pods at this point)
	totalPods := len(podList.Items)
	totalUnhealthyPods := len(unhealthyPods)

	// Split pods into oldest half and newest half
	oldestHalf := totalPods / 2

	oldestFailureCount := 0
	newestFailureCount := 0

	for i, pod := range podList.Items {
		if !IsPodHealthy(&pod) {
			if i < oldestHalf {
				oldestFailureCount++
			} else {
				newestFailureCount++
			}
		}
	}

	// Follow majority logic
	// If more than 66% of failures are in oldest half -> oldest failing
	// If more than 66% of failures are in newest half -> newest failing
	// Otherwise this is a random failing pattern
	oldestFailureRatio := float64(oldestFailureCount) / float64(totalUnhealthyPods)
	newestFailureRatio := float64(newestFailureCount) / float64(totalUnhealthyPods)

	if oldestFailureRatio >= 0.66 {
		return OldestFailingPattern, nil
	} else if newestFailureRatio >= 0.66 {
		return NewestFailingPattern, nil
	} else {
		return RandomFailingPattern, nil
	}
}

// IsPodHealthy determines if a pod is healthy based on its conditions
func IsPodHealthy(pod *corev1.Pod) bool {
	// Check if pod is running
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	// Check readiness condition
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
