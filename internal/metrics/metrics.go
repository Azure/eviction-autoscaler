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

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// DeploymentGauge tracks the number of deployments seen by the controller
	// Labels: namespace, can_create_pdb (true/false)
	DeploymentGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_deployments_total",
			Help: "Total number of deployments seen by the eviction autoscaler",
		},
		[]string{"namespace", "can_create_pdb"},
	)

	// PDBGauge tracks the number of PDBs seen by the controller
	// Labels: namespace, created_by_us (true/false)
	PDBGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_pdbs_total",
			Help: "Total number of PDBs seen by the eviction autoscaler",
		},
		[]string{"namespace", "created_by_us", "max_unavailable_zero", "min_available_equals_replicas"},
	)

	// EvictionCounter tracks how often the eviction-autoscaler notices an eviction
	// Labels: namespace
	EvictionCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_evictions_total",
			Help: "Total number of evictions noticed by the eviction autoscaler",
		},
		[]string{"namespace"},
	)

	// BlockedEvictionCounter tracks how often evictions are blocked by PDBs
	// Labels: namespace, pdb_name
	BlockedEvictionCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_blocked_evictions_total",
			Help: "Total number of evictions blocked by PDBs",
		},
		[]string{"namespace", "pdb_name"},
	)

	// ScalingOpportunityCounter tracks how often the controller thinks it could have scaled a deployment
	// Labels: namespace, deployment_name, action (scale_up/scale_down), signal
	ScalingOpportunityCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_scaling_opportunities_total",
			Help: "Total number of times the controller identified scaling opportunities",
		},
		[]string{"namespace", "deployment_name", "action", "signal"},
	)

	// ActualScalingCounter tracks actual scaling actions performed
	// Labels: namespace, deployment_name, action (scale_up/scale_down)
	ActualScalingCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_scaling_actions_total",
			Help: "Total number of actual scaling actions performed by the controller",
		},
		[]string{"namespace", "deployment_name", "action"},
	)

	// PDBCreationCounter tracks PDB creation events
	// Labels: namespace, deployment_name
	PDBCreationCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_pdb_creations_total",
			Help: "Total number of PDBs created by the eviction autoscaler",
		},
		[]string{"namespace", "deployment_name"},
	)

	// EvictionAutoScalerCreationCounter tracks EvictionAutoScaler creation events
	// Labels: namespace, pdb_name, target_deployment
	EvictionAutoScalerCreationCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_creation_total",
			Help: "Total number of EvictionAutoScaler resources created",
		},
		[]string{"namespace", "pdb_name", "target_deployment"},
	)

	// NodeCordoningCounter tracks node cordoning events detected
	NodeCordoningCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_node_cordoning_total",
			Help: "Total number of node cordoning events detected by the eviction autoscaler",
		},
	)

	// PDBInfoGauge tracks various PDB-related metrics
	// Labels: namespace, pdb_name, target_name, metric_type
	// todo:chnage with PDBGauge instead of separate gauges per PDB
	// use labels on PDBGauge to count how many have maxUnavailable==0 and minAvailable==replicas
	PDBInfoGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_pdb_info",
			Help: "PDB configuration and status information",
		},
		[]string{"namespace", "pdb_name", "target_name", "metric_type"},
	)

	// PDBCounter tracks the number of PDBs with an increment interface
	// Labels: namespace, created_by_us (true/false)
	PDBCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_pdb_count_total",
			Help: "Total count of PDBs processed by the eviction autoscaler",
		},
		[]string{"namespace", "created_by_us"},
	)

	// PodAgeFailurePatternGauge tracks failure patterns by pod age
	// Labels: namespace, pdb_name, pattern (oldest_failing/newest_failing/random_failing/all_healthy)
	PodAgeFailurePatternGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_pod_age_failure_pattern",
			Help: "Current failure pattern based on pod age (1 = active pattern, 0 = not active)",
		},
		[]string{"namespace", "pdb_name", "pattern"},
	)
)

// Constants for PDB creation tracking
const (
	PDBCreatedByUsStr    = "true"
	PDBNotCreatedByUsStr = "false"
)

// Constants for deployment tracking
const (
	CanCreatePDBStr    = "true"
	CannotCreatePDBStr = "false"
)

// Constants for PDB info metric types
// todo: Remove when PDBInfoGauge is replaced with PDBGauge labels
const (
	MaxUnavailableMetric             = "max_unavailable"
	MinAvailableEqualsReplicasMetric = "min_available_equals_replicas"
	OldNotReadyPodsMetric            = "old_not_ready_pods"
)

// Constants for scaling actions
const (
	ScaleUpAction   = "scale_up"
	ScaleDownAction = "scale_down"
)

