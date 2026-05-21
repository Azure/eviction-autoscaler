package controllers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	myappsv1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"

	//v1 "k8s.io/api/apps/v1"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
)

const EvictionSurgeReplicasAnnotationKey = "evictionSurgeReplicas"
const OriginalMinReplicasAnnotationKey = "eviction-autoscaler.azure.com/original-min-replicas"

// EvictionAutoScalerReconciler reconciles a EvictionAutoScaler object
type EvictionAutoScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Filter   filter
}

const cooldown = 1 * time.Minute

// defaultTimeToReadyMinutes is used when the deployment does not have the
// eviction-autoscaler.azure.com/time-to-ready annotation. The controller requeues
// once per cooldown period, so this is also the default number of retries.
const defaultTimeToReadyMinutes = 5

// TimeToReadyAnnotationKey is a deployment annotation that tells the controller how
// many minutes the deployment's pods typically take to become Ready. The controller
// uses this to determine how many times to requeue (one per cooldown minute) before
// giving up and reverting the surge.
// Example: eviction-autoscaler.azure.com/time-to-ready: "10"
const TimeToReadyAnnotationKey = "eviction-autoscaler.azure.com/time-to-ready"

// getMaxSurgeAttempts returns the maximum number of requeue attempts before the
// controller gives up waiting for surged pods to become ready and reverts the surge.
// It reads the time-to-ready annotation from the target deployment (in minutes) and
// divides by the cooldown period. Falls back to defaultTimeToReadyMinutes.
// Callers must have already validated the annotation with validateTimeToReadyAnnotation.
func getMaxSurgeAttempts(target Surger) int32 {
	if annotations := target.Obj().GetAnnotations(); annotations != nil {
		if val, ok := annotations[TimeToReadyAnnotationKey]; ok {
			if parsed, err := strconv.ParseInt(val, 10, 32); err == nil && parsed > 0 {
				return int32(math.Ceil(float64(parsed) / cooldown.Minutes()))
			}
		}
	}
	return int32(math.Ceil(float64(defaultTimeToReadyMinutes) / cooldown.Minutes()))
}

// validateTimeToReadyAnnotation checks the TimeToReadyAnnotationKey annotation on the
// target. If present the value must be an integer in [1, 10] (inclusive). Returns a
// non-nil error with a human-readable message when the annotation is invalid so the
// caller can mark the EA CR as Degraded.
func validateTimeToReadyAnnotation(target Surger) error {
	annotations := target.Obj().GetAnnotations()
	if annotations == nil {
		return nil
	}
	val, ok := annotations[TimeToReadyAnnotationKey]
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(val, 10, 32)
	if err != nil || parsed < 1 || parsed > 10 {
		return fmt.Errorf("invalid %s annotation %q: must be an integer between 1 and 10",
			TimeToReadyAnnotationKey, val)
	}
	return nil
}

// reconcileCtx bundles the resources loaded during a single reconcile pass so
// they can be passed between focused helper methods without long argument lists.
type reconcileCtx struct {
	ea           *myappsv1.EvictionAutoScaler
	pdb          *policyv1.PodDisruptionBudget
	target       Surger
	surgeApplier SurgeApplier
}

// +kubebuilder:rbac:groups=eviction-autoscaler.azure.com,resources=evictionautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eviction-autoscaler.azure.com,resources=evictionautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=eviction-autoscaler.azure.com,resources=evictionautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=watch;get;list;update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=watch;get;list
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=update
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;update

