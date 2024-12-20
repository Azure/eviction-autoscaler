package controllers

import (
	"context"

	v1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
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
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;update;watch

// Reconcile watches for Deployment changes (created, updated, deleted) and creates or deletes the associated PDB.
// creates pdb with minAvailable to be same as replicas for any deployment
func (r *DeploymentToPDBReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Deployment instance
	var deployment v1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		log.Error(err, "unable to fetch Deployment")
		if apierrors.IsNotFound(err) {
			e := r.handleDeploymentDeletion(ctx, req)
			return ctrl.Result{}, client.IgnoreNotFound(e)
		}
		return reconcile.Result{}, err
	}
	log.Info("Found: ", "deployment", deployment.Name, "namespace", deployment.Namespace)
	// If the Deployment is created, ensure a PDB exists
	return r.handleDeploymentReconcile(ctx, &deployment)
}

// handleDeploymentReconcile creates a PodDisruptionBudget when a Deployment is created or updated.
func (r *DeploymentToPDBReconciler) handleDeploymentReconcile(ctx context.Context, deployment *v1.Deployment) (reconcile.Result, error) {
	log := log.FromContext(ctx)
	// Check if PDB already exists for this Deployment
	pdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: deployment.Namespace,
		Name:      r.generatePDBName(deployment.Name),
	}, pdb)

	if err == nil {
		// PDB already exists, nothing to do
		log.Info("PodDisruptionBudget already exists", "namespace", deployment.Namespace, "name", deployment.Name)
		//if pdb.Spec.MinAvailable.IntVal != *deployment.Spec.Replicas {
		//	pdb.Spec.MinAvailable.IntVal = *deployment.Spec.Replicas
		//	if err := r.Update(ctx, pdb); err != nil {
		//		return reconcile.Result{}, err
		//	}
		//}
		return reconcile.Result{}, nil
	}

	// Create a new PDB for the Deployment
	pdb = &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.generatePDBName(deployment.Name),
			Namespace: deployment.Namespace,
			Annotations: map[string]string{
				"createdBy": "DeploymentToPDBController",
				"target":    deployment.Name,
			},
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

func (r *DeploymentToPDBReconciler) generatePDBName(deploymentName string) string {
	return deploymentName
}

// handleDeploymentDeletion deletes the associated PDB when the Deployment is deleted
// we can leak here if controller stops working
// Ironically leaking pdbs would block our current upgrade logic we had to toggle off wher expected pods == 0
func (r *DeploymentToPDBReconciler) handleDeploymentDeletion(ctx context.Context, req ctrl.Request) error {
	log := log.FromContext(ctx)

	// Try to get the associated PDB
	pdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: req.NamespacedName.Namespace,
		Name:      r.generatePDBName(req.NamespacedName.Name),
	}, pdb)
	//ToDo: use pdb.DeletionTimestamp instead to determine if pdb got deleted
	if err != nil {
		// If there's no PDB or it can't be fetched, just return
		log.Info("PodDisruptionBudget does not exist or error fetching", "namespace", req.NamespacedName.Namespace, "name", req.NamespacedName.Name)
		return err
	}

	// ToDo: only delete if it has createdby/ownerref
	// If the PDB exists, delete it
	log.Info("Deleting PodDisruptionBudget", "namespace", pdb.Namespace, "name", pdb.Name)
	if err := r.Delete(ctx, pdb); err != nil {
		log.Error(err, "unable to delete PDB")
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentToPDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := mgr.GetLogger()
	// Set up the controller to watch Deployments and trigger the reconcile function
	// when controller restarts everything is seen as a create event
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Deployment{}).
		WithEventFilter(predicate.Funcs{
			// Only trigger for Create and Delete events
			CreateFunc: func(e event.CreateEvent) bool {
				logger.Info("Create event detected, pdb will be created if not exists")
				// Handle create event (this will be true for all create events)
				//creationTime, _ := time.Parse(time.RFC3339, e.Object.GetCreationTimestamp().String())
				//if time.Since(creationTime) < 5*time.Minute {
				//	return false // Ignore create if it's an existing resource (e.g., by checking timestamp)
				//}
				return true // Allow to create for new resources
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				logger.Info("Delete event detected, pdb will be deleted if exists")
				// Handle delete event (this will be true for all delete events)
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				logger.Info("Update event detected, no action will be taken")
				// No need to handle update event
				//ToDo: distinguish scales from our pdbwatcher from scales from other owners and keep minAvailable up near replicas.
				// Like if I start a deployment at 3 but then later say this is popular let me bump it to 5 should our pdb change.
				//oldDeployment := e.ObjectOld.(*v1.Deployment)
				//newDeployment := e.ObjectNew.(*v1.Deployment)
				//return oldDeployment.Spec.Replicas != newDeployment.Spec.Replicas
				return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
			},
		}).
		Owns(&policyv1.PodDisruptionBudget{}). // Watch PDBs for ownership
		Complete(r)
}
