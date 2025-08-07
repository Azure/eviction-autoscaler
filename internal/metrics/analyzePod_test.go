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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mockPodLister implements PodLister interface for testing
type mockPodLister struct {
	pods []corev1.Pod
	err  error
}

func (m *mockPodLister) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if m.err != nil {
		return m.err
	}
	
	// Cast to PodList and populate with our test data
	if podList, ok := list.(*corev1.PodList); ok {
		podList.Items = m.pods
	}
	
	return nil
}

// Helper function to create a pod with specific status
func createPod(name string, phase corev1.PodPhase, ready bool, creationTime metav1.Time) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: creationTime,
			Labels:            map[string]string{"app": "test"},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
	
	if phase == corev1.PodRunning {
		condition := corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}
		if !ready {
			condition.Status = corev1.ConditionFalse
		}
		pod.Status.Conditions = []corev1.PodCondition{condition}
	}
	
	return pod
}

// Helper function to create a test PDB
func createTestPDB() *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pdb",
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
		},
	}
}

func TestAnalyzePodAgeFailurePattern_EmptyPodList(t *testing.T) {
	ctx := context.Background()
	lister := &mockPodLister{pods: []corev1.Pod{}}
	pdb := createTestPDB()
	
	pattern, err := AnalyzePodAgeFailurePattern(ctx, lister, pdb)
	
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if pattern != AbandonedPDBPattern {
		t.Errorf("Expected %s, got %s", AbandonedPDBPattern, pattern)
	}
}

func TestAnalyzePodAgeFailurePattern_SingleHealthyPod(t *testing.T) {
	ctx := context.Background()
	baseTime := metav1.Now()
	
	pods := []corev1.Pod{
		createPod("pod-1", corev1.PodRunning, true, baseTime),
	}
	
	lister := &mockPodLister{pods: pods}
	pdb := createTestPDB()
	
	pattern, err := AnalyzePodAgeFailurePattern(ctx, lister, pdb)
	
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if pattern != AllHealthyPattern {
		t.Errorf("Expected %s, got %s", AllHealthyPattern, pattern)
	}
}

func TestAnalyzePodAgeFailurePattern_SingleUnhealthyPod(t *testing.T) {
	ctx := context.Background()
	baseTime := metav1.Now()
	
	pods := []corev1.Pod{
		createPod("pod-1", corev1.PodFailed, false, baseTime),
	}
	
	lister := &mockPodLister{pods: pods}
	pdb := createTestPDB()
	
	pattern, err := AnalyzePodAgeFailurePattern(ctx, lister, pdb)
	
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if pattern != RandomFailingPattern {
		t.Errorf("Expected %s, got %s", RandomFailingPattern, pattern)
	}
}

func TestAnalyzePodAgeFailurePattern_AllHealthyPods(t *testing.T) {
	ctx := context.Background()
	baseTime := metav1.Now()
	
	pods := []corev1.Pod{
		createPod("pod-1", corev1.PodRunning, true, baseTime),
		createPod("pod-2", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-5*time.Minute))), // 5 min ago
		createPod("pod-3", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-10*time.Minute))), // 10 min ago
	}
	
	lister := &mockPodLister{pods: pods}
	pdb := createTestPDB()
	
	pattern, err := AnalyzePodAgeFailurePattern(ctx, lister, pdb)
	
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if pattern != AllHealthyPattern {
		t.Errorf("Expected %s, got %s", AllHealthyPattern, pattern)
	}
}

func TestAnalyzePodAgeFailurePattern_OldestFailingPattern(t *testing.T) {
	ctx := context.Background()
	baseTime := metav1.Now()
	
	// Create 6 pods: oldest 3 are unhealthy (100% of oldest half), newest 3 are healthy
	pods := []corev1.Pod{
		// Oldest half (all failing)
		createPod("pod-oldest-1", corev1.PodFailed, false, metav1.NewTime(baseTime.Add(-30*time.Minute))), // 30 min ago
		createPod("pod-oldest-2", corev1.PodFailed, false, metav1.NewTime(baseTime.Add(-25*time.Minute))), // 25 min ago
		createPod("pod-oldest-3", corev1.PodFailed, false, metav1.NewTime(baseTime.Add(-20*time.Minute))), // 20 min ago
		// Newest half (all healthy)
		createPod("pod-newest-1", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-15*time.Minute))), // 15 min ago
		createPod("pod-newest-2", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-10*time.Minute))), // 10 min ago
		createPod("pod-newest-3", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-5*time.Minute))),  // 5 min ago
	}
	
	lister := &mockPodLister{pods: pods}
	pdb := createTestPDB()
	
	pattern, err := AnalyzePodAgeFailurePattern(ctx, lister, pdb)
	
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if pattern != OldestFailingPattern {
		t.Errorf("Expected %s, got %s", OldestFailingPattern, pattern)
	}
}

