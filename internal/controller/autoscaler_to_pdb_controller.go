package controllers

import (
	"context"
	"errors"
	"strings"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	v1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// AutoscalerToPDBReconciler watches HPA and KEDA ScaledObject changes
// and updates the PDB minAvailable to match their min replicas floor.
// This ensures the PDB stays correct when an autoscaler's minReplicas/minReplicaCount
// changes without a corresponding deployment spec change.
//
// A single controller handles both HPA and ScaledObject because the reconcile logic
// is identical (resolve the target deployment from scaleTargetRef → resolve min replicas
// floor → update PDB). Two separate controllers would duplicate this logic without
// any benefit.
type AutoscalerToPDBReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Filter filter
}

// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;update

// Reconcile is triggered when an HPA or ScaledObject changes. The request key is the
// autoscaler's namespace/name. We resolve the target deployment from its scaleTargetRef.
func (r *AutoscalerToPDBReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Gate: only act in namespaces where the eviction autoscaler is enabled.
	// Without this, we'd update PDBs in namespaces the operator isn't managing.
	isEnabled, err := r.Filter.Filter(ctx, r.Client, req.Namespace)
	if err != nil {
		logger.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", req.Namespace)
		return reconcile.Result{}, err
	}
	if !isEnabled {
		return reconcile.Result{}, nil
	}

	// Resolve the target deployment name from the autoscaler's scaleTargetRef.
	// Try HPA first, then ScaledObject.
	deploymentName, err := r.resolveDeploymentName(ctx, req)
	if err != nil {
		return reconcile.Result{}, err
	}
	if deploymentName == "" {
		return reconcile.Result{}, nil
	}

	// Fetch the target deployment. If it doesn't exist (e.g. HPA outlived it), there's nothing to do.
	var deployment v1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: deploymentName}, &deployment); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Don't update PDB minAvailable during an active surge. The eviction controller
	// temporarily raises replicas (and possibly HPA/KEDA minReplicas) above the floor
	// to handle evictions. Updating the PDB now would lock in the surged value as the
	// new floor. The surge revert path will restore original values, and the next
	// reconcile after that will set minAvailable correctly.
	// Check the HPA/KEDA object for the surge annotation (not the deployment),
	// since the surge annotation is placed on the autoscaler object.
	if r.isSurgeActiveOnAutoscaler(ctx, req) {
		logger.V(1).Info("Surge active on autoscaler, skipping PDB minAvailable update",
			"deployment", deployment.Name)
		return reconcile.Result{}, nil
	}

	// Find the PDB that matches this deployment's pod selector labels.
	// Only consider PDBs created by this controller (ownedBy annotation) — user-managed PDBs
	// should not be modified. If no controller-owned PDB exists, there's nothing to update.
	//
	// PDB creation is intentionally left to DeploymentToPDBReconciler, not this controller.
	// The pdb-create annotation lives on the Deployment, so the deployment controller is the
	// natural place to gate and create PDBs. Duplicating that decision here (or supporting
	// the annotation on HPA/ScaledObject too) would add complexity without benefit.
	pdb, found, err := findPDBForDeployment(ctx, r.Client, &deployment, true)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !found {
		return reconcile.Result{}, nil
	}

	// Resolve the autoscaler's minimum replica floor (KEDA minReplicaCount > HPA minReplicas).
	// If no autoscaler targets this deployment, the deployment controller owns PDB updates
	// and we bail out. We pass 0 as the fallback (unused when found==true).
	minAvailable, found, err := ResolveMinReplicas(ctx, r.Client, req.Namespace, deploymentName, ResourceTypeDeployment, 0)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !found {
		logger.V(1).Info("No HPA/KEDA found for deployment, skipping PDB update",
			"deployment", deploymentName)
		return reconcile.Result{}, nil
	}

	// Idempotency: skip the API write if the PDB already has the correct value.
	// This avoids unnecessary updates and the resulting watch events.
	if pdb.Spec.MinAvailable != nil && pdb.Spec.MinAvailable.IntVal == minAvailable {
		return reconcile.Result{}, nil
	}

	pdb.Spec.MinAvailable = &intstr.IntOrString{IntVal: minAvailable}
	if err := r.Update(ctx, pdb); err != nil {
		logger.Error(err, "unable to update PDB minAvailable from autoscaler",
			"pdb", pdb.Name, "minAvailable", minAvailable)
		return reconcile.Result{}, err
	}

	logger.Info("Updated PDB minAvailable from autoscaler floor",
		"pdb", pdb.Name, "minAvailable", minAvailable)
	return reconcile.Result{}, nil
}