// Reconcile is the main reconcile entry point. It delegates each logical phase
// to a focused helper, making the overall flow easy to follow at a glance.
func (r *EvictionAutoScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	rc, result, err, done := r.loadAndValidate(ctx, req)
	if done {
		return result, err
	}

	if result, err, done := r.handleTargetGenerationChange(ctx, rc); done {
		return result, err
	}

	logger.Info(fmt.Sprintf("Checking PDB for %s: DisruptionsAllowed=%d, MinReplicas=%d", rc.pdb.Name, rc.pdb.Status.DisruptionsAllowed, rc.ea.Status.MinReplicas))

	if rc.ea.Spec.LastEviction == rc.ea.Status.LastEviction {
		logger.Info("No unhandled eviction ", "pdbname", rc.pdb.Name)
		ready(&rc.ea.Status.Conditions, "Reconciled", "no unhandled eviction")
		return ctrl.Result{}, r.Status().Update(ctx, rc.ea)
	}

	logger.V(1).Info("Detected new eviction",
		"podName", rc.ea.Spec.LastEviction.PodName,
		"evictionTime", rc.ea.Spec.LastEviction.EvictionTime)
	metrics.EvictionCounter.WithLabelValues(rc.ea.Namespace).Inc()

	if result, err, done := r.applySurgeIfBlocked(ctx, rc); done {
		return result, err
	}

	if result, err, done := r.revertSurgeAfterCooldown(ctx, rc); done {
		return result, err
	}

	//could get here if a scale up/down was not needed because we never hit allowed disruptions == 0.
	rc.ea.Status.LastEviction = rc.ea.Spec.LastEviction
	ready(&rc.ea.Status.Conditions, "Reconciled", "last eviction did not need scaling")
	logger.Info(fmt.Sprintf("Handled eviction %s", rc.ea.Spec.LastEviction))
	return ctrl.Result{}, r.Status().Update(ctx, rc.ea)
}

// loadAndValidate fetches the EvictionAutoScaler, its matching PDB and target workload,
// checks the namespace filter, and validates all annotations. Returns done=true
// whenever the caller should return immediately (error or handled early-exit).
func (r *EvictionAutoScalerReconciler) loadAndValidate(ctx context.Context, req ctrl.Request) (*reconcileCtx, ctrl.Result, error, bool) {
	logger := log.FromContext(ctx)

	ea := &myappsv1.EvictionAutoScaler{}
	if err := r.Get(ctx, req.NamespacedName, ea); err != nil {
		//should we use a finalizer to scale back down on deletion?
		if apierrors.IsNotFound(err) {
			return nil, ctrl.Result{}, nil, true
		}
		return nil, ctrl.Result{}, err, true
	}
	ea = ea.DeepCopy()

	isEnabled, err := r.Filter.Filter(ctx, r.Client, ea.Namespace)
	if err != nil {
		logger.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", ea.Namespace)
		return nil, ctrl.Result{}, err, true
	}
	if !isEnabled {
		logger.V(1).Info("Eviction autoscaler not enabled for namespace", "namespace", ea.Namespace)
		return nil, ctrl.Result{}, nil, true
	}

	// Fetch the PDB using a 1:1 name mapping
	pdb := &policyv1.PodDisruptionBudget{}
	if err := r.Get(ctx, types.NamespacedName{Name: ea.Name, Namespace: ea.Namespace}, pdb); err != nil {
		if apierrors.IsNotFound(err) {
			degraded(&ea.Status.Conditions, "NoPdb", "PDB of same name not found")
			logger.Error(err, "no matching pdb", "namespace", ea.Namespace, "name", ea.Name)
			return nil, ctrl.Result{}, r.Status().Update(ctx, ea), true
		}
		return nil, ctrl.Result{}, err, true
	}

	if ea.Spec.TargetName == "" {
		degraded(&ea.Status.Conditions, "EmptyTarget", "no specified target")
		logger.Error(nil, "no specified target name", "targetname", ea.Spec.TargetName)
		return nil, ctrl.Result{}, r.Status().Update(ctx, ea), true
	}

	// StatefulSets are intentionally skipped — their ordered pod management
	// semantics conflict with the eviction surge strategy.
	if strings.EqualFold(ea.Spec.TargetKind, statefulSetKind) {
		logger.V(1).Info("skipping StatefulSet target, not supported for eviction surge",
			"targetname", ea.Spec.TargetName)
		return nil, ctrl.Result{}, nil, true
	}

	// TODO enum validation https://book.kubebuilder.io/reference/generating-crd#validation
	target, err := GetSurger(ea.Spec.TargetKind)
	if err != nil {
		logger.Error(err, "invalid target kind", "kind", ea.Spec.TargetKind)
		degraded(&ea.Status.Conditions, "InvalidTarget", "Invalid Target Kind: "+ea.Spec.TargetKind)
		return nil, ctrl.Result{}, r.Status().Update(ctx, ea), true
	}
	if err := r.Get(ctx, types.NamespacedName{Name: ea.Spec.TargetName, Namespace: ea.Namespace}, target.Obj()); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(err, "pdb watcher target does not exist", "kind", ea.Spec.TargetKind, "targetname", ea.Spec.TargetName)
			degraded(&ea.Status.Conditions, "MissingTarget", "Misssing  Target "+ea.Spec.TargetName)
			return nil, ctrl.Result{}, r.Status().Update(ctx, ea), true
		}
		return nil, ctrl.Result{}, err, true
	}

	// Validate the time-to-ready annotation before doing any surge work.
	// An out-of-range value is a misconfiguration — surface it as Degraded immediately.
	if err := validateTimeToReadyAnnotation(target); err != nil {
		logger.Error(err, "invalid time-to-ready annotation, marking EA as degraded",
			"target", ea.Spec.TargetName)
		degraded(&ea.Status.Conditions, "InvalidTimeToReadyAnnotation", err.Error())
		return nil, ctrl.Result{}, r.Status().Update(ctx, ea), true
	}

	// TODO: Move PDB configuration tracking to PDB controller with aggregate labels
	// Consider tracking: maxUnavailable==0 and minAvailable==replicas as PDBGauge labels
	surgeApplier, err := detectSurgeApplier(ctx, r.Client, ea.Namespace, ea.Spec.TargetName, ea.Spec.TargetKind, target)
	if err != nil {
		if errors.Is(err, errUnsupportedAutoscalerConfig) {
			logger.Error(err, "unsupported autoscaler configuration, not requeueing")
			degraded(&ea.Status.Conditions, "UnsupportedAutoscalerConfiguration", err.Error())
			return nil, ctrl.Result{}, r.Status().Update(ctx, ea), true
		}
		logger.Error(err, "failed to detect surge strategy")
		return nil, ctrl.Result{}, err, true
	}

	return &reconcileCtx{ea: ea, pdb: pdb, target: target, surgeApplier: surgeApplier}, ctrl.Result{}, nil, false
}

