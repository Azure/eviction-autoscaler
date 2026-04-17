package controllers

import (
	"context"
	"errors"
	"strings"

	v1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
type AutoscalerToPDBReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Filter filter
}

// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch

// Reconcile is triggered when an HPA or ScaledObject changes. The request key is the
// target deployment's namespace/name (mapped by the watch handlers).
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

	// Resolve the target deployment from the HPA/ScaledObject's scaleTargetRef.
	// The watch mappers translate HPA/ScaledObject events into deployment namespace/name keys.
	// If the deployment doesn't exist (e.g. HPA outlived it), there's nothing to do.
	var deployment v1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Find the PDB that matches this deployment's pod selector labels.
	// Only consider PDBs created by this controller (ownedBy annotation) — user-managed PDBs
	// should not be modified. If no controller-owned PDB exists, there's nothing to update.
	pdb, found, err := findPDBForDeployment(ctx, r.Client, &deployment, true)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !found {
		return reconcile.Result{}, nil
	}

	// Don't update PDB minAvailable during an active surge. The eviction controller
	// temporarily raises replicas (and possibly HPA/KEDA minReplicas) above the floor
	// to handle evictions. Updating the PDB now would lock in the surged value as the
	// new floor. The surge revert path will restore original values, and the next
	// reconcile after that will set minAvailable correctly.
	if _, surgeActive := deployment.Annotations[EvictionSurgeReplicasAnnotationKey]; surgeActive {
		logger.V(1).Info("Surge active on deployment, skipping PDB minAvailable update",
			"deployment", deployment.Name)
		return reconcile.Result{}, nil
	}

	// Resolve the autoscaler's minimum replica floor (KEDA minReplicaCount > HPA minReplicas).
	// If no autoscaler targets this deployment, the deployment controller owns PDB updates
	// and we bail out. We pass 0 as the fallback (unused when found==true).
	minAvailable, found, err := ResolveMinReplicas(ctx, r.Client, req.Namespace, req.Name, ResourceTypeDeployment, 0)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !found {
		logger.V(1).Info("No HPA/KEDA found for deployment, skipping PDB update",
			"deployment", req.Name)
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

// requeueDeploymentFromHPA maps an HPA event to the target deployment's reconcile request.
func requeueDeploymentFromHPA() handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
		if !ok {
			return nil
		}
		// Skip KEDA-managed HPAs — the ScaledObject watch handles those
		if isKEDAManagedHPA(hpa) {
			return nil
		}
		if !strings.EqualFold(hpa.Spec.ScaleTargetRef.Kind, ResourceTypeDeployment) {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: hpa.Namespace,
				Name:      hpa.Spec.ScaleTargetRef.Name,
			},
		}}
	}
}

// requeueDeploymentFromScaledObject maps a KEDA ScaledObject event to the target deployment.
func requeueDeploymentFromScaledObject() handler.TypedMapFunc[*unstructured.Unstructured, reconcile.Request] {
	return func(_ context.Context, obj *unstructured.Unstructured) []reconcile.Request {
		scaleTargetRef, found, err := unstructured.NestedMap(obj.Object, "spec", "scaleTargetRef")
		if err != nil || !found {
			return nil
		}
		name, _ := scaleTargetRef["name"].(string)
		if name == "" {
			return nil
		}
		kind, _ := scaleTargetRef["kind"].(string)
		if kind == "" {
			kind = ResourceTypeDeployment
		}
		if !strings.EqualFold(kind, ResourceTypeDeployment) {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      name,
			},
		}}
	}
}

// SetupWithManager registers watches on HPA and KEDA ScaledObject resources.
func (r *AutoscalerToPDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		// No primary "For" resource — this controller is driven entirely by watches.
		// We use a dummy source to satisfy the builder and rely on Watches below.
		Watches(&autoscalingv2.HorizontalPodAutoscaler{},
			handler.EnqueueRequestsFromMapFunc(requeueDeploymentFromHPA()))

	// Try to watch KEDA ScaledObjects (only if the CRD is installed)
	scaledObjectGVK := schema.GroupVersionKind{
		Group:   "keda.sh",
		Version: "v1alpha1",
		Kind:    "ScaledObject",
	}
	scaledObj := &unstructured.Unstructured{}
	scaledObj.SetGroupVersionKind(scaledObjectGVK)

	if err := r.discoverScaledObjectCRD(mgr); err == nil {
		builder = builder.WatchesRawSource(
			source.Kind(mgr.GetCache(), scaledObj,
				handler.TypedEnqueueRequestsFromMapFunc(requeueDeploymentFromScaledObject())))
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