func TestAnalyzePodAgeFailurePattern_NewestFailingPattern(t *testing.T) {
	ctx := context.Background()
	baseTime := metav1.Now()
	
	// Create 6 pods: oldest 3 are healthy, newest 3 are unhealthy (100% of newest half)
	pods := []corev1.Pod{
		// Oldest half (all healthy)
		createPod("pod-oldest-1", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-30*time.Minute))), // 30 min ago
		createPod("pod-oldest-2", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-25*time.Minute))), // 25 min ago
		createPod("pod-oldest-3", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-20*time.Minute))), // 20 min ago
		// Newest half (all failing)
		createPod("pod-newest-1", corev1.PodFailed, false, metav1.NewTime(baseTime.Add(-15*time.Minute))), // 15 min ago
		createPod("pod-newest-2", corev1.PodFailed, false, metav1.NewTime(baseTime.Add(-10*time.Minute))), // 10 min ago
		createPod("pod-newest-3", corev1.PodFailed, false, metav1.NewTime(baseTime.Add(-5*time.Minute))),  // 5 min ago
	}
	
	lister := &mockPodLister{pods: pods}
	pdb := createTestPDB()
	
	pattern, err := AnalyzePodAgeFailurePattern(ctx, lister, pdb)
	
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if pattern != NewestFailingPattern {
		t.Errorf("Expected %s, got %s", NewestFailingPattern, pattern)
	}
}

func TestAnalyzePodAgeFailurePattern_RandomFailingPattern(t *testing.T) {
	ctx := context.Background()
	baseTime := metav1.Now()
	
	// Create 6 pods with mixed failures (50/50 distribution)
	pods := []corev1.Pod{
		// Oldest half (1 healthy, 1 failing)
		createPod("pod-oldest-1", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-30*time.Minute))), // 30 min ago
		createPod("pod-oldest-2", corev1.PodFailed, false, metav1.NewTime(baseTime.Add(-25*time.Minute))),  // 25 min ago
		createPod("pod-oldest-3", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-20*time.Minute))), // 20 min ago
		// Newest half (1 healthy, 1 failing)
		createPod("pod-newest-1", corev1.PodFailed, false, metav1.NewTime(baseTime.Add(-15*time.Minute))), // 15 min ago
		createPod("pod-newest-2", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-10*time.Minute))), // 10 min ago
		createPod("pod-newest-3", corev1.PodRunning, true, metav1.NewTime(baseTime.Add(-5*time.Minute))),  // 5 min ago
	}
	
	lister := &mockPodLister{pods: pods}
	pdb := createTestPDB()
	
	pattern, err := AnalyzePodAgeFailurePattern(ctx, lister, pdb)
	
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if pattern != RandomFailingPattern {
		t.Errorf("Expected %s, got %s", RandomFailingPattern, pattern)
	}
}

func TestIsPodHealthy(t *testing.T) {
	tests := []struct {
		name     string
		pod      corev1.Pod
		expected bool
	}{
		{
			name: "Running pod with ready condition true",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			expected: true,
		},
		{
			name: "Running pod with ready condition false",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			expected: false,
		},
		{
			name: "Failed pod",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			expected: false,
		},
		{
			name: "Pending pod",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expected: false,
		},
		{
			name: "Running pod without ready condition",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase:      corev1.PodRunning,
					Conditions: []corev1.PodCondition{},
				},
			},
			expected: false,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsPodHealthy(&tt.pod)
			if result != tt.expected {
				t.Errorf("IsPodHealthy() = %v, expected %v", result, tt.expected)
			}
		})
	}
}
