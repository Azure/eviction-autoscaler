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
	// Labels: namespace, pod_name
	EvictionCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_evictions_total",
			Help: "Total number of evictions noticed by the eviction autoscaler",
		},
		[]string{"namespace", "pod_name"},
	)

	// BlockedEvictionCounter tracks how often evictions are blocked by PDBs
	// Labels: namespace, pdb_name, pod_name
	BlockedEvictionCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_blocked_evictions_total",
			Help: "Total number of evictions blocked by PDBs",
		},
		[]string{"namespace", "pdb_name", "pod_name"},
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
	// Labels: node_name, cordoned (true/false)
	NodeCordoningCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_node_cordoning_total",
			Help: "Total number of node cordoning events detected by the eviction autoscaler",
		},
		[]string{"node_name", "cordoned"},
	)

	// NodeDrainCounter tracks node drain events detected (pods evicted from cordoned nodes)
	// Labels: node_name, pod_name, namespace
	NodeDrainCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_node_drain_total",
			Help: "Total number of pods evicted from nodes during drain operations",
		},
		[]string{"node_name", "pod_name", "namespace"},
	)
)

func init() {
	// Register metrics with controller-runtime's registry
	// Use safe registration to prevent panics
	collectors := []prometheus.Collector{
		DeploymentGauge,
		PDBGauge,
		EvictionCounter,
		BlockedEvictionCounter,
		ScalingOpportunityCounter,
		ActualScalingCounter,
		PDBCreationCounter,
		EvictionAutoScalerCreationCounter,
		NodeCordoningCounter,
		NodeDrainCounter,
	}

	for _, collector := range collectors {
		if err := metrics.Registry.Register(collector); err != nil {
			// Log error but don't crash - metrics are not critical for controller function
			// In production, you might want to use a proper logger here
			// For now, we'll just continue without that specific metric
			continue
		}
	}
}

// Helper functions to update metrics

// UpdateDeploymentCount updates the deployment gauge
func UpdateDeploymentCount(namespace string, canCreatePDB bool, count float64) {
	canCreatePDBStr := "false"
	if canCreatePDB {
		canCreatePDBStr = "true"
	}
	DeploymentGauge.WithLabelValues(namespace, canCreatePDBStr).Set(count)
}

// UpdatePDBCount updates the PDB gauge
func UpdatePDBCount(namespace string, createdByUs bool, count float64) {
	createdByUsStr := "false"
	if createdByUs {
		createdByUsStr = "true"
	}
	PDBGauge.WithLabelValues(namespace, createdByUsStr).Set(count)
}

// IncrementEvictionCount increments the eviction counter
func IncrementEvictionCount(namespace, podName string) {
	EvictionCounter.WithLabelValues(namespace, podName).Inc()
}

// IncrementBlockedEvictionCount increments the blocked eviction counter
func IncrementBlockedEvictionCount(namespace, pdbName, podName string) {
	BlockedEvictionCounter.WithLabelValues(namespace, pdbName, podName).Inc()
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
func IncrementNodeCordoningCount(nodeName string, cordoned bool) {
	cordonedStr := "false"
	if cordoned {
		cordonedStr = "true"
	}
	NodeCordoningCounter.WithLabelValues(nodeName, cordonedStr).Inc()
}

// IncrementNodeDrainCount increments the node drain counter
func IncrementNodeDrainCount(nodeName, podName, namespace string) {
	NodeDrainCounter.WithLabelValues(nodeName, podName, namespace).Inc()
}
