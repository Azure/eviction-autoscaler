package controllers

import (
	"context"
	"fmt"

	types "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
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

// PDBToPDBWatcherReconciler reconciles a PodDisruptionBudget object.
type PDBToPDBWatcherReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;create;watch

// Reconcile reads the state of the cluster for a PDB and creates/deletes PDBWatchers accordingly.
func (r *PDBToPDBWatcherReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// Fetch the PodDisruptionBudget object based on the reconcile request
	var pdb policyv1.PodDisruptionBudget
	err := r.Get(ctx, req.NamespacedName, &pdb)
	if err != nil {
		// If the PDB is deleted, we should delete the corresponding PDBWatcher.
		// First, check if the PDBWatcher exists
		var pdbWatcher types.PDBWatcher
		err := r.Get(ctx, req.NamespacedName, &pdbWatcher)
		if err == nil {
			// Delete PDBWatcher if PDB is deleted
			err := r.Delete(ctx, &pdbWatcher)
			if err != nil {
				return reconcile.Result{}, fmt.Errorf("unable to delete PDBWatcher: %v", err)
			}
			log.Log.Info("Deleted PDBWatcher", "name", req.NamespacedName)
		}
		return reconcile.Result{}, nil
	}

	// If the PDB exists, create a corresponding PDBWatcher if it does not exist
	var pdbWatcher types.PDBWatcher
	err = r.Get(ctx, req.NamespacedName, &pdbWatcher)

	if err != nil {
		deploymentName, e := r.discoverDeployment(ctx, &pdb)
		if e != nil {
			return reconcile.Result{}, e
		}

		// Create a new PDBWatcher
		pdbWatcher = types.PDBWatcher{
			TypeMeta: metav1.TypeMeta{
				Kind:       "PDBWatcher",
				APIVersion: "apps.mydomain.com/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      pdb.Name,
				Namespace: pdb.Namespace,
				Annotations: map[string]string{
					"controlledBy": "PDBToPDBWatcherController",
					"target":       deploymentName,
				},
			},
			Spec: types.PDBWatcherSpec{
				TargetName: deploymentName,
				TargetKind: deploymentKind,
			},
		}

		err := r.Create(ctx, &pdbWatcher)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("unable to create PDBWatcher: %v", err)
		}
		log.Log.Info("Created PDBWatcher", "name", pdb.Name)
	}

	// Return no error and no requeue
	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PDBToPDBWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Set up the controller to watch Deployments and trigger the reconcile function
	return ctrl.NewControllerManagedBy(mgr).
		For(&policyv1.PodDisruptionBudget{}).
		WithEventFilter(predicate.Funcs{
			// Only trigger for Create and Delete events
			CreateFunc: func(e event.CreateEvent) bool {
				// Handle create event (this will be true for all create events)
				//should we create pdbwatchers for customer created pdbs
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// Handle delete event (this will be true for all delete events)
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				//ToDo: theoretically you could have a pdb update and change
				// its label selectors in which case you might need to update the deployment target?
				return false
			},
		}).
		Owns(&types.PDBWatcher{}). // Watch PDBs for ownership
		Complete(r)
}

func (r *PDBToPDBWatcherReconciler) discoverDeployment(ctx context.Context, pdb *policyv1.PodDisruptionBudget) (string, error) {
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
		return "", fmt.Errorf("no pods found matching the PDB selector %s", pdb.Name)
	}

	// Iterate through each pod
	for _, pod := range podList.Items {
		logger.Info("Examining pod", "pod", pod.Name)

		// Check the OwnerReferences of each pod
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "ReplicaSet" {
				replicaSet := &appsv1.ReplicaSet{}
				err = r.Get(ctx, k8s_types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, replicaSet)
				if err != nil {
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
			}
			// Optional: Handle StatefulSets if necessary
			if ownerRef.Kind == "StatefulSet" {
				statefulSet := &appsv1.StatefulSet{}
				err = r.Get(ctx, k8s_types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, statefulSet)
				if err != nil {
					return "", fmt.Errorf("error fetching StatefulSet: %v", err)
				}
				logger.Info("Found StatefulSet owner", "statefulSet", statefulSet.Name)
				// Handle StatefulSet logic if required
			}
		}
	}

	return "", fmt.Errorf("PDB %s/%s overlaps with zero deployments", pdb.Namespace, pdb.Name)
}
