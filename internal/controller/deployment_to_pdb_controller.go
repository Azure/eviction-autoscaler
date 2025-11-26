package controllers

import (
	"context"
	"fmt"
	"strconv"

	myappsv1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/go-logr/logr"
	"github.com/samber/lo"
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

const PDBCreateAnnotationKey = "eviction-autoscaler.azure.com/pdb-create"
const PDBCreateAnnotationFalse = "false"
const PDBCreateAnnotationTrue = "true"
const PDBOwnedByAnnotationKey = "ownedBy"
const ControllerName = "EvictionAutoScaler"
const ResourceTypeDeployment = "Deployment"

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
	// Fetch the Deployment instance
	var deployment v1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		// todo: decrement? DeploymentGauge when deployment not found
		// was deployment ever tracked? permanent vs temporary not found?
		return reconcile.Result{}, err
	}

	// check if deployment is being deleted:
	// if !deployment.DeletionTimestamp.IsZero() {
	// Deployment is being deleted
	//metrics.DeploymentGauge.WithLabelValues(deployment.Namespace, metrics.CanCreatePDBStr).Dec()
	//return reconcile.Result{}, nil
	//}

	log := log.FromContext(ctx)

	// Increment deployment count for metrics
	metrics.DeploymentGauge.WithLabelValues(deployment.Namespace, metrics.CanCreatePDBStr).Inc()

	// Check for pdb-create annotation on deployment
	if val, ok := deployment.Annotations[PDBCreateAnnotationKey]; ok {
		b, err := strconv.ParseBool(val)
		if err == nil && !b {
			return reconcile.Result{}, nil
		}
		// Only "false" is supported, log a warning for any other value
		log.Error(fmt.Errorf("Unsupported value for pdb-create annotation, only 'false' is supported"), "value", val)
		return reconcile.Result{}, fmt.Errorf("unsupported value for pdb-create annotation: %s, only 'false' is supported", val)
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
			// PDB already exists, nothing to do
			EvictionAutoScaler := &myappsv1.EvictionAutoScaler{}
			err := r.Get(ctx, types.NamespacedName{Name: pdb.Name, Namespace: pdb.Namespace}, EvictionAutoScaler)
			if err != nil {
				//TODO don't ignore not found. Retry and fix unittest DeploymentToPDBReconciler when a deployment is created [It] should not create a PodDisruptionBudget if one already matches
				return reconcile.Result{}, client.IgnoreNotFound(err)
			}
			// if pdb exists get EvictionAutoScaler --> compare targetGeneration field for deployment if both not same deployment was not changed by pdb watcher
			// update pdb minReplicas to current deployment replicas
			return reconcile.Result{}, r.updateMinAvailableAsNecessary(ctx, &deployment, EvictionAutoScaler, pdb)
		}
	}
	fmt.Printf("Creating pdb: %s", deployment.Name)
	//variables
	controller := true
	blockOwnerDeletion := true

	// Create a new PDB for the Deployment
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.generatePDBName(deployment.Name),
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

	// Track PDB creation event
	metrics.PDBCreationCounter.WithLabelValues(deployment.Namespace, deployment.Name).Inc()

	log.Info("Created PodDisruptionBudget", "namespace", pdb.Namespace, "name", pdb.Name)
	return reconcile.Result{}, nil
}

func (r *DeploymentToPDBReconciler) updateMinAvailableAsNecessary(ctx context.Context,
	deployment *v1.Deployment, EvictionAutoScaler *myappsv1.EvictionAutoScaler, pdb policyv1.PodDisruptionBudget) error {
	logger := log.FromContext(ctx)

	// Check if PDB has the ownedBy annotation - if not, skip updates (user owns it)
	hasAnnotation := pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName

	if !hasAnnotation {
		logger.Info("Skipping PDB update - not owned by DeploymentToPDBController",
			"namespace", pdb.Namespace, "name", pdb.Name)
		return nil
	}

	if EvictionAutoScaler.Status.TargetGeneration != deployment.GetGeneration() {
		//EvictionAutoScaler can fail between updating deployment and EvictionAutoScaler targetGeneration;
		//hence we need to rely on checking if annotation exists and compare with deployment.Spec.Replicas
		// no surge happened but customer already increased deployment replicas, then annotation would not exist
		if surgeReplicas, scaleUpAnnotationExists := deployment.Annotations[EvictionSurgeReplicasAnnotationKey]; scaleUpAnnotationExists {
			newReplicas, err := strconv.Atoi(surgeReplicas)
			if err != nil {
				logger.Error(err, "unable to parse surge replicas from annotation NOT updating",
					"namespace", deployment.Namespace, "name", deployment.Name, "replicas", surgeReplicas)
				return err
			}

			if int32(newReplicas) == *deployment.Spec.Replicas {
				return nil
			}
		}
		//someone else changed deployment num of replicas
		pdb.Spec.MinAvailable = &intstr.IntOrString{IntVal: *deployment.Spec.Replicas}
		err := r.Update(ctx, &pdb)
		if err != nil {
			logger.Error(err, "unable to update pdb minAvailable to deployment replicas ",
				"namespace", pdb.Namespace, "name", pdb.Name, "replicas", *deployment.Spec.Replicas)
			return err
		}
		logger.Info("Successfully updated pdb minAvailable to deployment replicas ",
			"namespace", pdb.Namespace, "name", pdb.Name, "replicas", *deployment.Spec.Replicas)
	}
	return nil
}

func (r *DeploymentToPDBReconciler) generatePDBName(deploymentName string) string {
	return deploymentName
}

// triggerOnReplicaChange checks if a deployment update event should trigger reconciliation
// by comparing the number of replicas between old and new deployment
func triggerOnReplicaChange(e event.UpdateEvent, logger logr.Logger) bool {
	if oldDeployment, ok := e.ObjectOld.(*v1.Deployment); ok {
		newDeployment := e.ObjectNew.(*v1.Deployment)
		if lo.FromPtr(oldDeployment.Spec.Replicas) != lo.FromPtr(newDeployment.Spec.Replicas) {
			logger.Info("Update event detected, num of replicas changed",
				"newReplicas", lo.FromPtr(newDeployment.Spec.Replicas),
				"oldReplicas", lo.FromPtr(oldDeployment.Spec.Replicas))
			return true
		}
	}
	return false
}

// triggerOnAnnotationChange checks if a deployment update event should trigger reconciliation
// by comparing the annotations between old and new deployment

// triggerOnAnnotationChange checks if a deployment update event should trigger reconciliation
// by comparing the annotations between old and new deployment
func triggerOnAnnotationChange(e event.UpdateEvent, logger logr.Logger) bool {
	oldDeployment, okOld := e.ObjectOld.(*v1.Deployment)
	newDeployment, okNew := e.ObjectNew.(*v1.Deployment)
	if okOld && okNew {
		oldVal := oldDeployment.Annotations[PDBCreateAnnotationKey]
		newVal := newDeployment.Annotations[PDBCreateAnnotationKey]
		if oldVal != newVal {
			logger.Info("Update event detected, annotation value changed",
				"oldValue", oldVal, "newValue", newVal)
			return true
		}
	}
	return false
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
				return (triggerOnReplicaChange(e, logger) || triggerOnAnnotationChange(e, logger))
			},
			DeleteFunc: func(e event.DeleteEvent) bool { return false },
		}).
		Owns(&policyv1.PodDisruptionBudget{}). // Watch PDBs for ownership
		Complete(r)
}