// handleTargetGenerationChange detects when the target workload has been modified
// outside of a surge and resets MinReplicas to reflect the new desired state.
func (r *EvictionAutoScalerReconciler) handleTargetGenerationChange(ctx context.Context, rc *reconcileCtx) (ctrl.Result, error, bool) {
	ea, target, surgeApplier := rc.ea, rc.target, rc.surgeApplier
	if ea.Status.TargetGeneration != 0 && ea.Status.TargetGeneration == target.Obj().GetGeneration() {
		return ctrl.Result{}, nil, false
	}
	logger := log.FromContext(ctx)
	ea.Status.TargetGeneration = target.Obj().GetGeneration()
	// Don't reset MinReplicas if a surge is in progress (e.g., HPA/KEDA-driven scaling
	// changes the deployment generation as part of the surge, not a user change).
	if surgeApplier.IsSurgeActive() {
		logger.Info("Target generation changed during active surge, preserving min replicas",
			"kind", ea.Spec.TargetKind, "targetname", ea.Spec.TargetName,
			"currentGeneration", target.Obj().GetGeneration(),
			"previousGeneration", ea.Status.TargetGeneration,
			"minReplicas", ea.Status.MinReplicas)
	} else {
		logger.Info("Target resource version changed resetting min replicas",
			"kind", ea.Spec.TargetKind, "targetname", ea.Spec.TargetName,
			"currentGeneration", target.Obj().GetGeneration(),
			"previousGeneration", ea.Status.TargetGeneration)
		// The resource version has changed — someone else modified the Target.
		// Use ResolveMinReplicas to track the effective floor.
		minReplicas, _, resolveErr := ResolveMinReplicas(ctx, r.Client, ea.Namespace, ea.Spec.TargetName, ea.Spec.TargetKind, target.GetReplicas())
		if resolveErr != nil {
			return ctrl.Result{}, resolveErr, true
		}
		ea.Status.MinReplicas = minReplicas
	}
	ready(&ea.Status.Conditions, "TargetSpecChange", fmt.Sprintf("resetting min replicas to %d", ea.Status.MinReplicas))
	return ctrl.Result{}, r.Status().Update(ctx, ea), true
}

