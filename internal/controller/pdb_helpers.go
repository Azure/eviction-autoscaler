package controllers

import (
	"context"
	"fmt"
	"strconv"

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

// ShouldSkipPDBCreation checks if PDB creation should be skipped for a deployment
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
			MinAvailable: &intstr.IntOrString{IntVal: *deployment.Spec.Replicas},
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
