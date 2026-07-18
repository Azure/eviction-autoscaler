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

// SurgeOverrideAnnotationKey lets an operator override how much the eviction
// autoscaler surges a target, independently of the target's own
// spec.strategy.rollingUpdate.maxSurge. When set on the target workload it
// ALWAYS takes precedence over maxSurge. The value is an int ("2") or a
// percentage of minReplicas ("10%"), matching maxSurge semantics.
//
// Motivation: safe-deployment guidance often mandates maxSurge=0 on the rollout
// strategy (no extra pods created during an app update). But that also disables
// the eviction autoscaler's drain-time surge for exactly those workloads. This
// annotation decouples the two: keep maxSurge=0 for rollouts while still allowing
// a bounded, drain-only surge that the autoscaler reverts when the drain finishes.
const SurgeOverrideAnnotationKey = "eviction-autoscaler.azure.com/surge-override"

// EvictionAutoScalerReconciler reconciles a EvictionAutoScaler object
type EvictionAutoScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Filter   filter
}

const cooldown = 1 * time.Minute

// +kubebuilder:rbac:groups=eviction-autoscaler.azure.com,resources=evictionautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eviction-autoscaler.azure.com,resources=evictionautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=eviction-autoscaler.azure.com,resources=evictionautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=watch;get;list;update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=watch;get;list
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=update
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;update

