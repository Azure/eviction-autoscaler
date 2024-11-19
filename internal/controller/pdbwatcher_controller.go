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
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;update
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list

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

	if pdbWatcher.Spec.TargetName == "" {
		//move away from doing this. Have another controller that watches pdbs and creates/updates pdbwatchers
		deploymentName, err := r.discoverDeployment(ctx, pdb) //move this out of thie controller to a controller that watches pdbs
		if err != nil {
			//better error on notfound
			return ctrl.Result{}, err // Error fetching PDB
		}
		pdbWatcher.Spec.TargetName = deploymentName
		pdbWatcher.Spec.TargetKind = deploymentKind
		err = r.Update(ctx, pdbWatcher)
		if err != nil {
			logger.Error(err, "Failed to update PDBWatcher deployment")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil

	}

	// Fetch the Deployment or Statefulset
	target, err := GetSurger(pdbWatcher.Spec.TargetKind)
	if err != nil {
		return ctrl.Result{}, err
	}
	err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Spec.TargetName, Namespace: pdbWatcher.Namespace}, target.Obj())
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
	if pdbWatcher.Status.TargetGeneration == 0 || pdbWatcher.Status.TargetGeneration != target.Obj().GetGeneration() {
		logger.Info("Deployment resource version changed reseting min replicas")
		// The resource version has changed, which means someone else has modified the Deployment.
		// To avoid conflicts, we update our status to reflect the new state and avoid making further changes.
		pdbWatcher.Status.TargetGeneration = target.Obj().GetGeneration()
		pdbWatcher.Status.MinReplicas = target.GetReplicas()
		err = r.Status().Update(ctx, pdbWatcher)
		if err != nil {
			logger.Error(err, "Failed to update PDBWatcher status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil //should we go rety in case there is also an eviction or just wait till the next eviction
	}

	// Log current state before checks
	logger.Info(fmt.Sprintf("Checking PDB for %s: DisruptionsAllowed=%d, MinReplicas=%d", pdb.Name, pdb.Status.DisruptionsAllowed, pdbWatcher.Status.MinReplicas))

	// Check if there are recent evictions
	if !unhandledEviction(ctx, *pdbWatcher) {
		logger.Info("No unhandled eviction ", "pdbname", pdb.Name)
		return ctrl.Result{}, nil
	}

	// Check the DisruptionsAllowed field
	if pdb.Status.DisruptionsAllowed == 0 {
		//What if the evict went through because the pod being evicted wasn't ready anyways? Handle that in webhook or here?

		// Handle nil Deployment Strategy and MaxSurge
		logger.Info(fmt.Sprintf("No disruptions allowed for %s and recent eviction attempting to scale up", pdb.Name))
		// Scale up the Deployment
		newReplicas := calculateSurge(ctx, target, pdbWatcher.Status.MinReplicas)
		target.SetReplicas(newReplicas)
		err = r.Update(ctx, target.Obj())
		if err != nil {
			logger.Error(err, "failed to update deployment")
			return ctrl.Result{}, err
		}

		// Log the scaling action
		logger.Info(fmt.Sprintf("Scaled up Deployment %s/%s to %d replicas", target.Obj().GetNamespace(), target.Obj().GetName(), newReplicas))
	} else if target.GetReplicas() != pdbWatcher.Status.MinReplicas {
		//don't scale down immediately as eviction and scaledown might remove all good pods.
		//instead give a cool off time?
		evictionTime, err := time.Parse(time.RFC3339, pdbWatcher.Status.LastEviction.EvictionTime)
		if err != nil {
			logger.Error(err, "Failed to parse eviction time")
			return ctrl.Result{}, err
		}
		cooldown := 10 * time.Second //this is trcky how long do we wate for eviction to proccess. Let pdbwatcher set this?
		if time.Since(evictionTime) < cooldown {

			logger.Info(fmt.Sprintf("Giving %s/%s cooldown of  %s after last eviction %s ", target.Obj().GetNamespace(), target.Obj().GetName(), cooldown, evictionTime))
			return ctrl.Result{RequeueAfter: cooldown / 2}, nil
		}

		//okay we aren't at allowed disruptions Revert Target to the original state
		target.SetReplicas(pdbWatcher.Status.MinReplicas)
		err = r.Update(ctx, target.Obj())
		if err != nil {
			return ctrl.Result{}, err
		}

		// Log the scaling action
		logger.Info(fmt.Sprintf("Reverted Deployment %s/%s to %d replicas", target.Obj().GetNamespace(), target.Obj().GetName(), target.GetReplicas()))
	} // else log nothing to do or too noisy?

	// Save ResourceVersion to PDBWatcher status this will cause another reconcile.
	pdbWatcher.Status.TargetGeneration = target.Obj().GetGeneration()
	pdbWatcher.Status.LastEviction = pdbWatcher.Spec.LastEviction //we could still keep a log here if thats useful
	//should we clear evictions?
	err = r.Status().Update(ctx, pdbWatcher)
	if err != nil {
		logger.Error(err, "Failed to update PDBWatcher status")
		return ctrl.Result{}, err
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

// discoverDeployment is for lazy users who don't specify a targetName. Its just goin to pick the deployment owned by the first pod it finds
// matcing
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
						logger.Info(fmt.Sprintf("Determined Deployment name: %s->%s", pdb.Name, rsOwnerRef.Name))
						return rsOwnerRef.Name, nil
					}
				}
			}
			//todo handle stateful sets? Too dangersous?
		}
	}
	return "", fmt.Errorf("PDB %s/%s overlaps with zero deployments", pdb.Namespace, pdb.Name)
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
func calculateSurge(ctx context.Context, target Surger, minrepicas int32) int32 {

	surge := target.GetMaxSurge()
	if surge.Type == intstr.Int {
		return minrepicas
	}

	if surge.Type == intstr.String {
		percentageStr := strings.TrimSuffix(surge.StrVal, "%")
		percentage, err := strconv.Atoi(percentageStr)
		if err != nil {
			//todo add name?
			log.FromContext(ctx).Error(err, "invalid surge")
		}
		return minrepicas + int32(math.Ceil((float64(minrepicas)*float64(percentage))/100.0))
	}

	panic("must be string or int")

}

// should these be guids rather than times?
func unhandledEviction(ctx context.Context, watcher myappsv1.PDBWatcher) bool {
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