// applySurgeIfBlocked scales up the target when the PDB has DisruptionsAllowed==0.
// Returns done=true if a scale-up was initiated (or an error occurred).
func (r *EvictionAutoScalerReconciler) applySurgeIfBlocked(ctx context.Context, rc *reconcileCtx) (ctrl.Result, error, bool) {
	if rc.pdb.Status.DisruptionsAllowed != 0 {
		return ctrl.Result{}, nil, false
	}
	logger := log.FromContext(ctx)
	ea, pdb, target, surgeApplier := rc.ea, rc.pdb, rc.target, rc.surgeApplier

	// Compute a right-sized surge: bring up exactly enough replacements to unblock
	// displaced pods, capped at maxSurge. Re-evaluated every reconcile so incremental
	// cordons (Node Y after Node X) top up without double-counting.
	displaced, countErr := countPodsOnCordoned(ctx, r.Client, pdb)
	if countErr != nil {
		logger.Error(countErr, "failed to count displaced pods on cordoned nodes")
		return ctrl.Result{}, countErr, true
	}

	// surgeTarget = minReplicas + displaced, capped at minReplicas + maxSurge.
	// displaced==0 → surgeTarget==minReplicas → no scale-up (fall through to revert).
	// maxSurge==0 → surgeTarget==minReplicas → opted out of surge.
	maxSurgeTarget := calculateSurge(ctx, target, ea.Status.MinReplicas)
	surgeTarget := ea.Status.MinReplicas + displaced
	if surgeTarget > maxSurgeTarget {
		logger.Info("Displaced pods exceed maxSurge capacity, capping surge", "pdb", pdb.Name, "displaced", displaced, "maxSurgeTarget", maxSurgeTarget)
		surgeTarget = maxSurgeTarget
	}

	if target.GetReplicas() >= surgeTarget {
		// Already at or above the desired level; fall through to cooldown/revert.
		return ctrl.Result{}, nil, false
	}

	logger.Info("No disruptions allowed, scaling up",
		"pdb", pdb.Name, "lastEviction", ea.Spec.LastEviction,
		"strategy", surgeApplier.Name(), "displaced", displaced, "surgeTarget", surgeTarget)
	metrics.BlockedEvictionCounter.WithLabelValues(ea.Namespace, pdb.Name).Inc()
	signalLabel := metrics.GetScalingSignal(pdb)
	metrics.ScalingOpportunityCounter.WithLabelValues(ea.Namespace, ea.Spec.TargetName, metrics.ScaleUpAction, signalLabel).Inc()

	if err := surgeApplier.ApplySurge(ctx, surgeTarget); err != nil {
		logger.Error(err, "failed to apply surge", "kind", ea.Spec.TargetKind, "targetname", ea.Spec.TargetName, "strategy", surgeApplier.Name())
		return ctrl.Result{}, err, true
	}

	metrics.ActualScalingCounter.WithLabelValues(ea.Namespace, ea.Spec.TargetName, metrics.ScaleUpAction).Inc()
	logger.Info(fmt.Sprintf("Scaled up %s %s/%s to %d replicas (via %s)", ea.Spec.TargetKind, target.Obj().GetNamespace(), target.Obj().GetName(), surgeTarget, surgeApplier.Name()))
	logger.Info(fmt.Sprintf("TargetGeneration moving from %d->%d", ea.Status.TargetGeneration, target.Obj().GetGeneration()))
	ea.Status.TargetGeneration = target.Obj().GetGeneration()
	ea.Status.SurgeAttempts = 1
	//Do not update ea.Status.LastEviction because we need to keep reconciling till scale down
	ready(&ea.Status.Conditions, "Reconciled", "eviction with scale up")
	return ctrl.Result{RequeueAfter: cooldown}, r.Status().Update(ctx, ea), true
}

