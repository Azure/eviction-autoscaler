package controllers

import (
	"context"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// PDBWatcher is a custom resource representing a PDBWatcher.
type PDBWatcher struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PDBWatcherSpec   `json:"spec,omitempty"`
	Status            PDBWatcherStatus `json:"status,omitempty"`
}

// ToDo: Implement this?
func (P PDBWatcher) DeepCopyObject() runtime.Object {
	return nil
}

// PDBWatcherSpec defines the desired state of PDBWatcher
type PDBWatcherSpec struct {
	Name string `json:"name"`
}

// PDBWatcherStatus defines the observed state of PDBWatcher
type PDBWatcherStatus struct {
	Status string `json:"status"`
}

// PDBWatcherReconciler reconciles a PodDisruptionBudget object.
type PDBWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile reads the state of the cluster for a PDB and creates/deletes PDBWatchers accordingly.
func (r *PDBWatcherReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// Fetch the PodDisruptionBudget object based on the reconcile request
	var pdb policyv1.PodDisruptionBudget
	err := r.Get(ctx, req.NamespacedName, &pdb)
	if err != nil {
		// If the PDB is deleted, we should delete the corresponding PDBWatcher.
		// First, check if the PDBWatcher exists
		var pdbWatcher PDBWatcher
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
	var pdbWatcher PDBWatcher
	err = r.Get(ctx, req.NamespacedName, &pdbWatcher)
	if err != nil {
		// Create a new PDBWatcher
		pdbWatcher = PDBWatcher{
			TypeMeta: metav1.TypeMeta{
				Kind:       "PDBWatcher",
				APIVersion: "apps.mydomain.com/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      pdb.Name,
				Namespace: pdb.Namespace,
			},
			Spec: PDBWatcherSpec{
				Name: pdb.Name,
			},
			Status: PDBWatcherStatus{
				Status: "created",
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
func (r *PDBWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Set up the controller to watch Deployments and trigger the reconcile function
	return ctrl.NewControllerManagedBy(mgr).
		For(&policyv1.PodDisruptionBudget{}).
		WithEventFilter(predicate.Funcs{
			// Only trigger for Create and Delete events
			CreateFunc: func(e event.CreateEvent) bool {
				// Handle create event (this will be true for all create events)
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// Handle delete event (this will be true for all delete events)
				return true
			},
		}).
		Owns(&PDBWatcher{}). // Watch PDBs for ownership
		Complete(r)
}
