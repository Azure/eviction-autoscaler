package controllers

import (
	"context"
	"fmt"
	"strconv"

	myappsv1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	v1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

const deploymentCountedAnnotation = "eviction-autoscaler.azure.com/counted"

// Reconcile watches for Deployment changes (created, updated, deleted) and creates or deletes the associated PDB.
// creates pdb with minAvailable to be same as replicas for any deployment
func (r *DeploymentToPDBReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Deployment instance
	var deployment v1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Found: ", "deployment", deployment.Name, "namespace", deployment.Namespace)

	// check if deployment is being deleted:
	if !deployment.DeletionTimestamp.IsZero() {
		// Deployment is being deleted
		metrics.DecrementDeploymentCount(deployment.Namespace, metrics.CanCreatePDB)
		return reconcile.Result{}, nil
	}

	// If the Deployment is created, ensure a PDB exists
	return r.handleDeploymentReconcile(ctx, &deployment)
}

// handleDeploymentReconcile creates a PodDisruptionBudget when a Deployment is created or updated.
func (r *DeploymentToPDBReconciler) handleDeploymentReconcile(ctx context.Context, deployment *v1.Deployment) (reconcile.Result, error) {
	log := log.FromContext(ctx)
	
	// Check if we've already counted this deployment
	if _, counted := deployment.Annotations[deploymentCountedAnnotation]; !counted {
		// First time seeing this deployment (increment)
		metrics.IncrementDeploymentCount(deployment.Namespace, metrics.CanCreatePDB)
		
		// Mark it as counted
		if deployment.Annotations == nil {
			deployment.Annotations = make(map[string]string)
		}
		deployment.Annotations[deploymentCountedAnnotation] = "true"
		if err := r.Update(ctx, deployment); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Check if PDB already exists for this Deployment
	var pdbList policyv1.PodDisruptionBudgetList
	err := r.List(ctx, &pdbList, &client.ListOptions{
		Namespace: deployment.Namespace,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, pdb := range pdbList.Items {
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("error converting label selector: %w", err)
		}

		if selector.Matches(labels.Set(deployment.Spec.Template.Labels)) {
			// Track the PDB (check if it was created by us)
			createdByUs := metrics.PDBNotCreatedByUs
			if ann, ok := pdb.Annotations["createdBy"]; ok && ann == "DeploymentToPDBController" {
				createdByUs = metrics.PDBCreatedByUs
			}
			metrics.IncrementPDBCount(pdb.Namespace, createdByUs)

			// PDB already exists, nothing to do
			log.Info("PodDisruptionBudget already exists", "namespace", pdb.Namespace, "name", pdb.Name)
			EvictionAutoScaler := &myappsv1.EvictionAutoScaler{}
			e := r.Get(ctx, types.NamespacedName{Name: pdb.Name, Namespace: pdb.Namespace}, EvictionAutoScaler)
			if e == nil {
				// if pdb exists get EvictionAutoScaler --> compare targetGeneration field for deployment if both not same deployment was not changed by pdb watcher
				// update pdb minReplicas to current deployment replicas
				return r.updateMinAvailableAsNecessary(ctx, deployment, EvictionAutoScaler, pdb)
			}
			return reconcile.Result{}, nil
		}
	}

	//variables
	controller := true
	blockOwnerDeletion := true

	// Create a new PDB for the Deployment
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.generatePDBName(deployment.Name),
			Namespace: deployment.Namespace,
			Annotations: map[string]string{
				"createdBy": "DeploymentToPDBController",
				"target":    deployment.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "Deployment",
					Name:               deployment.Name,
					UID:                deployment.UID,
					Controller:         &controller,         // Mark as managed by this controller
					BlockOwnerDeletion: &blockOwnerDeletion, // Prevent deletion of the PDB until the deployment is deleted
				},
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{IntVal: *deployment.Spec.Replicas},
			Selector:     &metav1.LabelSelector{MatchLabels: deployment.Spec.Selector.MatchLabels},
		},
	}

	if err := r.Create(ctx, pdb); err != nil {
		return reconcile.Result{}, err
	}

	// Track PDB creation
	metrics.IncrementPDBCount(pdb.Namespace, metrics.PDBCreatedByUs)
	metrics.IncrementPDBCreationCount(deployment.Namespace, deployment.Name)

	log.Info("Created PodDisruptionBudget", "namespace", pdb.Namespace, "name", pdb.Name)
	return reconcile.Result{}, nil
}

func (r *DeploymentToPDBReconciler) updateMinAvailableAsNecessary(ctx context.Context,
	deployment *v1.Deployment, EvictionAutoScaler *myappsv1.EvictionAutoScaler, pdb policyv1.PodDisruptionBudget) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("deployment replicas got updated", " EvictionAutoScaler.Status.TargetGeneration", EvictionAutoScaler.Status.TargetGeneration, "deployment.Generation", deployment.GetGeneration())
	if EvictionAutoScaler.Status.TargetGeneration != deployment.GetGeneration() {
		//EvictionAutoScaler can fail between updating deployment and EvictionAutoScaler targetGeneration;
		//hence we need to rely on checking if annotation exists and compare with deployment.Spec.Replicas
		// no surge happened but customer already increased deployment replicas, then annotation would not exist
		if surgeReplicas, scaleUpAnnotationExists := deployment.Annotations[EvictionSurgeReplicasAnnotationKey]; scaleUpAnnotationExists {
			newReplicas, err := strconv.Atoi(surgeReplicas)
			if err != nil {
				logger.Error(err, "unable to parse surge replicas from annotation NOT updating",
					"namespace", deployment.Namespace, "name", deployment.Name, "replicas", surgeReplicas)
				return reconcile.Result{}, nil
			}

			if int32(newReplicas) == *deployment.Spec.Replicas {
				return reconcile.Result{}, nil
			}
		}
		//someone else changed deployment num of replicas
		pdb.Spec.MinAvailable = &intstr.IntOrString{IntVal: *deployment.Spec.Replicas}
		e := r.Update(ctx, &pdb)
		if e != nil {
			logger.Error(e, "unable to update pdb minAvailable to deployment replicas ",
				"namespace", pdb.Namespace, "name", pdb.Name, "replicas", *deployment.Spec.Replicas)
			return reconcile.Result{}, e
		}
		logger.Info("Successfully updated pdb minAvailable to deployment replicas ",
			"namespace", pdb.Namespace, "name", pdb.Name, "replicas", *deployment.Spec.Replicas)
	}
	return reconcile.Result{}, nil
}

func (r *DeploymentToPDBReconciler) generatePDBName(deploymentName string) string {
	return deploymentName
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentToPDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := mgr.GetLogger()
	// Set up the controller to watch Deployments and trigger the reconcile function
	// when controller restarts everything is seen as a create event
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Deployment{}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				//logger.Info("Update event detected, no action will be taken")
				//ToDo: distinguish scales from our EvictionAutoScaler from scales from other owners and keep minAvailable up near replicas.
				// Like if I start a deployment at 3 but then later say this is popular let me bump it to 5 should our pdb change.
				if oldDeployment, ok := e.ObjectOld.(*v1.Deployment); ok {
					newDeployment := e.ObjectNew.(*v1.Deployment)
					logger.Info("Update event detected, num of replicas changed", "newReplicas", newDeployment.Spec.Replicas)
					return oldDeployment.Spec.Replicas != newDeployment.Spec.Replicas
				}
				return false
				//return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
			},
		}).
		Owns(&policyv1.PodDisruptionBudget{}). // Watch PDBs for ownership
		Complete(r)
}
