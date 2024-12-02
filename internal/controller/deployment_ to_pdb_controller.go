package controllers

import (
	"context"

	"k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DeploymentToPDBReconciler reconciles a Deployment object and ensures an associated PDB is created and deleted
type DeploymentToPDBReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile watches for Deployment changes (created, updated, deleted) and creates or deletes the associated PDB.
func (r *DeploymentToPDBReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Deployment instance
	var deployment v1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		log.Error(err, "unable to fetch Deployment")
		return ctrl.Result{}, client.IgnoreNotFound(err) // Ignore if the deployment is not found (i.e., deleted)
	}

	// If the Deployment is deleted, ensure the associated PDB is also deleted
	if deployment.DeletionTimestamp != nil {
		return r.handleDeploymentDeletion(ctx, &deployment)
	}

	// If the Deployment is created or updated, ensure a PDB exists
	return r.handleDeploymentCreation(ctx, &deployment)
}

// handleDeploymentCreation creates a PodDisruptionBudget when a Deployment is created or updated.
func (r *DeploymentToPDBReconciler) handleDeploymentCreation(ctx context.Context, deployment *v1.Deployment) (reconcile.Result, error) {
	log := log.FromContext(ctx)
	// Check if PDB already exists for this Deployment
	pdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: deployment.Namespace,
		Name:      deployment.Name + "-pdb",
	}, pdb)

	if err == nil {
		// PDB already exists, nothing to do
		log.Info("PodDisruptionBudget already exists", "namespace", deployment.Namespace, "name", deployment.Name)
		return reconcile.Result{}, nil
	} else if client.IgnoreNotFound(err) != nil {
		// If there was an error fetching the PDB (other than NotFound), log and return
		log.Error(err, "unable to fetch PDB")
		return ctrl.Result{}, err
	}

	// Create a new PDB for the Deployment
	pdb = &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PodDisruptionBudget",
			APIVersion: "policy/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.Name + "-pdb",
			Namespace: deployment.Namespace,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{IntVal: *deployment.Spec.Replicas},
			Selector:     &metav1.LabelSelector{MatchLabels: deployment.Spec.Selector.MatchLabels},
		},
	}

	log.Info("Creating PodDisruptionBudget", "namespace", pdb.Namespace, "name", pdb.Name)
	if err := r.Create(ctx, pdb); err != nil {
		log.Error(err, "unable to create PDB")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// handleDeploymentDeletion deletes the associated PDB when the Deployment is deleted
func (r *DeploymentToPDBReconciler) handleDeploymentDeletion(ctx context.Context, deployment *v1.Deployment) (reconcile.Result, error) {
	log := log.FromContext(ctx)

	// Try to get the associated PDB
	pdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: deployment.Namespace,
		Name:      deployment.Name + "-pdb",
	}, pdb)

	if err != nil {
		// If there's no PDB or it can't be fetched, just return
		log.Info("PodDisruptionBudget does not exist or error fetching", "namespace", deployment.Namespace, "name", deployment.Name)
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// If the PDB exists, delete it
	log.Info("Deleting PodDisruptionBudget", "namespace", pdb.Namespace, "name", pdb.Name)
	if err := r.Delete(ctx, pdb); err != nil {
		log.Error(err, "unable to delete PDB")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentToPDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Set up the controller to watch Deployments and trigger the reconcile function
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Deployment{}).
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
		Owns(&policyv1.PodDisruptionBudget{}). // Watch PDBs for ownership
		Complete(r)
}
