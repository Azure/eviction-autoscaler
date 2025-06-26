package controllers

import (
	"context"
	"fmt"

	types "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8s_types "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var errOwnerNotFound error = fmt.Errorf("owner not found")

// PDBToEvictionAutoScalerReconciler reconciles a PodDisruptionBudget object.
type PDBToEvictionAutoScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;create;watch;update
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;update;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;update;watch

// Reconcile reads the state of the cluster for a PDB and creates/deletes EvictionAutoScalers accordingly.
func (r *PDBToEvictionAutoScalerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.WithValues("pdb", req.Name, "namespace", req.Namespace)
	ctx = log.IntoContext(ctx, logger)
	// Fetch the PodDisruptionBudget object based on the reconcile request
	var pdb policyv1.PodDisruptionBudget
	err := r.Get(ctx, req.NamespacedName, &pdb)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Update PDB metrics - check if this PDB was created by our deployment controller
	createdByUs := false
	if annotations := pdb.GetAnnotations(); annotations != nil {
		if createdBy, exists := annotations["createdBy"]; exists && createdBy == "DeploymentToPDBController" {
			createdByUs = true
		}
	}
	metrics.UpdatePDBCount(pdb.Namespace, createdByUs, 1)

	// If the PDB exists, create a corresponding EvictionAutoScaler if it does not exist
	var EvictionAutoScaler types.EvictionAutoScaler
	err = r.Get(ctx, req.NamespacedName, &EvictionAutoScaler)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		deploymentName, e := r.discoverDeployment(ctx, &pdb)
		if e != nil {
			if e == errOwnerNotFound {
				return reconcile.Result{}, nil
			}
			return reconcile.Result{}, e
		}

		//variables
		controller := true
		blockOwnerDeletion := true

		// Create a new EvictionAutoScaler
		EvictionAutoScaler = types.EvictionAutoScaler{
			TypeMeta: metav1.TypeMeta{
				Kind:       "EvictionAutoScaler",
				APIVersion: "eviction-autoscaler.azure.com/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      pdb.Name,
				Namespace: pdb.Namespace,
				Annotations: map[string]string{
					"createdBy": "PDBToEvictionAutoScalerController",
					"target":    deploymentName,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "policy/v1",
						Kind:               "PodDisruptionBudget",
						Name:               pdb.Name,
						UID:                pdb.UID,
						Controller:         &controller,         // Mark as managed by this controller
						BlockOwnerDeletion: &blockOwnerDeletion, // Prevent deletion of the EvictionAutoScaler until the controller is deleted
					},
				},
			},
			Spec: types.EvictionAutoScalerSpec{
				TargetName: deploymentName,
				TargetKind: deploymentKind,
			},
		}

		err := r.Create(ctx, &EvictionAutoScaler)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("unable to create EvictionAutoScaler: %v", err)
		}

		// Track EvictionAutoScaler creation
		metrics.IncrementEvictionAutoScalerCreationCount(pdb.Namespace, pdb.Name, deploymentName)

		logger.Info("Created EvictionAutoScaler")
	}
	// Return no error and no requeue
	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PDBToEvictionAutoScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Set up the controller to watch Deployments and trigger the reconcile function
	return ctrl.NewControllerManagedBy(mgr).
		For(&policyv1.PodDisruptionBudget{}).
		WithEventFilter(predicate.Funcs{
			// Only trigger for Create and Delete events
			UpdateFunc: func(e event.UpdateEvent) bool {
				//ToDo: theoretically you could have a pdb update and change
				// its label selectors in which case you might need to update the deployment target?
				return false
			},
		}).
		Owns(&types.EvictionAutoScaler{}). // Watch EvictionAutoScalers for ownership
		Complete(r)
}

func (r *PDBToEvictionAutoScalerReconciler) discoverDeployment(ctx context.Context, pdb *policyv1.PodDisruptionBudget) (string, error) {
	logger := log.FromContext(ctx)

	// Convert PDB label selector to Kubernetes selector
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return "", fmt.Errorf("error converting label selector: %v", err)
	}
	logger.Info("PDB Selector", "selector", pdb.Spec.Selector)

	podList := &corev1.PodList{}
	err = r.List(ctx, podList, &client.ListOptions{Namespace: pdb.Namespace, LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("error listing pods: %v", err)
	}
	logger.Info("Number of pods found", "count", len(podList.Items))

	if len(podList.Items) == 0 {
		return "", fmt.Errorf("no pods found matching the PDB selector %s; leaky pdb(?!)", pdb.Name)
	}

	// Iterate through each pod
	for _, pod := range podList.Items {
		// Check the OwnerReferences of each pod
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "ReplicaSet" {
				replicaSet := &appsv1.ReplicaSet{}
				err = r.Get(ctx, k8s_types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, replicaSet)
				if apierrors.IsNotFound(err) {
					return "", fmt.Errorf("error fetching ReplicaSet: %v", err)
				}

				// Log ReplicaSet details
				logger.Info("Found ReplicaSet", "replicaSet", replicaSet.Name)

				// Look for the Deployment owner of the ReplicaSet
				for _, rsOwnerRef := range replicaSet.OwnerReferences {
					if rsOwnerRef.Kind == "Deployment" {
						logger.Info("Found Deployment owner", "deployment", rsOwnerRef.Name)
						return rsOwnerRef.Name, nil
					}
				}
				// no replicaset owner just move on and see if any other pods have have something.
			}
			//// Optional: Handle StatefulSets if necessary
			//if ownerRef.Kind == "StatefulSet" {
			//	statefulSet := &appsv1.StatefulSet{}
			//	err = r.Get(ctx, k8s_types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, statefulSet)
			//	if apierrors.IsNotFound(err) {
			//		return "", fmt.Errorf("error fetching StatefulSet: %v", err)
			//	}
			//	logger.Info("Found StatefulSet owner", "statefulSet", statefulSet.Name)
			//	// Handle StatefulSet logic if required
			//}

		}
	}
	logger.Info("No Deployment owner found")
	return "", errOwnerNotFound
}
