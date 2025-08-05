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
	// Labels: namespace, deployment_name, action (scale_up/scale_down), signal, age_pattern, likely_helpful
	ScalingOpportunityCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_scaling_opportunities_total",
			Help: "Total number of times the controller identified scaling opportunities",
		},
		[]string{"namespace", "deployment_name", "action", "signal", "age_pattern", "likely_helpful"},
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

	// ScalingEffectivenessGauge tracks whether scaling would likely help based on failure patterns
	// Labels: namespace, pdb_name, likely_helpful (true/false)
	ScalingEffectivenessGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_scaling_effectiveness_prediction",
			Help: "Prediction of whether scaling up would be effective (1 = likely helpful, 0 = unlikely helpful)",
		},
		[]string{"namespace", "pdb_name", "likely_helpful"},
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
	PDBBlockedSignal                = "pdb_blocked"
	MinAvailableEqualsDesiredSignal = "min_available_equals_desired_healthy"
	CooldownElapsedSignal           = "cooldown_elapsed"
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
func GetScalingSignal(pdb *policyv1.PodDisruptionBudget) string {
	// TODO: Could implement later for proactive scaling logic
	// if pdb.Spec.MinAvailable != nil && int64(pdb.Spec.MinAvailable.IntValue()) == int64(pdb.Status.DesiredHealthy) {
	//     return MinAvailableEqualsDesiredSignal
	// }
	return PDBBlockedSignal
}

// GetScalingOpportunityLabels gets age pattern and effectiveness labels for scaling opportunities
func GetScalingOpportunityLabels(ctx context.Context, c client.Client, pdb *policyv1.PodDisruptionBudget) (string, string) {
	pattern, err := AnalyzePodAgeFailurePattern(ctx, c, pdb)
	if err != nil {
		return "unknown", "unknown"
	}
	
	isHelpful := PredictScalingEffectiveness(pattern)
	helpfulLabel := UnlikelyHelpfulStr
	if isHelpful {
		helpfulLabel = LikelyHelpfulStr
	}
	
	return pattern, helpfulLabel
}

// AnalyzePodAgeFailurePattern analyzes the failure pattern of pods based on their age
func AnalyzePodAgeFailurePattern(ctx context.Context, c client.Client, pdb *policyv1.PodDisruptionBudget) (string, error) {
	// Convert PDB label selector to Kubernetes selector
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return AllHealthyPattern, err
	}

	// List all pods matching the PDB selector
	var podList corev1.PodList
	err = c.List(ctx, &podList, &client.ListOptions{
		Namespace:     pdb.Namespace,
		LabelSelector: selector,
	})
	if err != nil {
		return AllHealthyPattern, err
	}

	if len(podList.Items) == 0 {
		return AllHealthyPattern, nil
	}

	// Sort pods by creation time (oldest first)
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

	// Analyze failure pattern - simpler logic based on where most failures are
	totalPods := len(podList.Items)
	totalUnhealthyPods := len(unhealthyPods)
	
	// If less than 2 pods, can't determine pattern clearly
	if totalPods < 2 {
		return AllHealthyPattern, nil
	}

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

	// Simple majority logic:
	// If more than 66% of failures are in oldest half -> oldest failing
	// If more than 66% of failures are in newest half -> newest failing  
	// Otherwise -> random failing
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

// PredictScalingEffectiveness predicts whether scaling up would be helpful based on failure pattern
func PredictScalingEffectiveness(pattern string) bool {
	switch pattern {
	case OldestFailingPattern:
		// If oldest pods are failing, scaling up helps by giving new pods time to stabilize
		return true
	case NewestFailingPattern:
		// If newest pods are failing, scaling up won't help much as new pods will likely also fail
		return false
	case RandomFailingPattern:
		// Random failures - unclear if scaling helps, default to cautious approach
		return false
	case AllHealthyPattern:
		// All pods healthy, no need to scale
		return false
	default:
		return false
	}
}

// UpdatePodAgeFailureMetrics updates the metrics for pod age failure patterns
func UpdatePodAgeFailureMetrics(ctx context.Context, c client.Client, pdb *policyv1.PodDisruptionBudget) {
	pattern, err := AnalyzePodAgeFailurePattern(ctx, c, pdb)
	if err != nil {
		// Log error but don't fail the reconcile
		return
	}

	// Reset all pattern metrics for this PDB
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, OldestFailingPattern).Set(0)
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, NewestFailingPattern).Set(0)
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, RandomFailingPattern).Set(0)
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, AllHealthyPattern).Set(0)

	// Set the current pattern
	PodAgeFailurePatternGauge.WithLabelValues(pdb.Namespace, pdb.Name, pattern).Set(1)

	// Update scaling effectiveness prediction
	isHelpful := PredictScalingEffectiveness(pattern)
	helpfulLabel := UnlikelyHelpfulStr
	if isHelpful {
		helpfulLabel = LikelyHelpfulStr
	}

	// Reset both effectiveness metrics for this PDB
	ScalingEffectivenessGauge.WithLabelValues(pdb.Namespace, pdb.Name, LikelyHelpfulStr).Set(0)
	ScalingEffectivenessGauge.WithLabelValues(pdb.Namespace, pdb.Name, UnlikelyHelpfulStr).Set(0)

	// Set the current effectiveness prediction
	ScalingEffectivenessGauge.WithLabelValues(pdb.Namespace, pdb.Name, helpfulLabel).Set(1)
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
		ScalingEffectivenessGauge,
	)
}
