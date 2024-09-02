package controllers

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	myappsv1 "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
)

// PDBWatcherReconciler reconciles a PDBWatcher object
type PDBWatcherReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

func (r *PDBWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PDBWatcher instance
	pdbWatcher := &myappsv1.PDBWatcher{}
	err := r.Get(ctx, req.NamespacedName, pdbWatcher)
	if err != nil {
		//should we use a finalizer to scale back down on deletion?
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil // PDBWatcher not found, could be deleted, nothing to do
		}
		return ctrl.Result{}, err // Error fetching PDBWatcher
	}

	//only do this on create? kill off by sharing name?
	if err := r.conflicts(ctx, pdbWatcher); err != nil {
		return ctrl.Result{}, err
	}

	// Fetch the PDB
	pdb := &policyv1.PodDisruptionBudget{}
	err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Spec.PDBName, Namespace: pdbWatcher.Namespace}, pdb)
	if err != nil {
		//better error on notfound
		return ctrl.Result{}, err // Error fetching PDB
	}

	deploymentName := pdbWatcher.Spec.TargetName
	if deploymentName == "" {
		deploymentName, err := r.discoverDeployment(ctx, pdb)
		if err != nil {
			//better error on notfound
			return ctrl.Result{}, err // Error fetching PDB
		}
		pdbWatcher.Spec.TargetName = deploymentName
		pdbWatcher.Spec.TargetType = "Deployment"
		err = r.Update(ctx, pdbWatcher)
		if err != nil {
			logger.Error(err, "Failed to update PDBWatcher deployment")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil

	}

	// Fetch the Deployment
	deployment := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: pdbWatcher.Namespace}, deployment)
	if err != nil {
		if errors.IsNotFound(err) {
			//blank out deployment and try again?
			pdbWatcher.Spec.TargetName = ""
			err = r.Update(ctx, pdbWatcher)
			if err != nil {
				logger.Error(err, "Failed to clear PDBWatcher deployment")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // Error fetching Deployment
	}

	// Check if the resource version has changed or if it's empty (initial state)
	if pdbWatcher.Status.TargetGeneration == 0 || pdbWatcher.Status.TargetGeneration != deployment.GetGeneration() {
		logger.Info("Deployment resource version changed reseting min replicas")
		// The resource version has changed, which means someone else has modified the Deployment.
		// To avoid conflicts, we update our status to reflect the new state and avoid making further changes.
		pdbWatcher.Status.TargetGeneration = deployment.GetGeneration()
		pdbWatcher.Status.MinReplicas = *deployment.Spec.Replicas
		err = r.Status().Update(ctx, pdbWatcher)
		if err != nil {
			logger.Error(err, "Failed to update PDBWatcher status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Log current state before checks
	logger.Info(fmt.Sprintf("Checking PDB for %s: DisruptionsAllowed=%d, MinReplicas=%d", pdb.Name, pdb.Status.DisruptionsAllowed, pdbWatcher.Status.MinReplicas))

	// Check the DisruptionsAllowed field
	if pdb.Status.DisruptionsAllowed == 0 {
		// Check if there are recent evictions

		if recentEviction(ctx, *pdbWatcher) {
			//What if the evict went through because the pod being evicted wasn't ready anyways? Handle that in webhook or here?

			// Handle nil Deployment Strategy and MaxSurge
			logger.Info(fmt.Sprintf("No disruptions allowed for %s and recent eviction attempting to scale up", pdb.Name))
			// Scale up the Deployment
			newReplicas := calculateSurge(ctx, deployment, pdbWatcher.Status.MinReplicas)
			deployment.Spec.Replicas = &newReplicas
			err = r.Update(ctx, deployment)
			if err != nil {
				logger.Error(err, "failed to update deployment")
				return ctrl.Result{}, err
			}

			// Save ResourceVersion to PDBWatcher status this will cause another reconcile.
			pdbWatcher.Status.TargetGeneration = deployment.GetGeneration()
			pdbWatcher.Status.LastEviction = pdbWatcher.Spec.LastEviction //we could still keep a log here if thats useful
			//should we clear evictions?
			err = r.Status().Update(ctx, pdbWatcher)
			if err != nil {
				logger.Error(err, "Failed to update PDBWatcher status")
				return ctrl.Result{}, err
			}

			// Log the scaling action
			logger.Info(fmt.Sprintf("Scaled up Deployment %s/%s to %d replicas", deployment.Namespace, deployment.Name, newReplicas))
			return ctrl.Result{}, nil
		}

		logger.Info("No recent reconcile event", "pdbname", pdb.Name)
		return ctrl.Result{}, nil
	}

	// Watch for changes in PDB to revert to original state
	if *deployment.Spec.Replicas != pdbWatcher.Status.MinReplicas {

		// Revert Deployment to the original state
		deployment.Spec.Replicas = &pdbWatcher.Status.MinReplicas
		err = r.Update(ctx, deployment)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Log the scaling action
		logger.Info(fmt.Sprintf("Reverted Deployment %s/%s to %d replicas", deployment.Namespace, deployment.Name, *deployment.Spec.Replicas))

		// Update ResourceVersion in PDBWatcher status
		pdbWatcher.Status.TargetGeneration = deployment.GetGeneration()
		err = r.Status().Update(ctx, pdbWatcher)
		if err != nil {
			logger.Error(err, "Failed to update PDBWatcher status")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// does this go away if we force the pdb watcher name to be same as pdb?
func (r *PDBWatcherReconciler) conflicts(ctx context.Context, pdbWatcher *myappsv1.PDBWatcher) error {
	// Check for conflicts with other PDBWatchers
	conflictWatcherList := &myappsv1.PDBWatcherList{}
	err := r.List(ctx, conflictWatcherList, &client.ListOptions{Namespace: pdbWatcher.Namespace})
	if err != nil {
		return err // Error listing PDBWatchers
	}

	for _, watcher := range conflictWatcherList.Items {
		if watcher.Name != pdbWatcher.Name && watcher.Spec.PDBName == pdbWatcher.Spec.PDBName {
			// Conflict detected
			err := fmt.Errorf("PDB %s is already being watched by another PDBWatcher %s", pdbWatcher.Spec.PDBName, watcher.Name)
			log.FromContext(ctx).Error(err, "conflict!")
			r.Recorder.Event(pdbWatcher, corev1.EventTypeWarning, "Conflict", err.Error())
			return err
		}
	}
	return nil
}

func (r *PDBWatcherReconciler) discoverDeployment(ctx context.Context, pdb *policyv1.PodDisruptionBudget) (string, error) {
	logger := log.FromContext(ctx)
	// Check if PDB overlaps with multiple deployments
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return "", err // Error converting label selector
	}

	podList := &corev1.PodList{}
	err = r.List(ctx, podList, &client.ListOptions{Namespace: pdb.Namespace, LabelSelector: selector, Limit: 1})
	if err != nil {
		return "", err // Error listing pods
	}

	deployments := []string{}
	for _, pod := range podList.Items {
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "ReplicaSet" {
				replicaSet := &appsv1.ReplicaSet{}
				err = r.Get(ctx, types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, replicaSet)
				if err != nil {
					return "", err // Error fetching ReplicaSet
				}

				// Get the Deployment that owns this ReplicaSet
				for _, rsOwnerRef := range replicaSet.OwnerReferences {
					if rsOwnerRef.Kind == "Deployment" {
						deployments = append(deployments, rsOwnerRef.Name)
					}
				}
			}
			//todo handle stateful sets
		}
	}

	// If multiple deployments are found, log a warning and return an error
	if len(deployments) > 1 {
		r.Recorder.Event(pdb, corev1.EventTypeWarning, "MultipleDeployments", "PDB overlaps with multiple deployments") //should we event on pdb watcher?
		return "", fmt.Errorf("PDB %s/%s overlaps with multiple deployments", pdb.Namespace, pdb.Name)
	}

	if len(deployments) == 0 {
		return "", fmt.Errorf("PDB %s/%s overlaps with zero deployments", pdb.Namespace, pdb.Name)
	}
	// Log the deployment map and deployment name for debugging
	logger.Info(fmt.Sprintf("Determined Deployment name: %s->%s", pdb.Name, deployments[0]))
	return deployments[0], nil
}

func (r *PDBWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&myappsv1.PDBWatcher{}).
		WithEventFilter(predicate.Funcs{
			// ignore status updates as we make those.
			UpdateFunc: func(ue event.UpdateEvent) bool {
				return ue.ObjectOld.GetGeneration() != ue.ObjectNew.GetGeneration()
			},
		}).
		Complete(r)
}

// TODO Unittest
// TODO don't do anything if they don't have a max surge
func calculateSurge(ctx context.Context, deployment *appsv1.Deployment, minrepicas int32) int32 {

	maxSurge := int32(1) // Default max surge value
	if deployment.Spec.Strategy.RollingUpdate != nil && deployment.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		if deployment.Spec.Strategy.RollingUpdate.MaxSurge.Type == intstr.Int {
			maxSurge = deployment.Spec.Strategy.RollingUpdate.MaxSurge.IntVal
		} else if deployment.Spec.Strategy.RollingUpdate.MaxSurge.Type == intstr.String {
			percentageStr := strings.TrimSuffix(deployment.Spec.Strategy.RollingUpdate.MaxSurge.StrVal, "%")
			percentage, err := strconv.Atoi(percentageStr)
			if err != nil {
				log.FromContext(ctx).Error(err, "invalid surge", "deployment", deployment.Name)
			}
			maxSurge = int32(math.Ceil((float64(minrepicas) * float64(percentage)) / 100.0))
		}
	}
	return minrepicas + maxSurge
}

func recentEviction(ctx context.Context, watcher myappsv1.PDBWatcher) bool {
	logger := log.FromContext(ctx)
	lastevict := watcher.Spec.LastEviction
	if lastevict.EvictionTime == "" {
		return false
	}

	if lastevict == watcher.Status.LastEviction {
		return false
	}

	evictionTime, err := time.Parse(time.RFC3339, lastevict.EvictionTime)
	if err != nil {
		logger.Error(err, "Failed to parse eviction time")
		return false
	}

	return time.Since(evictionTime) < 5*time.Minute //TODO let user set in spec
}
