package controllers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const EvictionSurgeReplicasAnnotationKey = "evictionSurgeReplicas"
const OriginalMinReplicasAnnotationKey = "eviction-autoscaler.azure.com/original-min-replicas"

// EASSurgeFinalizer is placed on the EvictionAutoScaler while it holds an active surge on
// its target, so a mid-drain CR delete is held until this reconciler reverts the surge. It
// is distinct from PDBFloorFinalizer (owned by the PDB actuator, which restores the partner
// PDB): each controller owns the finalizer for the object it writes — this reconciler writes
// the Deployment/HPA/KEDA surge, the actuator writes the PDB. Removing it releases the CR
// for garbage collection.
const EASSurgeFinalizer = "eviction-autoscaler.azure.com/surge-revert"

// EvictionAutoScalerReconciler reconciles a EvictionAutoScaler object
type EvictionAutoScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Filter   filter
	// ZeroSurgeOverride lets the controller surge a workload whose maxSurge
	// resolves to 0 — an explicit maxSurge: 0 (common under safe-deployment
	// guidance) or a Recreate strategy — which otherwise cannot surge and
	// would degrade. Note: an unset RollingUpdate strategy is NOT treated as
	// zero — Kubernetes defaults it to 25% at admission time, and GetMaxSurge
	// returns that default. When non-nil, its value is applied as the drain surge for such
	// workloads: an int-or-percentage resolved against minReplicas, mirroring
	// Kubernetes' own maxSurge semantics — e.g. "25%" (rounded up) or an absolute
	// "10". The actual surge stays demand-driven (minReplicas + displaced) and is
	// capped at this amount, so larger drains proceed in waves. It is a fleet-wide,
	// install-time knob (the ZERO_SURGE_OVERRIDE controller env var); nil (the
	// default) preserves today's degrade-on-zero behavior, so Cosmic — not
	// individual workload owners — decides whether it applies.
	ZeroSurgeOverride *intstr.IntOrString
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

// recordPanic counts a recovered reconcile panic against the namespace and target that caused
// it, then re-panics so controller-runtime still handles it as before.
func recordPanic(controller, namespace string, target *string) {
	rec := recover()
	if rec == nil {
		return
	}
	targetName := ""
	if target != nil {
		targetName = *target
	}
	metrics.PanicCounter.WithLabelValues(namespace, targetName, controller).Inc()
	panic(rec)
}

