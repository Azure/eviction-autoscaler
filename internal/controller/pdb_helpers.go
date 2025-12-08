package controllers

import (
	"context"
	"fmt"

	v1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ShouldSkipPDBCreation checks if PDB creation should be skipped for a deployment
// Returns true if:
// - Deployment has pdb-create annotation set to false
// - Deployment has non-zero maxUnavailable
func ShouldSkipPDBCreation(deployment *v1.Deployment) (bool, string) {
	// Check for pdb-create annotation on deployment
	if val, ok := deployment.Annotations[PDBCreateAnnotationKey]; ok {
		if val == PDBCreateAnnotationFalse {
			return true, "pdb-create annotation set to false"
		}
	}

	// Check if deployment has non-zero maxUnavailable
	if HasNonZeroMaxUnavailable(deployment) {
		return true, "maxUnavailable != 0"
	}

	return false, ""
}

// FindPDBForDeployment finds and returns the PDB that matches the deployment's pod selector
// If onlyOwnedByController is true:
//   - Returns (pdb, true, nil) if a matching PDB exists AND is owned by EvictionAutoScaler
//   - Returns (nil, false, nil) if a matching PDB exists BUT is not owned by EvictionAutoScaler
//   - Returns (nil, false, nil) if no matching PDB exists
// If onlyOwnedByController is false:
//   - Returns (pdb, true, nil) if any matching PDB exists (regardless of ownership)
//   - Returns (nil, false, nil) if no matching PDB exists
func FindPDBForDeployment(ctx context.Context, c client.Client, deployment *v1.Deployment, onlyOwnedByController bool) (*policyv1.PodDisruptionBudget, bool, error) {
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