// resolveDeploymentName extracts the target deployment name from the autoscaler
// identified by req.NamespacedName. Tries HPA first, then ScaledObject.
// Returns "" if the autoscaler doesn't target a Deployment or was not found.
func (r *AutoscalerToPDBReconciler) resolveDeploymentName(ctx context.Context, req ctrl.Request) (string, error) {
	// Try HPA
	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := r.Get(ctx, req.NamespacedName, &hpa); err == nil {
		// Skip KEDA-managed HPAs — the ScaledObject event handles those
		if isKEDAManagedHPA(&hpa) {
			return "", nil
		}
		if !strings.EqualFold(hpa.Spec.ScaleTargetRef.Kind, ResourceTypeDeployment) {
			return "", nil
		}
		return hpa.Spec.ScaleTargetRef.Name, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	// Try ScaledObject
	var scaledObj kedav1alpha1.ScaledObject
	if err := r.Get(ctx, req.NamespacedName, &scaledObj); err == nil {
		name := scaledObj.Spec.ScaleTargetRef.Name
		kind := scaledObj.Spec.ScaleTargetRef.Kind
		if kind == "" {
			kind = ResourceTypeDeployment
		}
		if !strings.EqualFold(kind, ResourceTypeDeployment) {
			return "", nil
		}
		return name, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	return "", nil
}

// isSurgeActiveOnAutoscaler checks whether the HPA or ScaledObject identified by req
// has the evictionSurgeReplicas annotation, indicating an active surge.
func (r *AutoscalerToPDBReconciler) isSurgeActiveOnAutoscaler(ctx context.Context, req ctrl.Request) bool {
	// Check HPA
	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := r.Get(ctx, req.NamespacedName, &hpa); err == nil {
		if _, exists := hpa.Annotations[EvictionSurgeReplicasAnnotationKey]; exists {
			return true
		}
	}

	// Check ScaledObject
	var scaledObj kedav1alpha1.ScaledObject
	if err := r.Get(ctx, req.NamespacedName, &scaledObj); err == nil {
		if _, exists := scaledObj.Annotations[EvictionSurgeReplicasAnnotationKey]; exists {
			return true
		}
	}

	return false
}

// SetupWithManager registers watches on HPA and KEDA ScaledObject resources.
// Events are enqueued with the autoscaler's own key; Reconcile resolves the
// target deployment from the autoscaler's scaleTargetRef.
func (r *AutoscalerToPDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		Named("autoscaler-to-pdb").
		Watches(&autoscalingv2.HorizontalPodAutoscaler{},
			&handler.EnqueueRequestForObject{})

	// Try to watch KEDA ScaledObjects (only if the CRD is installed).
	// CRD discovery happens once at startup. If KEDA is installed after the controller
	// starts, a restart is required to begin watching ScaledObjects.
	if err := r.discoverScaledObjectCRD(mgr); err == nil {
		builder = builder.WatchesRawSource(
			source.Kind(mgr.GetCache(), &kedav1alpha1.ScaledObject{},
				&handler.TypedEnqueueRequestForObject[*kedav1alpha1.ScaledObject]{}))
	} else {
		mgr.GetLogger().Info("KEDA ScaledObject CRD not found, skipping ScaledObject watch")
	}

	return builder.Complete(r)
}

// discoverScaledObjectCRD checks if the KEDA ScaledObject CRD is available.
func (r *AutoscalerToPDBReconciler) discoverScaledObjectCRD(mgr ctrl.Manager) error {
	_, err := mgr.GetRESTMapper().RESTMapping(
		schema.GroupKind{Group: "keda.sh", Kind: "ScaledObject"},
		"v1alpha1",
	)
	if err != nil {
		return errors.New("ScaledObject CRD not available")
	}
	return nil
}
