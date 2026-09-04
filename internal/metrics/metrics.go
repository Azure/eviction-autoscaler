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
	policyv1 "k8s.io/api/policy/v1"
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

	// ZeroMaxSurgeWorkloadGauge tracks workloads whose rollout maxSurge resolves to 0
	// (an explicit maxSurge: 0, a Recreate strategy, or an unset RollingUpdate), set to
	// 1 per such workload and 0 otherwise, so the series sum is the count of maxSurge:0
	// workloads currently seen in the cluster.
	// Labels: namespace, name
	ZeroMaxSurgeWorkloadGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_zero_maxsurge_workloads",
			Help: "Workloads whose rollout maxSurge resolves to 0, seen by the eviction autoscaler",
		},
		[]string{"namespace", "name"},
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

	// PanicCounter tracks recovered reconcile panics
	// Labels: namespace, target_name, controller
	PanicCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_panics_total",
			Help: "Total number of panics recovered in the eviction autoscaler reconcile loop",
		},
		[]string{"namespace", "target_name", "controller"},
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

	// ControllerEnabled is 1 when the global kill switch (CONTROLLER_ENABLED) is on and reconcilers
	// are registered, else 0. It is set once at startup on both paths, so it stays a continuous
	// series across a disable/enable — letting dashboards and alerts tell "disabled by config"
	// apart from "process down / scrape failed" (the /metrics endpoint serves either way).
	ControllerEnabled = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_controller_enabled",
			Help: "1 when the controller is enabled (reconcilers registered), 0 when disabled via the CONTROLLER_ENABLED kill switch",
		},
	)

	// PDBFloorTeardownUnrestorableCounter tracks PDB-floor teardowns where the partner
	// PDB could not be restored to its original spec because the snapshot annotation was
	// missing (only reachable via external annotation tampering — the snapshot and pin
	// annotations are otherwise written/removed atomically). The floor finalizer is
	// released anyway to avoid a stuck Terminating CR, leaving the (over-protective, never
	// availability-regressing) pinned floor in place for an operator to notice.
	// Labels: namespace, pdb_name
	PDBFloorTeardownUnrestorableCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "eviction_autoscaler_pdb_floor_teardown_unrestorable_total",
			Help: "Total number of PDB-floor teardowns where the original spec could not be restored (snapshot missing); finalizer released to avoid a stuck Terminating CR.",
		},
		[]string{"namespace", "pdb_name"},
	)

	// SurgeActive is 1 while a workload is currently surged by the controller, else 0. It is
	// re-asserted from the durable surge marker on every reconcile of an active surge (not only
	// on transitions), so it self-heals after a controller restart and a Prometheus `for:` on it
	// measures the true time a surge has been held (e.g. alert when a surge is stuck for too long).
	// Labels: namespace, target_name.
	SurgeActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_surge_active",
			Help: "1 while a workload is currently surged by the eviction autoscaler, else 0.",
		},
		[]string{"namespace", "target_name"},
	)

	// SurgeReplicas is the number of EXTRA replicas the eviction autoscaler currently requests via
	// an active surge — the surged replica count minus the recorded pre-surge baseline. This is the
	// desired (spec) surge, i.e. what the controller asked Kubernetes for, and only the EAS-owned
	// surge is counted (it is derived from the durable surge marker, so an independent HPA/KEDA
	// baseline is excluded). It is the basis for surge cost: replica-seconds =
	// sum_over_time(surge_replicas) × your per-replica cost.
	// Labels: namespace, target_name.
	SurgeReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_surge_replicas",
			Help: "Extra replicas the eviction autoscaler currently requests via an active surge (surged count minus baseline).",
		},
		[]string{"namespace", "target_name"},
	)

	// SurgeReplicasReady is the number of EXTRA replicas actually Ready above the baseline while a
	// surge is active, bounded by SurgeReplicas. Because surged pods may sit Pending until the
	// Cluster Autoscaler brings up nodes, realized lags the requested SurgeReplicas — the gap is the
	// not-yet-scheduled surge (a proxy for pending capacity / nodes still coming up). For a direct
	// Deployment surge the baseline is the true pre-surge replica count, so realized is exactly the
	// pods EAS added; for an HPA/KEDA surge the baseline is the autoscaler min floor, so realized is
	// an upper bound (it can include load-driven replicas that were already running above the floor).
	// Labels: namespace, target_name.
	SurgeReplicasReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_surge_replicas_ready",
			Help: "Extra replicas actually Ready above baseline during an active surge (bounded by surge_replicas).",
		},
		[]string{"namespace", "target_name"},
	)

	// PDBFloorPinned is 1 while a PDB's floor is currently pinned by the controller (the CR's
	// Status.PDBFloorPinned intent), else 0. Re-asserted from durable status every reconcile so
	// it self-heals across restarts; a Prometheus `for:` measures how long a PDB has been pinned,
	// enabling a "pinned far longer than any legitimate drain" alert.
	// Labels: namespace, pdb_name, target_name.
	PDBFloorPinned = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_pdb_floor_pinned",
			Help: "1 while a PDB's floor is currently pinned by the eviction autoscaler, else 0.",
		},
		[]string{"namespace", "pdb_name", "target_name"},
	)

	// PDBMutated is 1 while a PDB actually carries the controller's floor mutation (observed
	// from the live PDB annotation), else 0. Independent of the CR's intent, so a PDBMutated==1
	// with no corresponding PDBFloorPinned==1 (or no live CR) surfaces a drifted / orphaned
	// mutation for an operator to notice.
	// Labels: namespace, pdb_name.
	PDBMutated = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_pdb_mutated",
			Help: "1 while a PDB currently carries the eviction autoscaler's floor mutation, else 0.",
		},
		[]string{"namespace", "pdb_name"},
	)

	// Degraded is 1 while an EvictionAutoScaler is in a Degraded state, labelled by the reason
	// (e.g. SurgeForbidden, MissingTarget, UnsupportedAutoscalerConfiguration). It is keyed by the
	// EvictionAutoScaler's own name so it can be cleared on delete; it is cleared at the start of
	// each reconcile (and on NotFound) and re-set only if the object is still degraded, so it
	// reflects the current state and clears on recovery or removal. Alert on `== 1 for:` to catch
	// a controller that cannot protect a workload.
	// Labels: namespace, name, reason.
	Degraded = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "eviction_autoscaler_degraded",
			Help: "1 while an EvictionAutoScaler is Degraded, labelled by reason; 0/absent otherwise.",
		},
		[]string{"namespace", "name", "reason"},
	)
)