func (r *EvictionAutoScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the EvictionAutoScaler instance
	EvictionAutoScaler := &myappsv1.EvictionAutoScaler{}
	err := r.Get(ctx, req.NamespacedName, EvictionAutoScaler)
	if err != nil {
		//should we use a finalizer to scale back down on deletion?
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // EvictionAutoScaler not found, could be deleted, nothing to do
		}
		return ctrl.Result{}, err // Error fetching EvictionAutoScaler
	}
	EvictionAutoScaler = EvictionAutoScaler.DeepCopy() //don't mutate the cache

	// Check if eviction autoscaler should be enabled for this namespace
	isEnabled, err := r.Filter.Filter(ctx, r.Client, EvictionAutoScaler.Namespace)
	if err != nil {
		logger.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", EvictionAutoScaler.Namespace)
		return ctrl.Result{}, err
	}
	if !isEnabled {
		logger.V(1).Info("Eviction autoscaler not enabled for namespace", "namespace", EvictionAutoScaler.Namespace)
		// Don't process evictions for namespaces without the annotation
		return ctrl.Result{}, nil
	}

	// Fetch the PDB using a 1:1 name mapping
	pdb := &policyv1.PodDisruptionBudget{}
	err = r.Get(ctx, types.NamespacedName{Name: EvictionAutoScaler.Name, Namespace: EvictionAutoScaler.Namespace}, pdb)
	if err != nil {
		if apierrors.IsNotFound(err) {
			degraded(&EvictionAutoScaler.Status.Conditions, "NoPdb", "PDB of same name not found")
			logger.Error(err, "no matching pdb", "namespace", EvictionAutoScaler.Namespace, "name", EvictionAutoScaler.Name)
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
		return ctrl.Result{}, err
	}

	if EvictionAutoScaler.Spec.TargetName == "" {
		degraded(&EvictionAutoScaler.Status.Conditions, "EmptyTarget", "no specified target")
		logger.Error(err, "no specified target name", "targetname", EvictionAutoScaler.Spec.TargetName)
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	// StatefulSets are intentionally skipped — their ordered pod management
	// semantics conflict with the eviction surge strategy.
	if strings.EqualFold(EvictionAutoScaler.Spec.TargetKind, statefulSetKind) {
		logger.V(1).Info("skipping StatefulSet target, not supported for eviction surge",
			"targetname", EvictionAutoScaler.Spec.TargetName)
		return ctrl.Result{}, nil
	}

	// Fetch the Deployment target
	// TODO enum validation https://book.kubebuilder.io/reference/generating-crd#validation
	target, err := GetSurger(EvictionAutoScaler.Spec.TargetKind)
	if err != nil {
		logger.Error(err, "invalid target kind", "kind", EvictionAutoScaler.Spec.TargetKind)
		degraded(&EvictionAutoScaler.Status.Conditions, "InvalidTarget", "Invalid Target Kind: "+EvictionAutoScaler.Spec.TargetKind)
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}
	err = r.Get(ctx, types.NamespacedName{Name: EvictionAutoScaler.Spec.TargetName, Namespace: EvictionAutoScaler.Namespace}, target.Obj())
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(err, "pdb watcher target does not exist", "kind", EvictionAutoScaler.Spec.TargetKind, "targetname", EvictionAutoScaler.Spec.TargetName)
			degraded(&EvictionAutoScaler.Status.Conditions, "MissingTarget", "Misssing  Target "+EvictionAutoScaler.Spec.TargetName)
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
		return ctrl.Result{}, err
	}

	// TODO: Move PDB configuration tracking to PDB controller with aggregate labels
	// Consider tracking: maxUnavailable==0 and minAvailable==replicas as PDBGauge labels

	// Detect surge strategy based on KEDA, HPA, or plain deployment
	surgeApplier, err := detectSurgeApplier(ctx, r.Client, EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName, EvictionAutoScaler.Spec.TargetKind, target)
	if err != nil {
		if errors.Is(err, errUnsupportedAutoscalerConfig) {
			logger.Error(err, "unsupported autoscaler configuration, not requeueing")
			degraded(&EvictionAutoScaler.Status.Conditions, "UnsupportedAutoscalerConfiguration", err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
		logger.Error(err, "failed to detect surge strategy")
		return ctrl.Result{}, err
	}

	// Check if the resource version has changed or if it's empty (initial state)
	if EvictionAutoScaler.Status.TargetGeneration == 0 || EvictionAutoScaler.Status.TargetGeneration != target.Obj().GetGeneration() {
		EvictionAutoScaler.Status.TargetGeneration = target.Obj().GetGeneration()
		// Don't reset MinReplicas if a surge is in progress (e.g., HPA/KEDA-driven scaling
		// changes the deployment generation as part of the surge, not a user change).
		if surgeApplier.IsSurgeActive() {
			logger.Info("Target generation changed during active surge, preserving min replicas", "kind", EvictionAutoScaler.Spec.TargetKind, "targetname", EvictionAutoScaler.Spec.TargetName, "currentGeneration", target.Obj().GetGeneration(), "previousGeneration", EvictionAutoScaler.Status.TargetGeneration, "minReplicas", EvictionAutoScaler.Status.MinReplicas)
		} else {
			logger.Info("Target resource version changed resetting min replicas", "kind", EvictionAutoScaler.Spec.TargetKind, "targetname", EvictionAutoScaler.Spec.TargetName, "currentGeneration", target.Obj().GetGeneration(), "previousGeneration", EvictionAutoScaler.Status.TargetGeneration)
			// The resource version has changed, which means someone else has modified the Target.
			// To avoid conflicts, we update our status to reflect the new state and avoid making further changes.
			// Use ResolveMinReplicas to track the effective floor (HPA minReplicas, KEDA minReplicaCount, or deployment replicas).
			minReplicas, _, resolveErr := ResolveMinReplicas(ctx, r.Client, EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName, EvictionAutoScaler.Spec.TargetKind, target.GetReplicas())
			if resolveErr != nil {
				return ctrl.Result{}, resolveErr
			}
			EvictionAutoScaler.Status.MinReplicas = minReplicas
		}
		ready(&EvictionAutoScaler.Status.Conditions, "TargetSpecChange", fmt.Sprintf("resetting min replicas to %d", EvictionAutoScaler.Status.MinReplicas))
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler) //should we go rety in case there is also an eviction or just wait till the next eviction
	}

	// Log current state before checks
	logger.Info(fmt.Sprintf("Checking PDB for %s: DisruptionsAllowed=%d, MinReplicas=%d", pdb.Name, pdb.Status.DisruptionsAllowed, EvictionAutoScaler.Status.MinReplicas))

	// Have we processed all evictions okay don't do anything else
	if EvictionAutoScaler.Spec.LastEviction == EvictionAutoScaler.Status.LastEviction {
		logger.Info("No unhandled eviction ", "pdbname", pdb.Name)
		ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "no unhandled eviction")
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	// Last eviction already tracked above so we can just log it
	logger.V(1).Info("Detected new eviction",
		"podName", EvictionAutoScaler.Spec.LastEviction.PodName,
		"evictionTime", EvictionAutoScaler.Spec.LastEviction.EvictionTime)
	metrics.EvictionCounter.WithLabelValues(EvictionAutoScaler.Namespace).Inc()

	// surgeTarget = minReplicas + displaced, capped at minReplicas + maxSurge.
	// If displaced == 0 the formula yields minReplicas, so no scale-up fires and
	// we fall through to the cooldown/scale-down path — which is correct.
	maxSurgeTarget, surgeErr := calculateSurge(ctx, target, EvictionAutoScaler.Status.MinReplicas)
	if surgeErr != nil {
		switch {
		case errors.Is(surgeErr, errSurgeZero):
			// Surge resolves to 0 (maxSurge=0/unset or a zero override) — can't surge, degrade.
			degraded(&EvictionAutoScaler.Status.Conditions, "UnsupportedAutoscalerConfiguration", surgeErr.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		default:
			// Parse error or unexpected — degrade.
			degraded(&EvictionAutoScaler.Status.Conditions, "InvalidSurgeConfiguration", surgeErr.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
	} else if pdb.Status.DisruptionsAllowed == 0 {
		displaced, countErr := countPodsOnCordoned(ctx, r.Client, pdb)
		if countErr != nil {
			logger.Error(countErr, "failed to count displaced pods on cordoned nodes")
			return ctrl.Result{}, countErr
		}

		surgeTarget := EvictionAutoScaler.Status.MinReplicas + displaced
		if surgeTarget > maxSurgeTarget {
			logger.Info("Displaced pods exceed maxSurge capacity, capping surge", "pdb", pdb.Name, "displaced", displaced, "maxSurgeTarget", maxSurgeTarget)
			surgeTarget = maxSurgeTarget
		}

		if target.GetReplicas() >= surgeTarget {
			//we've scaled up but pdb is still blockign may just be waiting for new pods to become ready
			logger.Info("Have already scaled up to handle evictions, waiting for PDB to allow disruptions before reverting",
				"pdb", pdb.Name,
				"target", EvictionAutoScaler.Spec.TargetName)
			ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "Have already scaled up to handle evictions, waiting for PDB to allow disruptions before reverting")
			return ctrl.Result{RequeueAfter: cooldown}, r.Status().Update(ctx, EvictionAutoScaler)
		}

		logger.Info("No disruptions allowed, scaling up", "pdb", pdb.Name, "lastEviction", EvictionAutoScaler.Spec.LastEviction, "strategy", surgeApplier.Name(), "displaced", displaced, "surgeTarget", surgeTarget)

		// Track blocked eviction if the PDB is blocking the eviction
		metrics.BlockedEvictionCounter.WithLabelValues(EvictionAutoScaler.Namespace, pdb.Name).Inc()

		// Track scaling opportunity with signal label
		signalLabel := metrics.GetScalingSignal(pdb)
		metrics.ScalingOpportunityCounter.WithLabelValues(EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName, metrics.ScaleUpAction, signalLabel).Inc()

		err = surgeApplier.ApplySurge(ctx, surgeTarget)
		if err != nil {
			logger.Error(err, "failed to apply surge", "kind", EvictionAutoScaler.Spec.TargetKind, "targetname", EvictionAutoScaler.Spec.TargetName, "strategy", surgeApplier.Name())
			return ctrl.Result{}, err
		}

		// Track actual scaling action
		metrics.ActualScalingCounter.WithLabelValues(EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName, metrics.ScaleUpAction).Inc()

		// Log the scaling action
		logger.Info(fmt.Sprintf("Scaled up %s %s/%s to %d replicas (via %s)", EvictionAutoScaler.Spec.TargetKind, target.Obj().GetNamespace(), target.Obj().GetName(), surgeTarget, surgeApplier.Name()))
		logger.Info(fmt.Sprintf("TargetGeneration moving from %d->%d", EvictionAutoScaler.Status.TargetGeneration, target.Obj().GetGeneration()))
		// Save ResourceVersion to EvictionAutoScaler status this will cause another reconcile.
		EvictionAutoScaler.Status.TargetGeneration = target.Obj().GetGeneration()
		//Do not update EvictionAutoScaler.Status.LastEviction because we need to keep reconciling till scale down
		ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "eviction with scale up")
		return ctrl.Result{RequeueAfter: cooldown}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	//what if we're allowed disruptions >0 and minreplicas == replicas? Could argue that we should mark the eviction as handled
	//BUT maybe PDB is slow to update? so just letting it requeue anyways

	//Cool down time makes sure we're not still getting more evictions
	//we could substantially reduce this if we looked at pods and knew that none remaining (not already evicted) had been an eviction target but that means tracking more data in EvictionAutoScaler
	// or using pod conditons which we're not doing.....yet
	if time.Since(EvictionAutoScaler.Spec.LastEviction.EvictionTime.Time) < cooldown {
		logger.Info(fmt.Sprintf("Giving %s/%s cooldown of  %s after last eviction %s ", target.Obj().GetNamespace(), target.Obj().GetName(), cooldown, EvictionAutoScaler.Spec.LastEviction.EvictionTime))
		return ctrl.Result{RequeueAfter: cooldown}, nil
	}

	//still at a scaled out state check if we can scale back down
	if target.GetReplicas() > EvictionAutoScaler.Status.MinReplicas {

		// Track scaling opportunity
		metrics.ScalingOpportunityCounter.WithLabelValues(EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName, metrics.ScaleDownAction, metrics.CooldownElapsedSignal).Inc()

		//okay we have allowed disruptions, revert target to the original state
		err = surgeApplier.RevertSurge(ctx, EvictionAutoScaler.Status.MinReplicas)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Track actual scaling action
		metrics.ActualScalingCounter.WithLabelValues(EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName, metrics.ScaleDownAction).Inc()

		// Log the scaling action
		logger.Info(fmt.Sprintf("Reverted surge on %s %s/%s (via %s)", EvictionAutoScaler.Spec.TargetKind, target.Obj().GetNamespace(), target.Obj().GetName(), surgeApplier.Name()))
		// Save ResourceVersion to EvictionAutoScaler status this will cause another reconcile.
		logger.Info(fmt.Sprintf("TargetGeneration moving from %d->%d", EvictionAutoScaler.Status.TargetGeneration, target.Obj().GetGeneration()))
		EvictionAutoScaler.Status.TargetGeneration = target.Obj().GetGeneration()
		EvictionAutoScaler.Status.LastEviction = EvictionAutoScaler.Spec.LastEviction //we could still keep a log here if thats useful
		logger.Info(fmt.Sprintf("Handled eviction %s", EvictionAutoScaler.Spec.LastEviction))

		ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "evictions hit cooldown so scaled down")
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	//could get here if a scale up/down was not needed because we never hit allowed diruptios == 0.
	EvictionAutoScaler.Status.LastEviction = EvictionAutoScaler.Spec.LastEviction //we could still keep a log here if thats useful
	ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "last eviction did not need scaling")
	logger.Info(fmt.Sprintf("Handled eviction %s", EvictionAutoScaler.Spec.LastEviction))
	return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler) //should we go rety in case there is also an eviction or just wait till the next eviction
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

var (
	errSurgeZero         = errors.New("surge is 0; eviction autoscaler cannot surge")
	errInvalidPercentage = errors.New("invalid surge percentage")
	errNegativeSurge     = errors.New("surge value is negative")
)

// calculateSurge returns the maximum replica count after surge (minReplicas + surge).
// The surge amount is taken from the SurgeOverrideAnnotationKey annotation on the
// target when present (it always wins), otherwise from the target's maxSurge.
// Returns a sentinel error to distinguish:
//   - errSurgeZero: the surge amount resolves to 0 (explicitly set or not configured)
//   - errInvalidPercentage: percentage string could not be parsed or lacks a "%" suffix
//   - errNegativeSurge: the surge amount is negative
func calculateSurge(_ context.Context, target Surger, minReplicas int32) (int32, error) {
	// An explicit surge-override annotation on the target always wins over maxSurge,
	// so a workload can keep maxSurge=0 for its rollout strategy yet still surge on drain.
	if ann := target.Obj().GetAnnotations(); ann != nil {
		if override, ok := ann[SurgeOverrideAnnotationKey]; ok {
			return surgeFromValue(intstr.Parse(override), minReplicas)
		}
	}
	return surgeFromValue(target.GetMaxSurge(), minReplicas)
}

// surgeFromValue resolves an int-or-percentage surge value against minReplicas.
// An int is added directly; a percentage (a string ending in "%") is applied to
// minReplicas and rounded up. A zero value yields errSurgeZero; a negative value
// yields errNegativeSurge; a string that is not a valid "<n>%" percentage yields
// errInvalidPercentage.
func surgeFromValue(surge intstr.IntOrString, minReplicas int32) (int32, error) {
	if surge.Type == intstr.Int {
		switch {
		case surge.IntVal < 0:
			return minReplicas, fmt.Errorf("%w: %d", errNegativeSurge, surge.IntVal)
		case surge.IntVal == 0:
			return minReplicas, errSurgeZero
		}
		return minReplicas + surge.IntVal, nil
	}

	if surge.Type == intstr.String {
		// A string surge value must be a percentage, e.g. "10%". Reject bare numbers
		// like "10" so they are never silently interpreted as a percentage.
		if !strings.HasSuffix(surge.StrVal, "%") {
			return minReplicas, fmt.Errorf("%w: %q is not a percentage (missing %% suffix)", errInvalidPercentage, surge.StrVal)
		}
		percentage, err := strconv.Atoi(strings.TrimSuffix(surge.StrVal, "%"))
		if err != nil {
			return minReplicas, fmt.Errorf("%w: %q: %w", errInvalidPercentage, surge.StrVal, err)
		}
		switch {
		case percentage < 0:
			return minReplicas, fmt.Errorf("%w: %q", errNegativeSurge, surge.StrVal)
		case percentage == 0:
			return minReplicas, errSurgeZero
		}
		return minReplicas + int32(math.Ceil((float64(minReplicas)*float64(percentage))/100.0)), nil
	}

	// Unreachable for well-formed intstr values, but handle gracefully
	return minReplicas, errSurgeZero
}