// Constants for scaling opportunity signals
const (
	PDBBlockedSignal                 = "pdb_blocked"
	MinAvailableEqualsReplicasSignal = "min_available_equals_replicas"
	OldestFailingSignal              = "oldest_failing"
	NewestFailingSignal              = "newest_failing"
	RandomFailingSignal              = "random_failing"
	AllHealthySignal                 = "all_healthy"
	AbandonedPDBSignal               = "abandoned_pdb"
	UnknownSignal                    = "unknown"
	CooldownElapsedSignal            = "cooldown_elapsed"
	// todo: Implement these when additional scaling logic is added
	// OldNotReadyPodsSignal           = "old_not_ready_pods"
	// WouldExceedMinAvailableSignal   = "would_exceed_min_available"
)

// Constants for pod age failure patterns
const (
	OldestFailingPattern = "oldest_failing"
	NewestFailingPattern = "newest_failing"
	RandomFailingPattern = "random_failing"
	AllHealthyPattern    = "all_healthy"
	AbandonedPDBPattern  = "abandoned_pdb"
	UnknownPattern       = "unknown"
)

// Constants for scaling effectiveness predictions
const (
	LikelyHelpfulStr   = "true"
	UnlikelyHelpfulStr = "false"
)

// GetPDBCreatedByUsLabel returns the appropriate label value based on PDB annotations
func GetPDBCreatedByUsLabel(annotations map[string]string) string {
	if ann, ok := annotations["createdBy"]; ok && ann == "DeploymentToPDBController" {
		return PDBCreatedByUsStr
	}
	return PDBNotCreatedByUsStr
}

// GetScalingSignal determines the appropriate signal label for scaling opportunities
func GetScalingSignal(ctx context.Context, c client.Client, pdb *policyv1.PodDisruptionBudget) string {
	// First check if minAvailable == replicas (highest priority signal)
	if pdb.Spec.MinAvailable != nil && pdb.Status.CurrentHealthy > 0 {
		minAvailable := pdb.Spec.MinAvailable.IntValue()
		if minAvailable == int(pdb.Status.CurrentHealthy) {
			return MinAvailableEqualsReplicasSignal
		}
	}

	// Then check pod failure patterns
	pattern, err := analyzePodAgeFailurePattern(ctx, c, pdb)
	if err != nil {
		return PDBBlockedSignal // fallback to generic signal
	}

	switch pattern {
	case OldestFailingPattern:
		return OldestFailingSignal
	case NewestFailingPattern:
		return NewestFailingSignal
	case RandomFailingPattern:
		return RandomFailingSignal
	case AllHealthyPattern:
		return AllHealthySignal
	case AbandonedPDBPattern:
		return AbandonedPDBSignal
	case UnknownPattern:
		return UnknownSignal
	default:
		return PDBBlockedSignal // fallback
	}
}

// analyzePodAgeFailurePattern analyzes the failure pattern of pods based on their age
func analyzePodAgeFailurePattern(ctx context.Context, c client.Client, pdb *policyv1.PodDisruptionBudget) (string, error) {
	// Convert PDB label selector to Kubernetes selector
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return UnknownPattern, err
	}

	// List all pods matching the PDB selector
	var podList corev1.PodList
	err = c.List(ctx, &podList, &client.ListOptions{
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
		if !isPodHealthy(&podList.Items[0]) {
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
		if !isPodHealthy(&pod) {
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
		if !isPodHealthy(&pod) {
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

// isPodHealthy determines if a pod is healthy based on its conditions
func isPodHealthy(pod *corev1.Pod) bool {
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

// UpdatePodAgeFailureMetrics updates the metrics for pod age failure patterns
func UpdatePodAgeFailureMetrics(ctx context.Context, c client.Client, pdb *policyv1.PodDisruptionBudget) {
	pattern, err := analyzePodAgeFailurePattern(ctx, c, pdb)
	if err != nil {
		// Log error but don't fail the reconcile
		return
	}

	// Reset all pattern metrics for this PDB
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, OldestFailingPattern).Set(0)
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, NewestFailingPattern).Set(0)
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, RandomFailingPattern).Set(0)
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, AllHealthyPattern).Set(0)
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, AbandonedPDBPattern).Set(0)
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, UnknownPattern).Set(0)

	// Set the current pattern
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, pattern).Set(1)

	// Update scaling opportunity counter with signal
	signal := GetScalingSignal(ctx, c, pdb)
	ScalingOpportunityCounter.WithLabelValues(
		pdb.Namespace,
		pdb.Name,   // This should be the deployment/PDB name
		"scale_up", // This should be the action, not "v1"
		signal,
	).Inc()
}

func init() {
	// Register metrics with controller-runtime's registry
	ctrlmetrics.Registry.MustRegister(
		DeploymentGauge,
		PDBGauge,
		EvictionCounter,
		BlockedEvictionCounter,
		ScalingOpportunityCounter,
		ActualScalingCounter,
		PDBCreationCounter,
		EvictionAutoScalerCreationCounter,
		NodeCordoningCounter,
		PDBInfoGauge,
		PDBCounter,
		PodAgeFailurePatternGauge,
	)
}