func (r *EvictionAutoScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	panicTarget := req.Name
	defer recordPanic("evictionautoscaler", req.Namespace, &panicTarget)
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

	// Teardown-first: a terminating EAS that still owns an active surge must revert it
	// before any other early-return below (namespace-disabled, degrade, etc.), so a
	// mid-drain CR delete — or a namespace opt-out/uninstall that deletes the CR and never
	// recreates it — can never strand the target permanently surged. Held by
	// EASSurgeFinalizer, which this reconciler owns (single-writer: we write the target).
	if !EvictionAutoScaler.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileSurgeTeardown(ctx, EvictionAutoScaler)
	}

	// Flag-off clear: if the master switch is off but a pinned-floor policy is still
	// recorded, clear it unconditionally. Placed before namespace/target/surge lookups
	// (all of which can early-return) so turning the flag off always un-pins.
	if clearPinnedFloorIfDisabled(EvictionAutoScaler) {
		logger.Info("PDB floor mutation disabled, clearing pinned PDB floor policy")
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	// Check if eviction autoscaler should be enabled for this namespace
	isEnabled, err := r.Filter.Filter(ctx, r.Client, EvictionAutoScaler.Namespace)
	if err != nil {
		logger.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", EvictionAutoScaler.Namespace)
		return ctrl.Result{}, err
	}
	if !isEnabled {
		logger.V(1).Info("Eviction autoscaler not enabled for namespace", "namespace", EvictionAutoScaler.Namespace)
		// Namespace disabled while we hold a pin: drop it so the PDB actuator restores the
		// partner. Persist only when there was one, to avoid a status write on every pass.
		if EvictionAutoScaler.Status.PDBFloorPinned {
			clearPinIfHeld(EvictionAutoScaler)
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
		// Don't process evictions for namespaces without the annotation
		return ctrl.Result{}, nil
	}

	// Fetch the PDB using a 1:1 name mapping
	pdb := &policyv1.PodDisruptionBudget{}
	err = r.Get(ctx, types.NamespacedName{Name: EvictionAutoScaler.Name, Namespace: EvictionAutoScaler.Namespace}, pdb)
	if err != nil {
		if apierrors.IsNotFound(err) {
			degraded(EvictionAutoScaler, "NoPdb", "PDB of same name not found")
			logger.Error(err, "no matching pdb", "namespace", EvictionAutoScaler.Namespace, "name", EvictionAutoScaler.Name)
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
		return ctrl.Result{}, err
	}

	if EvictionAutoScaler.Spec.TargetName == "" {
		degraded(EvictionAutoScaler, "EmptyTarget", "no specified target")
		logger.Error(err, "no specified target name", "targetname", EvictionAutoScaler.Spec.TargetName)
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}
	panicTarget = EvictionAutoScaler.Spec.TargetName

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
		degraded(EvictionAutoScaler, "InvalidTarget", "Invalid Target Kind: "+EvictionAutoScaler.Spec.TargetKind)
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}
	err = r.Get(ctx, types.NamespacedName{Name: EvictionAutoScaler.Spec.TargetName, Namespace: EvictionAutoScaler.Namespace}, target.Obj())
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(err, "pdb watcher target does not exist", "kind", EvictionAutoScaler.Spec.TargetKind, "targetname", EvictionAutoScaler.Spec.TargetName)
			degraded(EvictionAutoScaler, "MissingTarget", "Misssing  Target "+EvictionAutoScaler.Spec.TargetName)
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
		return ctrl.Result{}, err
	}

	// Track whether this workload's rollout maxSurge resolves to 0 (an explicit
	// maxSurge: 0 or a Recreate strategy). An unset RollingUpdate defaults to
	// 25% (the Kubernetes default) and is NOT counted as zero. Set per reconcile
	// so the series sum reflects the current count of maxSurge:0 workloads in the cluster.
	recordZeroMaxSurge(target, EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName)

	// TODO: Move PDB configuration tracking to PDB controller with aggregate labels
	// Consider tracking: maxUnavailable==0 and minAvailable==replicas as PDBGauge labels

	// Detect surge strategy based on KEDA, HPA, or plain deployment
	surgeApplier, err := detectSurgeApplier(ctx, r.Client, EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName, EvictionAutoScaler.Spec.TargetKind, target)
	if err != nil {
		if errors.Is(err, errUnsupportedAutoscalerConfig) {
			logger.Error(err, "unsupported autoscaler configuration, not requeueing")
			degraded(EvictionAutoScaler, "UnsupportedAutoscalerConfiguration", err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
		logger.Error(err, "failed to detect surge strategy")
		return ctrl.Result{}, err
	}

	// Bail-on-replica-change: if policy says a pinned floor is active and the target's
	// replica count changed externally, clear the policy and end the pass. Returning here
	// is required so the pin condition further down cannot re-pin in the same reconcile.
	// The PDB actuator restores the partner PDB async.
	if bailPinnedFloorOnExternalChange(EvictionAutoScaler, target, surgeApplier) {
		logger.Info("External replica change detected while holding a pinned PDB floor policy, bailing", "pdb", pdb.Name)
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	// Check if the resource version has changed or if it's empty (initial state)
	if EvictionAutoScaler.Status.TargetGeneration == 0 || EvictionAutoScaler.Status.TargetGeneration != target.Obj().GetGeneration() {
		EvictionAutoScaler.Status.TargetGeneration = target.Obj().GetGeneration()
		// Don't reset MinReplicas if a surge is in progress (e.g., HPA/KEDA-driven scaling
		// changes the deployment generation as part of the surge, not a user change).
		if surgeApplier.IsSurgeActive() {
			r.recoverBaselineForActiveSurge(ctx, EvictionAutoScaler, surgeApplier)
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
		clearPinIfHeld(EvictionAutoScaler)
		logger.Info("No unhandled eviction ", "pdbname", pdb.Name)
		ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "no unhandled eviction")
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	// Last eviction already tracked above so we can just log it
	logger.V(1).Info("Detected new eviction",
		"podName", EvictionAutoScaler.Spec.LastEviction.PodName,
		"evictionTime", EvictionAutoScaler.Spec.LastEviction.EvictionTime)
	metrics.EvictionCounter.WithLabelValues(EvictionAutoScaler.Namespace).Inc()

	// calculateSurge returns the surge ceiling: minReplicas + maxSurge, or — when maxSurge
	// resolves to 0 and ZeroSurgeOverride is set — minReplicas + the override. surgeTarget
	// below is demand-driven (minReplicas + displaced), clamped to this ceiling.
	maxSurgeTarget, surgeErr := calculateSurge(ctx, target, EvictionAutoScaler.Status.MinReplicas, r.ZeroSurgeOverride)
	if surgeErr != nil {
		switch {
		case errors.Is(surgeErr, errMaxSurgeZero):
			// maxSurge resolves to 0 and no ZeroSurgeOverride set — nothing to surge, degrade.
			degraded(EvictionAutoScaler, "UnsupportedAutoscalerConfiguration", surgeErr.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		default:
			// Parse error or unexpected — degrade.
			degraded(EvictionAutoScaler, "InvalidSurgeConfiguration", surgeErr.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
	} else if pdb.Status.DisruptionsAllowed == 0 {
		return r.handleBlockedDrain(ctx, EvictionAutoScaler, target, pdb, surgeApplier, maxSurgeTarget)
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

		clearPinIfHeld(EvictionAutoScaler)

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
		// Note: the surge-revert finalizer is intentionally NOT released here. It only gates
		// deletion-time teardown (reconcileSurgeTeardown), so leaving it on an idle, already-
		// reverted EAS is harmless — on delete, teardown finds no owned surge and simply
		// removes it. Keeping finalizer changes out of this hot path keeps status and metadata
		// writes independent (no clobber, fully retry-convergent).
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	//could get here if a scale up/down was not needed because we never hit allowed diruptios == 0.
	clearPinIfHeld(EvictionAutoScaler)
	EvictionAutoScaler.Status.LastEviction = EvictionAutoScaler.Spec.LastEviction //we could still keep a log here if thats useful
	ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "last eviction did not need scaling")
	logger.Info(fmt.Sprintf("Handled eviction %s", EvictionAutoScaler.Spec.LastEviction))
	return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler) //should we go rety in case there is also an eviction or just wait till the next eviction
}

// pdbFloorMutationEnabled is the master switch for the PDB-floor pinning feature; it
// ships OFF (dormant). A package var (not const) so tests can enable it. PR4 wires it
// from install-time env.
var pdbFloorMutationEnabled = false

// handleBlockedDrain runs the DisruptionsAllowed==0 surge path: it counts displaced pods,
// computes the demand-driven surge target (capped at the maxSurge ceiling), pins the PDB
// floor policy when we are driving our own surge, and either waits (already surged) or
// applies the surge. Extracted from Reconcile to keep its cyclomatic complexity in check.
func (r *EvictionAutoScalerReconciler) handleBlockedDrain(ctx context.Context, EvictionAutoScaler *myappsv1.EvictionAutoScaler, target Surger, pdb *policyv1.PodDisruptionBudget, surgeApplier SurgeApplier, maxSurgeTarget int32) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Ensure the surge-revert finalizer is durably present BEFORE we surge, so a delete of
	// this CR at any point after the surge can always revert it. r.Update persists the
	// finalizer synchronously here, before ApplySurge writes the surge later in this same
	// pass — so the ordering guarantee holds without a separate reconcile. On update failure
	// we return the error (nothing surged yet) and retry.
	if controllerutil.AddFinalizer(EvictionAutoScaler, EASSurgeFinalizer) {
		if err := r.Update(ctx, EvictionAutoScaler); err != nil {
			// A forbidden write (e.g. Deployment Safeguards on an AKS-owned namespace) is
			// terminal, not transient: record it and stop rather than hot-looping. Nothing
			// has surged yet, so there is nothing to revert. Mirrors the ApplySurge path.
			if apierrors.IsForbidden(err) {
				logger.Error(err, "forbidden to add surge-revert finalizer; recording Degraded and not retrying")
				meta.RemoveStatusCondition(&EvictionAutoScaler.Status.Conditions, "Ready")
				degraded(EvictionAutoScaler, "SurgeForbidden", err.Error())
				return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
			}
			return ctrl.Result{}, err
		}
	}

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

	// Pin the PDB floor whenever WE are driving the surge (baseline OR our recorded surge).
	if shouldPinFloorForOwnSurge(EvictionAutoScaler, target, surgeApplier, pdb) {
		EvictionAutoScaler.Status.PDBFloorPinned = true
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

	// Surface when the fleet-wide zero-maxSurge override drives a surge — we are
	// deliberately surging a workload whose author set an explicit (often 0)
	// rollout maxSurge. Logged only when a surge actually fires.
	r.logZeroSurgeOverride(ctx, target, surgeTarget)

	// Track blocked eviction if the PDB is blocking the eviction
	metrics.BlockedEvictionCounter.WithLabelValues(EvictionAutoScaler.Namespace, pdb.Name).Inc()

	// Track scaling opportunity with signal label
	signalLabel := metrics.GetScalingSignal(pdb)
	metrics.ScalingOpportunityCounter.WithLabelValues(EvictionAutoScaler.Namespace, EvictionAutoScaler.Spec.TargetName, metrics.ScaleUpAction, signalLabel).Inc()

	if err := surgeApplier.ApplySurge(ctx, surgeTarget); err != nil {
		logger.Error(err, "failed to apply surge", "kind", EvictionAutoScaler.Spec.TargetKind, "targetname", EvictionAutoScaler.Spec.TargetName, "strategy", surgeApplier.Name())
		if apierrors.IsForbidden(err) {
			meta.RemoveStatusCondition(&EvictionAutoScaler.Status.Conditions, "Ready")
			degraded(EvictionAutoScaler, "SurgeForbidden", err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
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

// recoverBaselineForActiveSurge recovers the pre-surge baseline for an EvictionAutoScaler
// that observed an active surge but has Status.MinReplicas==0. A freshly (re)created EAS
// starts at 0; if a surge is already active on the target (an orphaned surge that outlived a
// prior EAS), preserving 0 would make a later RevertSurge scale the workload to zero. The
// applier stamped the true baseline as a durable annotation on the object it owns.
func (r *EvictionAutoScalerReconciler) recoverBaselineForActiveSurge(ctx context.Context, eas *myappsv1.EvictionAutoScaler, surgeApplier SurgeApplier) {
	if eas.Status.MinReplicas != 0 {
		return
	}
	if baseline, ok := surgeApplier.RecordedBaseline(); ok {
		log.FromContext(ctx).Info("Recovered pre-surge baseline from durable annotation for surging target",
			"kind", eas.Spec.TargetKind, "targetname", eas.Spec.TargetName, "baseline", baseline)
		eas.Status.MinReplicas = baseline
	}
}

// reconcileSurgeTeardown reverts a surge still owned by a terminating EvictionAutoScaler,
// then releases the surge finalizer for garbage collection. It runs before every other
// reconcile early-return so a mid-drain CR delete — or a namespace opt-out/uninstall that
// deletes the CR without recreating it — can never strand the target permanently surged.
// Single-writer: only this reconciler reverts the surge (the PDB actuator restores the
// partner PDB under its own PDBFloorFinalizer). Revert is conditional (ownsActiveSurge) so a
// partner that has since taken over the target's replica count is never fought.
func (r *EvictionAutoScalerReconciler) reconcileSurgeTeardown(ctx context.Context, eas *myappsv1.EvictionAutoScaler) error {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(eas, EASSurgeFinalizer) {
		return nil // not ours to hold — nothing blocks GC
	}

	// Resolve the target + applier to check ownership and revert. A missing target, an
	// unsupported target kind, or an unsupported autoscaler config means there is nothing we
	// surged to revert — fall through and release the finalizer.
	if eas.Spec.TargetName != "" && !strings.EqualFold(eas.Spec.TargetKind, statefulSetKind) {
		if target, err := GetSurger(eas.Spec.TargetKind); err == nil {
			getErr := r.Get(ctx, types.NamespacedName{Name: eas.Spec.TargetName, Namespace: eas.Namespace}, target.Obj())
			switch {
			case getErr == nil:
				// A deployment carrying our surge marker is the unambiguous fingerprint of a
				// plain-deployment surge (HPA/KEDA appliers mark their own object, not the
				// deployment). Revert it directly, so a topology change after the surge — e.g.
				// an HPA added before the delete, which detectSurgeApplier would now
				// mis-select — can't drop the finalizer while the deployment stays surged.
				var surgeApplier SurgeApplier
				var detErr error
				if hasTargetAnnotation(target) {
					surgeApplier = &DeploymentSurgeApplier{client: r.Client, target: target}
				} else {
					surgeApplier, detErr = detectSurgeApplier(ctx, r.Client, eas.Namespace, eas.Spec.TargetName, eas.Spec.TargetKind, target)
				}
				switch {
				case errors.Is(detErr, errUnsupportedAutoscalerConfig):
					// Unsupported config never reached ApplySurge — nothing to revert.
				case detErr != nil:
					return detErr // transient — keep finalizer, retry
				case ownsActiveSurge(target, surgeApplier):
					if err := surgeApplier.RevertSurge(ctx, eas.Status.MinReplicas); err != nil {
						return err // keep finalizer, retry
					}
					logger.Info("Reverted surge on EvictionAutoScaler deletion, releasing surge finalizer",
						"kind", eas.Spec.TargetKind, "targetname", eas.Spec.TargetName, "namespace", eas.Namespace)
				}
			case apierrors.IsNotFound(getErr):
				// Target already gone — nothing to revert.
			default:
				return getErr // transient — keep finalizer, retry
			}
		}
	}

	controllerutil.RemoveFinalizer(eas, EASSurgeFinalizer)
	return r.Update(ctx, eas)
}

// ownsActiveSurge reports whether the applier still owns an active surge we may safely revert.
// A plain-Deployment surge IS the deployment's replica count, so a live count that no longer
// matches what we recorded means a partner has taken over — reverting would fight them (and
// could scale them down), so we require an exact match. For HPA/KEDA the surge is a floor on
// the autoscaler object and RevertSurge is a safe, idempotent reset of that floor (the HPA
// legitimately moves deployment replicas), so an active surge marker alone is sufficient.
func ownsActiveSurge(target Surger, surgeApplier SurgeApplier) bool {
	if !surgeApplier.IsSurgeActive() {
		return false
	}
	if _, isDeployment := surgeApplier.(*DeploymentSurgeApplier); isDeployment {
		recorded, ok := surgeApplier.RecordedSurge()
		return ok && target.GetReplicas() == recorded
	}
	return true
}

// externalReplicaChange reports whether the target's replica count changed outside our
// surge: a generation mismatch, or live desired replicas != the recorded surge count.
func externalReplicaChange(eas *myappsv1.EvictionAutoScaler, target Surger, surgeApplier SurgeApplier) bool {
	if eas.Status.TargetGeneration != 0 && eas.Status.TargetGeneration != target.Obj().GetGeneration() {
		return true
	}
	recorded, ok := surgeApplier.RecordedSurge()
	return ok && target.GetReplicas() != recorded
}

// desiredHealthyAt computes a PDB's desired-healthy count at a replica count, mirroring
// the built-in disruption controller (minAvailable, or replicas-maxUnavailable, %
// rounded up, clamped ≥0). Lets the floor be derived synchronously instead of from the
// async Status.DesiredHealthy. No budget → 0.
func desiredHealthyAt(spec policyv1.PodDisruptionBudgetSpec, replicas int32) (int32, error) {
	switch {
	case spec.MaxUnavailable != nil:
		maxUnavailable, err := intstr.GetScaledValueFromIntOrPercent(spec.MaxUnavailable, int(replicas), true)
		if err != nil {
			return 0, fmt.Errorf("resolving maxUnavailable: %w", err)
		}
		desired := int(replicas) - maxUnavailable
		if desired < 0 {
			desired = 0
		}
		return int32(desired), nil
	case spec.MinAvailable != nil:
		minAvailable, err := intstr.GetScaledValueFromIntOrPercent(spec.MinAvailable, int(replicas), true)
		if err != nil {
			return 0, fmt.Errorf("resolving minAvailable: %w", err)
		}
		return int32(minAvailable), nil
	default:
		return 0, nil
	}
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

// degraded marks the EAS Degraded and drops any held pinned-floor policy: a degraded path means
// we can no longer manage the surge, so the PDB actuator must be allowed to restore the partner.
func degraded(eas *myappsv1.EvictionAutoScaler, reason string, message string) {
	clearPinIfHeld(eas)
	meta.SetStatusCondition(&eas.Status.Conditions, metav1.Condition{
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
				// React to spec (generation) changes as before.
				if ue.ObjectOld.GetGeneration() != ue.ObjectNew.GetGeneration() {
					return true
				}
				// Also react to a delete transition (DeletionTimestamp set) and finalizer
				// changes — neither bumps generation, but the surge-teardown finalizer
				// handler must run on them, otherwise a mid-drain CR delete would be filtered
				// out and never revert the surge.
				if ue.ObjectOld.GetDeletionTimestamp().IsZero() != ue.ObjectNew.GetDeletionTimestamp().IsZero() {
					return true
				}
				return !slices.Equal(ue.ObjectOld.GetFinalizers(), ue.ObjectNew.GetFinalizers())
			},
		}).
		Complete(r)
}

var (
	errMaxSurgeZero      = errors.New("maxSurge is 0; eviction autoscaler cannot surge")
	errInvalidPercentage = errors.New("invalid surge percentage")
	errNegativeSurge     = errors.New("surge value is negative")
)

// calculateSurge returns the maximum replica count after surge (minReplicas + surge).
// The surge amount is normally taken from the target's maxSurge (via GetMaxSurge,
// which returns the Kubernetes default 25% for an unset RollingUpdate strategy).
// When maxSurge resolves to 0 (an explicit maxSurge: 0 or a Recreate strategy) and
// a fleet-wide zeroSurgeOverride is configured, that override — an int-or-percentage
// resolved against minReplicas — is applied instead of refusing to surge.
// Note: workloads with an unset strategy (or unset maxSurge within RollingUpdate)
// get the Kubernetes default 25% and are NOT eligible for the override.
// The underlying error unwraps to a sentinel:
//   - errMaxSurgeZero: the surge amount resolves to 0 and no override applies
//   - errInvalidPercentage: percentage string could not be parsed or lacks a "%" suffix
//   - errNegativeSurge: the surge amount is negative
func calculateSurge(_ context.Context, target Surger, minReplicas int32, zeroSurgeOverride *intstr.IntOrString) (int32, error) {
	result, err := surgeFromValue(target.GetMaxSurge(), minReplicas)
	// When the workload cannot surge on its own (maxSurge resolves to 0) and a
	// fleet-wide override is configured, substitute the override surge (an
	// int-or-percentage of minReplicas). The actual surge stays demand-driven
	// (minReplicas + displaced) and is capped at this amount, so larger drains
	// proceed in waves.
	if errors.Is(err, errMaxSurgeZero) && zeroSurgeOverride != nil {
		return surgeFromValue(*zeroSurgeOverride, minReplicas)
	}
	return result, err
}

// ParseZeroSurgeOverride parses the fleet-wide zero-maxSurge override value (the
// ZERO_SURGE_OVERRIDE controller env var). The value is an int-or-percentage
// resolved against minReplicas at drain time, mirroring Kubernetes maxSurge — e.g.
// "25%" or an absolute "10". An empty string, or a value that resolves to zero
// ("0"/"0%"), returns (nil, nil) so the feature stays off; a negative or malformed
// value returns an error so startup fails fast rather than misbehaving mid-drain.
func ParseZeroSurgeOverride(raw string) (*intstr.IntOrString, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // a nil pointer signals the override is disabled; not an error
	}
	v := intstr.Parse(raw)
	// Validate against a representative base so a bad percentage/negative surfaces
	// now rather than on the first drain; the base value itself is irrelevant.
	switch _, err := surgeFromValue(v, 100); {
	case errors.Is(err, errMaxSurgeZero):
		return nil, nil //nolint:nilnil // a value that resolves to zero disables the override; not an error
	case err != nil:
		return nil, err
	}
	return &v, nil
}

// recordZeroMaxSurge sets the zero-maxSurge workload gauge for the target: 1 when its
// rollout maxSurge resolves to 0 (an explicit maxSurge: 0 or a Recreate strategy),
// else 0 — so the series sum is the cluster-wide count. Workloads with an unset
// RollingUpdate strategy are NOT counted as zero (Kubernetes defaults to 25%).
func recordZeroMaxSurge(target Surger, namespace, name string) {
	value := 0.0
	if isZeroSurge(target.GetMaxSurge()) {
		value = 1
	}
	metrics.ZeroMaxSurgeWorkloadGauge.WithLabelValues(namespace, name).Set(value)
}

// logZeroSurgeOverride emits a structured log when the fleet-wide zero-maxSurge override
// drives a surge — i.e. the target's rollout maxSurge resolves to 0 and an override is set.
func (r *EvictionAutoScalerReconciler) logZeroSurgeOverride(ctx context.Context, target Surger, surgeTarget int32) {
	maxSurge := target.GetMaxSurge()
	if r.ZeroSurgeOverride != nil && isZeroSurge(maxSurge) {
		log.FromContext(ctx).Info(fmt.Sprintf("zero-maxSurge override surging %s/%s during drain (rollout maxSurge %q)",
			target.Obj().GetNamespace(), target.Obj().GetName(), maxSurge.String()), "surgeTarget", surgeTarget)
	}
}

// isZeroSurge reports whether a maxSurge value resolves to no surge — an int 0 or
// a 0% (e.g. an explicit maxSurge: 0 or a Recreate strategy). An unset RollingUpdate
// strategy returns 25% (the Kubernetes default) and is NOT considered zero surge.
func isZeroSurge(maxSurge intstr.IntOrString) bool {
	_, err := surgeFromValue(maxSurge, 1)
	return errors.Is(err, errMaxSurgeZero)
}

// surgeFromValue resolves an int-or-percentage surge value against minReplicas.
// An int is added directly; a percentage (a string ending in "%") is applied to
// minReplicas and rounded up. A zero value yields errMaxSurgeZero; a negative value
// yields errNegativeSurge; a string that is not a valid "<n>%" percentage yields
// errInvalidPercentage.
func surgeFromValue(surge intstr.IntOrString, minReplicas int32) (int32, error) {
	if surge.Type == intstr.Int {
		switch {
		case surge.IntVal < 0:
			return minReplicas, fmt.Errorf("%w: %d", errNegativeSurge, surge.IntVal)
		case surge.IntVal == 0:
			return minReplicas, errMaxSurgeZero
		}
		return minReplicas + surge.IntVal, nil
	}

	if surge.Type == intstr.String {
		// A string surge value must be a percentage, e.g. "10%". Kubernetes stores a
		// numeric maxSurge as an intstr Int (handled above), so a String type is always
		// a percentage — reject a bare number like "10" as malformed.
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
			return minReplicas, errMaxSurgeZero
		}
		return minReplicas + int32(math.Ceil((float64(minReplicas)*float64(percentage))/100.0)), nil
	}

	// Unreachable for well-formed intstr values, but handle gracefully
	return minReplicas, errMaxSurgeZero
}
