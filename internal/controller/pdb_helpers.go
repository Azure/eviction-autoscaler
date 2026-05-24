package controllers

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8s_types "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// shouldSkipPDBCreation checks if PDB creation should be skipped for a deployment
// Returns true if:
// - Deployment has pdb-create annotation set to false
// - Deployment has non-zero maxUnavailable
func shouldSkipPDBCreation(deployment *v1.Deployment) (bool, string) {
	// Check for pdb-create annotation on deployment
	if val, ok := deployment.Annotations[PDBCreateAnnotationKey]; ok {
		pdbcreate, err := strconv.ParseBool(val)
		if err != nil {
			return true, "unknown annotation value for pdb-create annotation " + val
		}

		if !pdbcreate {
			return true, "pdb-create annotation set to false"
		}
	}

	// Check if deployment has non-zero maxUnavailable
	if hasNonZeroMaxUnavailable(deployment) {
		return true, "maxUnavailable != 0"
	}

	return false, ""
}

// findPDBForDeployment finds and returns the PDB that matches the deployment's pod selector
// If onlyOwnedByController is true:
//   - Returns (pdb, true, nil) if a matching PDB exists AND is owned by EvictionAutoScaler
//   - Returns (nil, false, nil) if a matching PDB exists BUT is not owned by EvictionAutoScaler
//   - Returns (nil, false, nil) if no matching PDB exists
//
// If onlyOwnedByController is false:
//   - Returns (pdb, true, nil) if any matching PDB exists (regardless of ownership)
//   - Returns (nil, false, nil) if no matching PDB exists
func findPDBForDeployment(ctx context.Context, c client.Client, deployment *v1.Deployment, onlyOwnedByController bool) (*policyv1.PodDisruptionBudget, bool, error) {
	var pdbList policyv1.PodDisruptionBudgetList
	if err := c.List(ctx, &pdbList, client.InNamespace(deployment.Namespace)); err != nil {
		return nil, false, fmt.Errorf("failed to list PDBs: %w", err)
	}

	for _, pdb := range pdbList.Items {
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			continue
		}
		if selector.Matches(labels.Set(deployment.Spec.Template.Labels)) {
			// Found a matching PDB
			if onlyOwnedByController {
				// Only return true if it's owned by EvictionAutoScaler
				if pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName {
					return &pdb, true, nil
				}
				// Matching PDB exists but is not owned by us - return false
				return nil, false, nil
			} else {
				// Return any matching PDB regardless of ownership
				return &pdb, true, nil
			}
		}
	}
	// No matching PDB found
	return nil, false, nil
}

// CreatePDBForDeployment creates a PDB for the given deployment with standard configuration
func CreatePDBForDeployment(ctx context.Context, c client.Client, deployment *v1.Deployment) error {
	controller := true
	blockOwnerDeletion := true

	// Use KEDA/HPA minReplicas when available instead of deployment.spec.replicas,
	// since the autoscaler controls the actual replica count and may have scaled above its floor.
	var deployReplicas int32 = 1
	if deployment.Spec.Replicas != nil {
		deployReplicas = *deployment.Spec.Replicas
	}
	minAvailable, _, err := ResolveMinReplicas(ctx, c, deployment.Namespace, deployment.Name, ResourceTypeDeployment, deployReplicas)
	if err != nil {
		return err
	}

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.Name,
			Namespace: deployment.Namespace,
			Annotations: map[string]string{
				PDBOwnedByAnnotationKey: ControllerName,
				"target":                deployment.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               ResourceTypeDeployment,
					Name:               deployment.Name,
					UID:                deployment.UID,
					Controller:         &controller,
					BlockOwnerDeletion: &blockOwnerDeletion,
				},
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{IntVal: minAvailable},
			Selector:     &metav1.LabelSelector{MatchLabels: deployment.Spec.Selector.MatchLabels},
		},
	}

	return c.Create(ctx, pdb)
}

// Watch Namespace calls this to handle dynamic enable/disable via annotations.
// When a namespace's eviction-autoscaler.azure.com/enable annotation changes,
// we need to reconcile all PDBs in that namespace to create or delete EvictionAutoScalers accordingly.
//
// Performance Note: The List call below reads from the controller-runtime client cache,
// NOT directly from the Kubernetes API server. This cache is maintained in-memory and
// automatically kept up-to-date via watches. Therefore, listing PDBs is a fast
// in-memory operation with no API server round-trip overhead.
//
// Important: We only reconcile user-owned PDBs (those without the ownedBy annotation).
// Controller-owned PDBs are managed by DeploymentToPDBReconciler. When a controller-owned
// PDB is deleted (due to namespace being disabled or deployment being deleted), its
// EvictionAutoScaler is automatically garbage collected by Kubernetes due to the
// OwnerReference from EvictionAutoScaler -> PDB.
func requeuePDBsOnNamespaceChange(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		logger := log.FromContext(ctx)
		ns, ok := obj.(*corev1.Namespace)
		if !ok {
			return nil
		}

		// List all PDBs in the namespace
		var pdbList policyv1.PodDisruptionBudgetList
		if err := c.List(ctx, &pdbList, client.InNamespace(ns.Name)); err != nil {
			logger.Error(err, "Failed to list PDBs in namespace", "namespace", ns.Name)
			return nil
		}

		var requests []reconcile.Request
		for _, pdb := range pdbList.Items {
			// Only enqueue user-owned PDBs (without ownedBy annotation)
			isControllerOwned := pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName
			if !isControllerOwned {
				requests = append(requests, reconcile.Request{
					NamespacedName: k8s_types.NamespacedName{
						Namespace: pdb.Namespace,
						Name:      pdb.Name,
					},
				})
			}
		}
		return requests
	}
}

