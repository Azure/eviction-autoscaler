package controllers

import (
	"context"
	"fmt"
	"strings"

	types "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
		deploymentName, e := r.getDeploymentName(pdb.Name)
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
					"createdBy": "PDBToPDBWatcherController",
					"target":    deploymentName,
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
				//should we create pdbwatchers for customer created pdbs?
				if _, err := r.getDeploymentName(e.Object.GetName()); err != nil {
					return false
				}
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// Handle delete event (this will be true for all delete events)
				return true
			},
		}).
		Owns(&types.PDBWatcher{}). // Watch PDBs for ownership
		Complete(r)
}

// getDeploymentName extracts deployment name form pdb name,
// will return err if pdbName doesn't contain "-pdb" as it's not created by PDBToPDBWatcherController
func (r *PDBToPDBWatcherReconciler) getDeploymentName(pdbName string) (string, error) {

	parts := strings.Split(pdbName, "-")

	// Check if the last part is "pdb"
	if len(parts) >= 2 && parts[len(parts)-1] == "pdb" {
		// Join the parts except the last one (the "pdb")
		extracted := strings.Join(parts[:len(parts)-1], "-")
		return extracted, nil
	} else {
		return "", fmt.Errorf("invalid format: string does not end with '-pdb'")
	}
}
