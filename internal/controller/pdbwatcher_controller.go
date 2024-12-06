package controllers

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	// Fetch the PDB using a 1:1 name mapping
	pdb := &policyv1.PodDisruptionBudget{}
	err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Name, Namespace: pdbWatcher.Namespace}, pdb)
	if err != nil {
		if errors.IsNotFound(err) {
			degraded(&pdbWatcher.Status.Conditions, "NoPdb", "PDB of same name not found")
			logger.Error(err, "no matching pdb", "namespace", pdbWatcher.Namespace, "name", pdbWatcher.Name)
			return ctrl.Result{}, r.Status().Update(ctx, pdbWatcher)
		}
		return ctrl.Result{}, err
	}

	if pdbWatcher.Spec.TargetName == "" {
		degraded(&pdbWatcher.Status.Conditions, "EmptyTarget", "no specified target")
		logger.Error(err, "no specified target name", "targetname", pdbWatcher.Spec.TargetName)
		return ctrl.Result{}, r.Status().Update(ctx, pdbWatcher)
	}

	// Fetch the Deployment or Statefulset
	// TODO enum validation https://book.kubebuilder.io/reference/generating-crd#validation
	target, err := GetSurger(pdbWatcher.Spec.TargetKind)
	if err != nil {
		logger.Error(err, "invalid target kind", "kind", pdbWatcher.Spec.TargetKind)
		degraded(&pdbWatcher.Status.Conditions, "InvalidTarget", "Invalid Target Kind: "+pdbWatcher.Spec.TargetKind)
		return ctrl.Result{}, r.Status().Update(ctx, pdbWatcher)
	}
	err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Spec.TargetName, Namespace: pdbWatcher.Namespace}, target.Obj())
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Error(err, "pdb watcher target does not exist", "kind", pdbWatcher.Spec.TargetKind, "targetname", pdbWatcher.Spec.TargetName)
			degraded(&pdbWatcher.Status.Conditions, "MissingTarget", "Misssing  Target "+pdbWatcher.Spec.TargetName)
			return ctrl.Result{}, r.Status().Update(ctx, pdbWatcher)
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
		ready(&pdbWatcher.Status.Conditions, "DeploymentSpecChange", fmt.Sprintf("resetting min replicas to %d", pdbWatcher.Status.MinReplicas))
		return ctrl.Result{}, r.Status().Update(ctx, pdbWatcher) //should we go rety in case there is also an eviction or just wait till the next eviction
	}

	// Log current state before checks
	logger.Info(fmt.Sprintf("Checking PDB for %s: DisruptionsAllowed=%d, MinReplicas=%d", pdb.Name, pdb.Status.DisruptionsAllowed, pdbWatcher.Status.MinReplicas))

	// Check if there are recent evictions
	if !unhandledEviction(*pdbWatcher) {
		logger.Info("No unhandled eviction ", "pdbname", pdb.Name)
		ready(&pdbWatcher.Status.Conditions, "Reconciled", "no unhandled eviction")
		return ctrl.Result{}, r.Status().Update(ctx, pdbWatcher)
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
		cooldown := 10 * time.Second //this is trcky how long do we wate for eviction to process. Let pdbwatcher set this?
		if time.Since(pdbWatcher.Spec.LastEviction.EvictionTime.Time) < cooldown {

			logger.Info(fmt.Sprintf("Giving %s/%s cooldown of  %s after last eviction %s ", target.Obj().GetNamespace(), target.Obj().GetName(), cooldown, pdbWatcher.Spec.LastEviction.EvictionTime))
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
	}
	// else log nothing to do or too noisy?

	// Save ResourceVersion to PDBWatcher status this will cause another reconcile.
	pdbWatcher.Status.TargetGeneration = target.Obj().GetGeneration()
	pdbWatcher.Status.LastEviction = pdbWatcher.Spec.LastEviction //we could still keep a log here if thats useful
	ready(&pdbWatcher.Status.Conditions, "Reconciled", "eviction handled")
	return ctrl.Result{}, r.Status().Update(ctx, pdbWatcher) //should we go rety in case there is also an eviction or just wait till the next eviction
}

func ready(conditions *[]metav1.Condition, reason string, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
	meta.RemoveStatusCondition(conditions, "Degraded")
}

func degraded(conditions *[]metav1.Condition, reason string, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               "Degraded",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
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
		return minrepicas + surge.IntVal
	}

	if surge.Type == intstr.String {
		percentageStr := strings.TrimSuffix(surge.StrVal, "%")
		percentage, err := strconv.Atoi(percentageStr)
		if err != nil {
			//return an error? so we can set degraded?
			log.FromContext(ctx).Error(err, "invalid surge")
			return minrepicas
		}
		return minrepicas + int32(math.Ceil((float64(minrepicas)*float64(percentage))/100.0))
	}

	panic("must be string or int")

}

// should these be guids rather than times?
func unhandledEviction(watcher myappsv1.PDBWatcher) bool {
	lastEvict := watcher.Spec.LastEviction

	if lastEvict == watcher.Status.LastEviction {
		return false
	}

	return time.Since(lastEvict.EvictionTime.Time) < 5*time.Minute //TODO let user set in spec
}