// revertSurgeAfterCooldown waits for the eviction cooldown to expire then scales
// the target back to minReplicas. Returns done=true whenever it requeues or reverts.
func (r *EvictionAutoScalerReconciler) revertSurgeAfterCooldown(ctx context.Context, rc *reconcileCtx) (ctrl.Result, error, bool) {
	ea, target, surgeApplier := rc.ea, rc.target, rc.surgeApplier
	logger := log.FromContext(ctx)

	//what if we're allowed disruptions >0 and minreplicas == replicas? Could argue that we should mark the eviction as handled
	//BUT maybe PDB is slow to update? so just letting it requeue anyways

	//Cool down time makes sure we're not still getting more evictions
	//we could substantially reduce this if we looked at pods and knew that none remaining (not already evicted) had been an eviction target
	if time.Since(ea.Spec.LastEviction.EvictionTime.Time) < cooldown {
		logger.Info(fmt.Sprintf("Giving %s/%s cooldown of  %s after last eviction %s ", target.Obj().GetNamespace(), target.Obj().GetName(), cooldown, ea.Spec.LastEviction.EvictionTime))
		return ctrl.Result{RequeueAfter: cooldown}, nil, true
	}

	//still at a scaled out state check if we can scale back down
	if target.GetReplicas() <= ea.Status.MinReplicas {
		return ctrl.Result{}, nil, false
	}

	metrics.ScalingOpportunityCounter.WithLabelValues(ea.Namespace, ea.Spec.TargetName, metrics.ScaleDownAction, metrics.CooldownElapsedSignal).Inc()

	// Wait for surge pods to be ready before reverting to avoid losing drain progress.
	if result, err, done := r.waitForSurgePodReadiness(ctx, ea, target); done {
		return result, err, true
	}

	//okay cooldown elapsed and pods are ready — revert to minReplicas
	if err := surgeApplier.RevertSurge(ctx, ea.Status.MinReplicas); err != nil {
		return ctrl.Result{}, err, true
	}

	metrics.ActualScalingCounter.WithLabelValues(ea.Namespace, ea.Spec.TargetName, metrics.ScaleDownAction).Inc()
	logger.Info(fmt.Sprintf("Reverted surge on %s %s/%s (via %s)", ea.Spec.TargetKind, target.Obj().GetNamespace(), target.Obj().GetName(), surgeApplier.Name()))
	logger.Info(fmt.Sprintf("TargetGeneration moving from %d->%d", ea.Status.TargetGeneration, target.Obj().GetGeneration()))
	ea.Status.TargetGeneration = target.Obj().GetGeneration()
	ea.Status.LastEviction = ea.Spec.LastEviction
	ea.Status.SurgeAttempts = 0
	logger.Info(fmt.Sprintf("Handled eviction %s", ea.Spec.LastEviction))
	ready(&ea.Status.Conditions, "Reconciled", "evictions hit cooldown so scaled down")
	return ctrl.Result{}, r.Status().Update(ctx, ea), true
}

// waitForSurgePodReadiness checks whether the surged pods are ready before allowing
// a revert. If pods are not all ready and the retry budget is not exhausted it
// increments SurgeAttempts, requeues with cooldown, and returns done=true.
// If the budget is exhausted it logs a warning and returns done=false so the
// caller proceeds with the unconditional revert.
func (r *EvictionAutoScalerReconciler) waitForSurgePodReadiness(
	ctx context.Context,
	ea *myappsv1.EvictionAutoScaler,
	target Surger,
) (ctrl.Result, error, bool) {
	logger := log.FromContext(ctx)
	maxAttempts := getMaxSurgeAttempts(target)
	if target.GetReadyReplicas() < target.GetReplicas() && ea.Status.SurgeAttempts < maxAttempts {
		ea.Status.SurgeAttempts++
		logger.Info("Surge active, waiting for pods to become ready before reverting",
			"readyReplicas", target.GetReadyReplicas(),
			"desiredReplicas", target.GetReplicas(),
			"surgeAttempts", ea.Status.SurgeAttempts,
			"maxSurgeAttempts", maxAttempts,
			"target", ea.Spec.TargetName)
		ready(&ea.Status.Conditions, "Reconciled", fmt.Sprintf("waiting for surged pods to become ready (attempt %d/%d)", ea.Status.SurgeAttempts, maxAttempts))
		return ctrl.Result{RequeueAfter: cooldown}, r.Status().Update(ctx, ea), true
	}
	if ea.Status.SurgeAttempts >= maxAttempts {
		logger.Info("Max surge attempts reached, reverting surge",
			"readyReplicas", target.GetReadyReplicas(),
			"desiredReplicas", target.GetReplicas(),
			"surgeAttempts", ea.Status.SurgeAttempts,
			"target", ea.Spec.TargetName)
	}
	return ctrl.Result{}, nil, false
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

func (r *EvictionAutoScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&myappsv1.EvictionAutoScaler{}).
		WithEventFilter(predicate.Funcs{
			// ignore status updates as we make those.
			UpdateFunc: func(ue event.UpdateEvent) bool {
				return ue.ObjectOld.GetGeneration() != ue.ObjectNew.GetGeneration()
			},
		}).
		Complete(r)
}

// TODO Unittest
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