// Constants for PDB creation tracking
const (
	PDBCreatedByUsStr    = "true"
	PDBNotCreatedByUsStr = "false"
)

// ClearDegraded removes any Degraded series for an EvictionAutoScaler (across all reasons). Call
// it at the start of a reconcile — and on delete — so the Degraded gauge reflects only the
// object's current state and clears automatically once it recovers or is removed.
func ClearDegraded(namespace, name string) {
	Degraded.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
}

// ClearPDBFloorPinned removes any PDBFloorPinned series for a namespace/pdb_name (across
// targets). Used when a PDB is not (or no longer) pinned, so the gauge drops to absent even
// when the target label is unknown (e.g. the EAS is gone).
func ClearPDBFloorPinned(namespace, pdbName string) {
	PDBFloorPinned.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "pdb_name": pdbName})
}

// ClearPDBMutated removes the PDBMutated series for a namespace/pdb_name. Call it when the PDB
// object is deleted: the controller holds no finalizer on the PDB, so a still-mutated PDB can
// vanish at any time, and its gauge would otherwise leak a stuck series against an object that
// no longer exists and can never be reconciled back to 0.
func ClearPDBMutated(namespace, pdbName string) {
	PDBMutated.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "pdb_name": pdbName})
}

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

func init() {
	// Register metrics with controller-runtime's registry
	ctrlmetrics.Registry.MustRegister(
		DeploymentGauge,
		ZeroMaxSurgeWorkloadGauge,
		ControllerEnabled,
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
		PanicCounter,
		PDBFloorTeardownUnrestorableCounter,
		SurgeActive,
		SurgeReplicas,
		SurgeReplicasReady,
		PDBFloorPinned,
		PDBMutated,
		Degraded,
	)
}
