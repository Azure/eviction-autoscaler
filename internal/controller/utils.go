package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	v1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsEvictionAutoscalerEnabled checks if eviction autoscaler is enabled for a given namespace.
// Returns true if the namespace is kube-system (always enabled) or has the enable annotation set to "true".
func IsEvictionAutoscalerEnabled(ctx context.Context, c client.Client, namespaceName string) (bool, error) {
	// kube-system namespace is always enabled
	if namespaceName == KubeSystemNamespace {
		return true, nil
	}

	// Fetch the namespace to check for the annotation
	namespace := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: namespaceName}, namespace)
	if err != nil {
		return false, fmt.Errorf("failed to get namespace %s: %w", namespaceName, err)
	}

	// Check if annotation is present and set to "true"
	// Missing annotation or annotation != "true" (including "false") disables the controller
	val, ok := namespace.Annotations[EnableEvictionAutoscalerAnnotationKey]
	return ok && val == EnableEvictionAutoscalerTrue, nil
}

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

// HasNonZeroMaxUnavailable returns true if the deployment has maxUnavailable set to a non-zero value.
// Deployments with maxUnavailable != 0 already tolerate downtime, so PDB creation is skipped.
func HasNonZeroMaxUnavailable(deployment *v1.Deployment) bool {
	if deployment.Spec.Strategy.RollingUpdate == nil {
		return false
	}
	maxUnavailable := deployment.Spec.Strategy.RollingUpdate.MaxUnavailable
	if maxUnavailable == nil {
		return false
	}
	if maxUnavailable.Type == intstr.Int {
		return maxUnavailable.IntVal != 0
	}
	// String type - check for "0" or "0%"
	return maxUnavailable.StrVal != "0" && maxUnavailable.StrVal != "0%"
}

// FindPDBForDeployment finds and returns the PDB that matches the deployment's pod selector
// Returns the matching PDB and true if found, or nil and false if not found
func FindPDBForDeployment(ctx context.Context, c client.Client, deployment *v1.Deployment) (*policyv1.PodDisruptionBudget, bool, error) {
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
			return &pdb, true, nil
		}
	}
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
