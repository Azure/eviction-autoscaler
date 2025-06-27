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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
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
		[]string{"namespace", "created_by_us"},
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
	// Labels: namespace, deployment_name, action (scale_up/scale_down)
	ScalingOpportunityCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_scaling_opportunities_total",
			Help: "Total number of times the controller identified scaling opportunities",
		},
		[]string{"namespace", "deployment_name", "action"},
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
	// Labels: cordoned (true/false)
	NodeCordoningCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_node_cordoning_total",
			Help: "Total number of node cordoning events detected by the eviction autoscaler",
		},
		[]string{"cordoned"},
	)

	// OldNotReadyPodsGauge tracks pods that are old and not ready
	// Labels: namespace, target_name, likely_to_help (true/false)
	OldNotReadyPodsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_old_not_ready_pods",
			Help: "Number of old pods that are not ready",
		},
		[]string{"namespace", "target_name", "likely_to_help"},
	)

	// MaxUnavailableGauge tracks the max unavailable value from PDBs
	// Labels: namespace, pdb_name, target_name
	MaxUnavailableGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_max_unavailable",
			Help: "Max unavailable value from PDB configuration",
		},
		[]string{"namespace", "pdb_name", "target_name"},
	)

	// MinAvailableEqualsReplicasGauge tracks when minAvailable equals replicas (indicating PDB might be too strict)
	// Labels: namespace, pdb_name, target_name
	MinAvailableEqualsReplicasGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_min_available_equals_replicas",
			Help: "Indicates when minAvailable equals replicas (1 if true, 0 if false)",
		},
		[]string{"namespace", "pdb_name", "target_name"},
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
)

// Constants for PDB creation tracking
const (
	PDBCreatedByUs    = true
	PDBNotCreatedByUs = false
)

func init() {
	// Register metrics with controller-runtime's registry
	metrics.Registry.MustRegister(
		DeploymentGauge,
		PDBGauge,
		EvictionCounter,
		BlockedEvictionCounter,
		ScalingOpportunityCounter,
		ActualScalingCounter,
		PDBCreationCounter,
		EvictionAutoScalerCreationCounter,
		NodeCordoningCounter,
		OldNotReadyPodsGauge,
		MaxUnavailableGauge,
		MinAvailableEqualsReplicasGauge,
		PDBCounter,
	)
}

// Helper functions to update metrics

// IncrementDeploymentCount increments the deployment gauge
func IncrementDeploymentCount(namespace string, canCreatePDB bool) {
	canCreatePDBStr := "false"
	if canCreatePDB {
		canCreatePDBStr = "true"
	}
	DeploymentGauge.WithLabelValues(namespace, canCreatePDBStr).Inc()
}

// DecrementDeploymentCount decrements the deployment gauge
func DecrementDeploymentCount(namespace string, canCreatePDB bool) {
	canCreatePDBStr := "false"
	if canCreatePDB {
		canCreatePDBStr = "true"
	}
	DeploymentGauge.WithLabelValues(namespace, canCreatePDBStr).Dec()
}

// IncrementPDBCount increments the PDB gauge
func IncrementPDBCount(namespace string, createdByUs bool) {
	createdByUsStr := "false"
	if createdByUs {
		createdByUsStr = "true"
	}
	PDBGauge.WithLabelValues(namespace, createdByUsStr).Inc()
}

// DecrementPDBCount decrements the PDB gauge
func DecrementPDBCount(namespace string, createdByUs bool) {
	createdByUsStr := "false"
	if createdByUs {
		createdByUsStr = "true"
	}
	PDBGauge.WithLabelValues(namespace, createdByUsStr).Dec()
}

// IncrementEvictionCount increments the eviction counter
func IncrementEvictionCount(namespace string) {
	EvictionCounter.WithLabelValues(namespace).Inc()
}

// IncrementBlockedEvictionCount increments the blocked eviction counter
func IncrementBlockedEvictionCount(namespace, pdbName string) {
	BlockedEvictionCounter.WithLabelValues(namespace, pdbName).Inc()
}

// IncrementScalingOpportunityCount increments the scaling opportunity counter
func IncrementScalingOpportunityCount(namespace, deploymentName, action string) {
	ScalingOpportunityCounter.WithLabelValues(namespace, deploymentName, action).Inc()
}

// IncrementActualScalingCount increments the actual scaling counter
func IncrementActualScalingCount(namespace, deploymentName, action string) {
	ActualScalingCounter.WithLabelValues(namespace, deploymentName, action).Inc()
}

// IncrementPDBCreationCount increments the PDB creation counter
func IncrementPDBCreationCount(namespace, deploymentName string) {
	PDBCreationCounter.WithLabelValues(namespace, deploymentName).Inc()
}

// IncrementEvictionAutoScalerCreationCount increments the EvictionAutoScaler creation counter
func IncrementEvictionAutoScalerCreationCount(namespace, pdbName, targetDeployment string) {
	EvictionAutoScalerCreationCounter.WithLabelValues(namespace, pdbName, targetDeployment).Inc()
}

// IncrementNodeCordoningCount increments the node cordoning counter
func IncrementNodeCordoningCount(cordoned bool) {
	cordonedStr := "false"
	if cordoned {
		cordonedStr = "true"
	}
	NodeCordoningCounter.WithLabelValues(cordonedStr).Inc()
}

// UpdateOldNotReadyPods updates the old not ready pods gauge
func UpdateOldNotReadyPods(namespace, targetName string, likelyToHelp bool, count float64) {
	likelyToHelpStr := "false"
	if likelyToHelp {
		likelyToHelpStr = "true"
	}
	OldNotReadyPodsGauge.WithLabelValues(namespace, targetName, likelyToHelpStr).Set(count)
}

// UpdateMaxUnavailable updates the max unavailable gauge
func UpdateMaxUnavailable(namespace, pdbName, targetName string, maxUnavailable float64) {
	MaxUnavailableGauge.WithLabelValues(namespace, pdbName, targetName).Set(maxUnavailable)
}

// UpdateMinAvailableEqualsReplicas updates the min available equals replicas gauge
func UpdateMinAvailableEqualsReplicas(namespace, pdbName, targetName string, value float64) {
	MinAvailableEqualsReplicasGauge.WithLabelValues(namespace, pdbName, targetName).Set(value)
}

// IncrementPDBProcessedCount increments the PDB counter (total processed)
func IncrementPDBProcessedCount(namespace string, createdByUs bool) {
	createdByUsStr := "false"
	if createdByUs {
		createdByUsStr = "true"
	}
	PDBCounter.WithLabelValues(namespace, createdByUsStr).Inc()
}
