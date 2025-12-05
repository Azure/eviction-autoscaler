package controllers

import (
	"context"
	"strconv"

	myappsv1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/go-logr/logr"
	"github.com/samber/lo"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const PDBCreateAnnotationKey = "eviction-autoscaler.azure.com/pdb-create"
const PDBCreateAnnotationFalse = "false"
const PDBCreateAnnotationTrue = "true"
const EnableEvictionAutoscalerAnnotationKey = "eviction-autoscaler.azure.com/enable"
const EnableEvictionAutoscalerTrue = "true"
const PDBOwnedByAnnotationKey = "ownedBy"
const ControllerName = "EvictionAutoScaler"
const ResourceTypeDeployment = "Deployment"

// DeploymentToPDBReconciler reconciles a Deployment object and ensures an associated PDB is created and deleted
type DeploymentToPDBReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	EnableAll bool // If true, enable for all namespaces by default (opt-out mode)
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;update;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

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

	// Check if eviction autoscaler should be enabled
	// Enable by default in kube-system namespace, otherwise check annotation on the namespace
	isEnabled, err := IsEvictionAutoscalerEnabled(ctx, r.Client, deployment.Namespace, r.EnabledByDefault, r.ActionedNamespaces)
	if err != nil {
		log.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", deployment.Namespace)
		return reconcile.Result{}, err
	}
	if !isEnabled {
		log.V(1).Info("Eviction autoscaler not enabled for namespace", "namespace", deployment.Namespace)
		// Clean up PDB if it exists and was created by this controller
		// EvictionAutoScaler will be cascade deleted automatically via ownerReference
		pdb, found, err := FindPDBForDeployment(ctx, r.Client, &deployment)
		if err != nil {
			return reconcile.Result{}, err
		}
		if found && pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName {
			log.Info("Deleting PDB for deployment in disabled namespace (EvictionAutoScaler will be cascade deleted)", "pdb", pdb.Name)
			if err := r.Delete(ctx, pdb); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	// Check if PDB creation should be skipped for this deployment
	if shouldSkip, reason := ShouldSkipPDBCreation(&deployment); shouldSkip {
		log.Info("Skipping PDB creation for deployment", "deployment", deployment.Name,
			"namespace", deployment.Namespace, "reason", reason)
		return reconcile.Result{}, nil
	}

	// Check if PDB already exists for this Deployment
	pdb, found, err := FindPDBForDeployment(ctx, r.Client, &deployment)
	if err != nil {
		return ctrl.Result{}, err
	}

	if found {
		// PDB already exists, check for EvictionAutoScaler and update if needed
		EvictionAutoScaler := &myappsv1.EvictionAutoScaler{}
		err := r.Get(ctx, types.NamespacedName{Name: pdb.Name, Namespace: pdb.Namespace}, EvictionAutoScaler)
		if err != nil {
			//TODO don't ignore not found. Retry and fix unittest DeploymentToPDBReconciler when a deployment is created [It] should not create a PodDisruptionBudget if one already matches
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		// if pdb exists get EvictionAutoScaler --> compare targetGeneration field for deployment if both not same deployment was not changed by pdb watcher
		// update pdb minReplicas to current deployment replicas
		return reconcile.Result{}, r.updateMinAvailableAsNecessary(ctx, &deployment, EvictionAutoScaler, *pdb)
	}

	// Create a new PDB for the Deployment using helper function
	if err := CreatePDBForDeployment(ctx, r.Client, &deployment); err != nil {
		return reconcile.Result{}, err
	}

	// Track PDB creation event
	metrics.PDBCreationCounter.WithLabelValues(deployment.Namespace, deployment.Name).Inc()

	log.Info("Created PodDisruptionBudget", "namespace", deployment.Namespace, "name", deployment.Name)
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

// EnqueueDeploymentsInNamespace enqueues all deployments in a namespace when namespace annotation changes
type EnqueueDeploymentsInNamespace struct {
	Client    client.Client
	EnableAll bool
}

// Create handles namespace create events (no-op)
func (e *EnqueueDeploymentsInNamespace) Create(ctx context.Context, evt event.CreateEvent, q handler.Queue) {
	// Don't enqueue on namespace creation
}

// Update handles namespace update events and enqueues all deployments in that namespace
func (e *EnqueueDeploymentsInNamespace) Update(ctx context.Context, evt event.UpdateEvent, q handler.Queue) {
	logger := log.FromContext(ctx)

	oldNs, okOld := evt.ObjectOld.(*corev1.Namespace)
	newNs, okNew := evt.ObjectNew.(*corev1.Namespace)
	if !okOld || !okNew {
		return
	}

	// Skip kube-system namespace
	if newNs.Name == metav1.NamespaceSystem {
		return
	}

	// Check if enable annotation changed
	oldVal := ""
	newVal := ""
	if oldNs.Annotations != nil {
		oldVal = oldNs.Annotations[EnableEvictionAutoscalerAnnotationKey]
	}
	if newNs.Annotations != nil {
		newVal = newNs.Annotations[EnableEvictionAutoscalerAnnotationKey]
	}

	// Only trigger if annotation changed
	// In opt-in mode: enabled if annotation is "true"
	// In opt-out mode: enabled unless annotation is "false"
	wasEnabled := (e.EnableAll && oldVal != "false") || (!e.EnableAll && oldVal == EnableEvictionAutoscalerTrue)
	isEnabled := (e.EnableAll && newVal != "false") || (!e.EnableAll && newVal == EnableEvictionAutoscalerTrue)

	if wasEnabled == isEnabled {
		return // No change in enabled state
	}

	logger.Info("Namespace annotation changed, enqueuing all deployments",
		"namespace", newNs.Name, "wasEnabled", wasEnabled, "isEnabled", isEnabled)

	// List all deployments in the namespace
	var deploymentList v1.DeploymentList
	if err := e.Client.List(ctx, &deploymentList, client.InNamespace(newNs.Name)); err != nil {
		logger.Error(err, "Failed to list deployments in namespace", "namespace", newNs.Name)
		return
	}

	// Enqueue each deployment for reconciliation
	for _, deployment := range deploymentList.Items {
		q.Add(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      deployment.Name,
				Namespace: deployment.Namespace,
			},
		})
	}

	logger.Info("Enqueued deployments for reconciliation", "namespace", newNs.Name, "count", len(deploymentList.Items))
}

// Delete handles namespace delete events (no-op, cascade deletion handles cleanup)
func (e *EnqueueDeploymentsInNamespace) Delete(ctx context.Context, evt event.DeleteEvent, q handler.Queue) {
	// Namespace deletion will cascade delete all resources
}

// Generic handles generic events (no-op)
func (e *EnqueueDeploymentsInNamespace) Generic(ctx context.Context, evt event.GenericEvent, q handler.Queue) {
	// Not used
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentToPDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := mgr.GetLogger()
	// Set up the controller to watch Deployments and trigger the reconcile function
	// when controller restarts everything is seen as a create event
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Deployment{}).
		Watches(&corev1.Namespace{}, &EnqueueDeploymentsInNamespace{Client: r.Client, EnableAll: r.EnableAll}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return (triggerOnReplicaChange(e, logger) || triggerOnAnnotationChange(e, logger))
			},
			DeleteFunc: func(e event.DeleteEvent) bool { return false },
		}).
		Owns(&policyv1.PodDisruptionBudget{}). // Watch PDBs for ownership
		Complete(r)
}