// tryGet safely retrieves a value from a map, returning empty string if map is nil
func tryGet(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

// resolveMinAvailableInt returns the absolute (integer) minAvailable count for the PDB.
// For integer-type specs the value is used directly. For percentage-type specs it is
// computed against baselineReplicas (the pre-surge replica count) using ceiling division,
// matching how the PDB controller computes desiredHealthy.
func resolveMinAvailableInt(pdb *policyv1.PodDisruptionBudget, baselineReplicas int32) int32 {
	if pdb.Spec.MinAvailable == nil {
		return 0
	}
	ma := pdb.Spec.MinAvailable
	if ma.Type == intstr.Int {
		return ma.IntVal
	}
	// Percentage: e.g. "50%" → ceil(0.5 × baselineReplicas)
	pct, err := strconv.Atoi(strings.TrimSuffix(ma.StrVal, "%"))
	if err != nil || pct < 0 {
		return 0
	}
	return int32(math.Ceil(float64(baselineReplicas) * float64(pct) / 100.0))
}

// countUnschedulablePodsForTarget counts pods matched by the PDB's selector that are
// Pending AND have a PodScheduled condition with Status=False and Reason=Unschedulable.
// This precisely identifies capacity-bound surge pods (scheduler tried but could not
// place them) rather than pods that are transiently Pending for other reasons such as
// image pulls, init containers, or volume binding.
func countUnschedulablePodsForTarget(ctx context.Context, c client.Client, namespace string, pdb *policyv1.PodDisruptionBudget) (int, error) {
	if pdb.Spec.Selector == nil {
		return 0, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return 0, fmt.Errorf("invalid PDB selector: %w", err)
	}
	var podList corev1.PodList
	if err := c.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return 0, fmt.Errorf("listing pods for unschedulable check: %w", err)
	}
	count := 0
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled &&
				cond.Status == corev1.ConditionFalse &&
				cond.Reason == corev1.PodReasonUnschedulable {
				count++
				break
			}
		}
	}
	return count, nil
}

// triggerOnPDBAnnotationChange checks if a PDB update event should trigger reconciliation
// by comparing the ownedBy annotation between old and new PDB
func triggerOnPDBAnnotationChange(e event.UpdateEvent, logger logr.Logger) bool {
	oldPDB, okOld := e.ObjectOld.(*policyv1.PodDisruptionBudget)
	newPDB, okNew := e.ObjectNew.(*policyv1.PodDisruptionBudget)
	if okOld && okNew {
		oldVal := tryGet(oldPDB.Annotations, PDBOwnedByAnnotationKey)
		newVal := tryGet(newPDB.Annotations, PDBOwnedByAnnotationKey)
		if oldVal != newVal {
			logger.Info("PDB update event detected, ownedBy annotation changed",
				"namespace", newPDB.Namespace, "name", newPDB.Name,
				"oldValue", oldVal, "newValue", newVal)
			return true
		}
	}
	return false
}

// countPodsOnCordoned counts pods matching the PDB selector that are currently on cordoned
// (Unschedulable) nodes. It aggregates across all cordoned nodes, so simultaneous drains
// are counted correctly.
func countPodsOnCordoned(ctx context.Context, c client.Client, pdb *policyv1.PodDisruptionBudget) (int32, error) {
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return 0, fmt.Errorf("invalid PDB selector: %w", err)
	}

	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.InNamespace(pdb.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return 0, fmt.Errorf("failed to list pods for PDB %s: %w", pdb.Name, err)
	}

	// We use node cordon (Spec.Unschedulable) as the signal for "pods need to move".
	// This is the best signal available today via the controller-runtime cache (no API server round-trip).
	// In the future this may be replaced by a more direct pod-eviction signal — e.g. via an
	// admission webhook interceptor or pod conditions — which would let us right-size the surge
	// without needing to inspect nodes at all.
	var nodeList corev1.NodeList
	if err := c.List(ctx, &nodeList); err != nil {
		return 0, fmt.Errorf("failed to list nodes: %w", err)
	}
	cordoned := make(map[string]bool, len(nodeList.Items))
	for _, node := range nodeList.Items {
		cordoned[node.Name] = node.Spec.Unschedulable
	}

	var count int32
	for _, pod := range podList.Items {
		if cordoned[pod.Spec.NodeName] {
			count++
		}
	}
	return count, nil
}
