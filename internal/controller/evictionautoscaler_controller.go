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

	corev1 "k8s.io/api/core/v1"
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

// EvictionAutoScalerReconciler reconciles a EvictionAutoScaler object
type EvictionAutoScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Filter   filter
	// PDBFloorMutationEnabled is the fleet-wide master switch for the PDB-floor
	// mutation feature. When false (the default) the controller never pins/mutates
	// a partner PDB's minAvailable floor, regardless of per-namespace opt-in. Set
	// via the ENABLE_PDB_FLOOR_MUTATION controller env var, validated and logged in
	// main.go so a misconfiguration fails fast at startup rather than silently
	// disabling the feature.
	PDBFloorMutationEnabled bool
	// StaleMutationWindow bounds how long a PDB may stay mutated before the backstop
	// restores it unconditionally. Set from PDB_MUTATION_STALE_WINDOW in main.go
	// (defaulting to DefaultStaleMutationWindow); a zero value disables the backstop.
	StaleMutationWindow time.Duration
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

	// Handle deletion: if we still hold the PDB-floor restore finalizer, restore
	// the partner's PDB before letting the CR be removed.
	if !EvictionAutoScaler.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileFloorOnDeletion(ctx, EvictionAutoScaler)
	}

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

	// Stale-window backstop: restore any PDB we left mutated longer than the
	// stale window (e.g. the CR was force-deleted without the finalizer running).
	if isMutationStale(pdb, time.Now(), r.StaleMutationWindow) {
		logger.Info("PDB floor mutation is stale, restoring partner spec", "pdb", pdb.Name)
		return ctrl.Result{}, r.restoreStaleFloor(ctx, EvictionAutoScaler, pdb)
	}

	if EvictionAutoScaler.Spec.TargetName == "" {
		degraded(&EvictionAutoScaler.Status.Conditions, "EmptyTarget", "no specified target")
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

	// Have we processed all evictions okay don't do anything else
	if EvictionAutoScaler.Spec.LastEviction == EvictionAutoScaler.Status.LastEviction {
		// Drain is fully handled; if the PDB is still mutated (or we still hold a
		// pinned floor), restore the partner PDB. Keyed off the PDB's own state so
		// a pin whose status write never landed is still cleaned up. Done before
		// the re-mutation guard so we don't re-pin and immediately restore.
		if err := r.restoreFloorIfDrainHandled(ctx, EvictionAutoScaler, pdb); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("No unhandled eviction ", "pdbname", pdb.Name)
		ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "no unhandled eviction")
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
	}

	// Re-mutation guard: while a floor is pinned and the drain is still active, keep
	// defending it against a partner overwriting the PDB mid-drain (the mutated PDB
	// is the sole gate). The floor is read from the PDB annotation first (survives a
	// lost CR status write) and falls back to the CR status (survives a partner
	// stripping the PDB annotations), so the guard fires under either failure. Gated
	// on the drain still blocking (DisruptionsAllowed==0) so we don't re-pin a PDB
	// that is about to be reverted this same reconcile.
	if err := r.defendPinnedFloor(ctx, EvictionAutoScaler, pdb); err != nil {
		return ctrl.Result{}, err
	}

	// Log current state before checks
	logger.Info(fmt.Sprintf("Checking PDB for %s: DisruptionsAllowed=%d, MinReplicas=%d", pdb.Name, pdb.Status.DisruptionsAllowed, EvictionAutoScaler.Status.MinReplicas))

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
			degraded(&EvictionAutoScaler.Status.Conditions, "UnsupportedAutoscalerConfiguration", surgeErr.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		default:
			// Parse error or unexpected — degrade.
			degraded(&EvictionAutoScaler.Status.Conditions, "InvalidSurgeConfiguration", surgeErr.Error())
			return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
		}
	} else if pdb.Status.DisruptionsAllowed == 0 {
		return r.handleBlockedDrain(ctx, EvictionAutoScaler, pdb, target, surgeApplier, maxSurgeTarget)
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
		return ctrl.Result{}, r.handleScaleDown(ctx, EvictionAutoScaler, pdb, target, surgeApplier)
	}

	//could get here if a scale up/down was not needed because we never hit allowed diruptios == 0.
	EvictionAutoScaler.Status.LastEviction = EvictionAutoScaler.Spec.LastEviction //we could still keep a log here if thats useful
	ready(&EvictionAutoScaler.Status.Conditions, "Reconciled", "last eviction did not need scaling")
	logger.Info(fmt.Sprintf("Handled eviction %s", EvictionAutoScaler.Spec.LastEviction))
	return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler) //should we go rety in case there is also an eviction or just wait till the next eviction
}

// handleBlockedDrain scales the target up during a PDB-blocked drain: it pins the
// PDB floor (when enabled) so the surge converts into DisruptionsAllowed, applies the
// demand-driven surge, and records the pinned floor on the CR status.
func (r *EvictionAutoScalerReconciler) handleBlockedDrain(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget, target Surger, surgeApplier SurgeApplier, maxSurgeTarget int32) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	displaced, countErr := countPodsOnCordoned(ctx, r.Client, pdb)
	if countErr != nil {
		logger.Error(countErr, "failed to count displaced pods on cordoned nodes")
		return ctrl.Result{}, countErr
	}

	surgeTarget := eas.Status.MinReplicas + displaced
	if surgeTarget > maxSurgeTarget {
		logger.Info("Displaced pods exceed maxSurge capacity, capping surge", "pdb", pdb.Name, "displaced", displaced, "maxSurgeTarget", maxSurgeTarget)
		surgeTarget = maxSurgeTarget
	}

	if target.GetReplicas() >= surgeTarget {
		// Already scaled up but the PDB is still blocking — likely waiting for new pods to become ready.
		logger.Info("Have already scaled up to handle evictions, waiting for PDB to allow disruptions before reverting",
			"pdb", pdb.Name, "target", eas.Spec.TargetName)
		ready(&eas.Status.Conditions, "Reconciled", "Have already scaled up to handle evictions, waiting for PDB to allow disruptions before reverting")
		return ctrl.Result{RequeueAfter: cooldown}, r.Status().Update(ctx, eas)
	}

	logger.Info("No disruptions allowed, scaling up", "pdb", pdb.Name, "lastEviction", eas.Spec.LastEviction, "strategy", surgeApplier.Name(), "displaced", displaced, "surgeTarget", surgeTarget)

	// Surface when the fleet-wide zero-maxSurge override drives a surge — we are
	// deliberately surging a workload whose author set an explicit (often 0)
	// rollout maxSurge. Logged only when a surge actually fires.
	r.logZeroSurgeOverride(ctx, target, surgeTarget)

	metrics.BlockedEvictionCounter.WithLabelValues(eas.Namespace, pdb.Name).Inc()
	signalLabel := metrics.GetScalingSignal(pdb)
	metrics.ScalingOpportunityCounter.WithLabelValues(eas.Namespace, eas.Spec.TargetName, metrics.ScaleUpAction, signalLabel).Inc()

	// Pin an absolute PDB floor before surging so the surge headroom converts into
	// DisruptionsAllowed instead of being absorbed by a floor that tracks the surged
	// replica count. Derived from the partner's PDB spec at the baseline replica count
	// (Status.MinReplicas) and held for the whole drain.
	floor, pinned, pinErr := r.pinFloorBeforeSurge(ctx, eas, pdb)
	if pinErr != nil {
		logger.Error(pinErr, "failed to pin PDB floor before surge", "pdb", pdb.Name)
		return ctrl.Result{}, pinErr
	}

	if err := surgeApplier.ApplySurge(ctx, surgeTarget); err != nil {
		logger.Error(err, "failed to apply surge", "kind", eas.Spec.TargetKind, "targetname", eas.Spec.TargetName, "strategy", surgeApplier.Name())
		return ctrl.Result{}, err
	}

	metrics.ActualScalingCounter.WithLabelValues(eas.Namespace, eas.Spec.TargetName, metrics.ScaleUpAction).Inc()

	logger.Info(fmt.Sprintf("Scaled up %s %s/%s to %d replicas (via %s)", eas.Spec.TargetKind, target.Obj().GetNamespace(), target.Obj().GetName(), surgeTarget, surgeApplier.Name()))
	logger.Info(fmt.Sprintf("TargetGeneration moving from %d->%d", eas.Status.TargetGeneration, target.Obj().GetGeneration()))
	// Save ResourceVersion to the CR status; this triggers another reconcile.
	eas.Status.TargetGeneration = target.Obj().GetGeneration()
	// Do not update LastEviction; keep reconciling until scale-down.
	if pinned {
		eas.Status.PinnedPDBFloor = &floor
	}
	ready(&eas.Status.Conditions, "Reconciled", "eviction with scale up")
	return ctrl.Result{RequeueAfter: cooldown}, r.Status().Update(ctx, eas)
}

// handleScaleDown reverts the surge and restores the partner PDB once the drain has
// finished (cooldown elapsed, disruptions allowed again), marking the eviction handled.
func (r *EvictionAutoScalerReconciler) handleScaleDown(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget, target Surger, surgeApplier SurgeApplier) error {
	logger := log.FromContext(ctx)

	// Track scaling opportunity
	metrics.ScalingOpportunityCounter.WithLabelValues(eas.Namespace, eas.Spec.TargetName, metrics.ScaleDownAction, metrics.CooldownElapsedSignal).Inc()

	// Order matters and is a deliberate fail-safe: revert the surge (scale back to
	// MinReplicas) BEFORE restoring the partner PDB. If the PDB restore then fails,
	// we requeue with the deployment already at baseline but the floor still pinned —
	// briefly over-restrictive (a new drain may need one extra requeue), which
	// converges on the next reconcile. The opposite order could momentarily leave a
	// permissive PDB over an under-replicated deployment (fail-open), so we prefer
	// the over-restrictive failure mode.
	// Revert the target to the original state now that disruptions are allowed again.
	if err := surgeApplier.RevertSurge(ctx, eas.Status.MinReplicas); err != nil {
		return err
	}

	// Restore the partner's PDB (and drop the finalizer) now the drain is done.
	if err := r.revertPDBFloor(ctx, eas, pdb); err != nil {
		return err
	}

	// Track actual scaling action
	metrics.ActualScalingCounter.WithLabelValues(eas.Namespace, eas.Spec.TargetName, metrics.ScaleDownAction).Inc()

	logger.Info(fmt.Sprintf("Reverted surge on %s %s/%s (via %s)", eas.Spec.TargetKind, target.Obj().GetNamespace(), target.Obj().GetName(), surgeApplier.Name()))
	logger.Info(fmt.Sprintf("TargetGeneration moving from %d->%d", eas.Status.TargetGeneration, target.Obj().GetGeneration()))
	eas.Status.TargetGeneration = target.Obj().GetGeneration()
	eas.Status.LastEviction = eas.Spec.LastEviction //we could still keep a log here if thats useful
	logger.Info(fmt.Sprintf("Handled eviction %s", eas.Spec.LastEviction))

	ready(&eas.Status.Conditions, "Reconciled", "evictions hit cooldown so scaled down")
	return r.Status().Update(ctx, eas)
}

// reconcileFloorOnDeletion restores the partner PDB (best effort) and drops the
// restore finalizer when the CR is being deleted, so a mid-drain deletion never
// leaves a partner PDB pinned.
func (r *EvictionAutoScalerReconciler) reconcileFloorOnDeletion(ctx context.Context, eas *myappsv1.EvictionAutoScaler) error {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(eas, PDBFloorFinalizer) {
		return nil
	}
	pdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, types.NamespacedName{Name: eas.Name, Namespace: eas.Namespace}, pdb)
	switch {
	case err == nil:
		if changed, rerr := restorePDBSpec(pdb); rerr != nil {
			// Can't restore a snapshot we can't parse — do NOT block CR deletion on
			// it. Log and proceed to drop the finalizer.
			logger.Error(rerr, "failed to restore PDB on deletion; removing finalizer anyway to avoid a stuck CR", "pdb", pdb.Name)
			metrics.PDBFloorRestoreFailureCounter.WithLabelValues(pdb.Namespace, pdb.Name, metrics.PDBFloorRestoreReasonDeletion).Inc()
		} else if changed {
			if uerr := r.Update(ctx, pdb); uerr != nil {
				return uerr
			}
		}
	case !apierrors.IsNotFound(err):
		return err
	}
	controllerutil.RemoveFinalizer(eas, PDBFloorFinalizer)
	if err := r.Update(ctx, eas); err != nil {
		return err
	}
	return nil
}

// restoreStaleFloor reverts a PDB left mutated past the stale window and persists
// the cleared floor on the CR status.
func (r *EvictionAutoScalerReconciler) restoreStaleFloor(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) error {
	if err := r.revertPDBFloor(ctx, eas, pdb); err != nil {
		return err
	}
	return r.Status().Update(ctx, eas)
}

// restoreFloorIfDrainHandled restores the partner PDB once the drain is fully
// handled, keyed off the PDB's own mutation state (or a still-recorded floor) so a
// pin whose status write never landed is still cleaned up.
func (r *EvictionAutoScalerReconciler) restoreFloorIfDrainHandled(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) error {
	if !isMutated(pdb) && eas.Status.PinnedPDBFloor == nil {
		return nil
	}
	log.FromContext(ctx).Info("Drain handled, restoring partner PDB", "pdb", pdb.Name)
	return r.revertPDBFloor(ctx, eas, pdb)
}

// defendPinnedFloor re-pins the PDB floor if a partner overwrote the PDB while the
// drain is still blocking. The floor is read from the PDB annotation first (survives
// a lost CR status write) and falls back to the CR status (survives a partner
// stripping the PDB annotations). No-op when no floor is known, the drain is no
// longer blocking, or the PDB already carries the floor.
func (r *EvictionAutoScalerReconciler) defendPinnedFloor(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) error {
	pinnedFloor, floorKnown := pinnedFloorFromPDB(pdb)
	if !floorKnown && eas.Status.PinnedPDBFloor != nil {
		pinnedFloor, floorKnown = *eas.Status.PinnedPDBFloor, true
	}
	if !floorKnown || pdb.Status.DisruptionsAllowed != 0 || pdbCarriesFloor(pdb, pinnedFloor) {
		return nil
	}
	allowed, err := r.pdbFloorMutationAllowed(ctx, eas.Namespace)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	log.FromContext(ctx).Info("Re-pinning PDB floor after partner change", "pdb", pdb.Name, "floor", pinnedFloor)
	_, _, err = r.ensurePDBFloor(ctx, eas, pdb)
	return err
}

// pinFloorBeforeSurge pins the PDB floor before a surge when the feature is enabled
// for the namespace. Returns the pinned floor and whether a pin was applied; a no-op
// (0, false, nil) when the feature is disabled for the namespace.
//
// Absolute-minAvailable PDBs are pinned too, for a consistent mid-drain contract: the
// floor is claimed (CR finalizer + status) so a partner edit mid-drain is defended and
// deferred to restore, exactly like maxUnavailable / percentage PDBs. This costs no PDB
// spec churn while the PDB already carries the floor — ensurePDBFloor's re-mutation
// guard (pdbCarriesFloor) short-circuits the snapshot+rewrite until the partner deviates.
func (r *EvictionAutoScalerReconciler) pinFloorBeforeSurge(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) (int32, bool, error) {
	allowed, err := r.pdbFloorMutationAllowed(ctx, eas.Namespace)
	if err != nil {
		return 0, false, err
	}
	if !allowed {
		return 0, false, nil
	}
	return r.ensurePDBFloor(ctx, eas, pdb)
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
				if ue.ObjectOld.GetGeneration() != ue.ObjectNew.GetGeneration() {
					return true
				}
				// Deletion of a finalizer-bearing CR arrives as an Update that sets
				// deletionTimestamp (which does not bump generation). Admit it so the
				// PDB-floor restore finalizer can run.
				if !ue.ObjectNew.GetDeletionTimestamp().IsZero() {
					return true
				}
				// Admit finalizer add/remove transitions (also generation-neutral).
				return !equalStringSets(ue.ObjectOld.GetFinalizers(), ue.ObjectNew.GetFinalizers())
			},
		}).
		Complete(r)
}

// equalStringSets reports whether a and b contain the same elements (order-insensitive).
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		if seen[s] == 0 {
			return false
		}
		seen[s]--
	}
	return true
}

// pdbFloorMutationAllowed reports whether PDB-floor mutation may run for the given
// namespace. It requires BOTH the master env switch (ENABLE_PDB_FLOOR_MUTATION)
// and a per-namespace opt-in annotation on the Namespace object, so the feature
// never silently rewrites a user-authored PDB the operator/namespace-owner has
// not explicitly consented to.
func (r *EvictionAutoScalerReconciler) pdbFloorMutationAllowed(ctx context.Context, namespace string) (bool, error) {
	if !r.PDBFloorMutationEnabled {
		return false, nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		return false, err
	}
	val, ok := ns.Annotations[AnnotationNamespacePDBFloorOptIn]
	if !ok {
		return false, nil
	}
	optIn, err := strconv.ParseBool(val)
	if err != nil {
		// An unparseable value is treated as "not opted in" rather than an error,
		// so a typo cannot break reconciliation of the eviction flow.
		return false, nil
	}
	return optIn, nil
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

// ensurePDBFloor pins the target PDB to an absolute minAvailable floor for the
// duration of a drain so the surge headroom converts into DisruptionsAllowed.
//
// The floor F is captured once — derived from the partner's PDB spec at the
// baseline replica count (Status.MinReplicas) — and then persisted on the CR
// status by the caller; on subsequent reconciles F is read back from the status
// so it is never recomputed against surged replicas.
// The partner's current spec is snapshotted onto the PDB (so a mid-drain partner
// change is preserved for revert) and the PDB is re-pinned whenever it no longer
// carries F (partner-overwrite protection). A restore finalizer is added so the
// partner PDB is restored even if the CR is deleted mid-drain.
//
// Returns the pinned floor, or (0,false,nil) if there is nothing safe to pin
// (the PDB expresses no availability requirement at baseline, so F resolves to
// <= 0), in which case the caller should not record a floor.
func (r *EvictionAutoScalerReconciler) ensurePDBFloor(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) (int32, bool, error) {
	var floor int32
	pinnedFloor, hasPinnedFloor := pinnedFloorFromPDB(pdb)
	switch {
	case hasPinnedFloor:
		// Existing pin: the floor recorded on the PDB is the durable source of
		// truth (survives a lost CR status write).
		floor = pinnedFloor
	case eas.Status.PinnedPDBFloor != nil:
		floor = *eas.Status.PinnedPDBFloor
	default:
		// First capture: derive the floor synchronously from the partner's PDB spec
		// at the baseline replica count (Status.MinReplicas). This mirrors what the
		// built-in controller writes to Status.DesiredHealthy, but without depending
		// on that asynchronously-populated field — which can lag or read 0 at the
		// moment of the first surge and silently skip the pin. MinReplicas is held
		// stable across the surge, so the derived floor stays correct even once
		// replicas inflate.
		// Limitation: for an autoscaler running above its min (MinReplicas < live
		// replicas), the floor reflects the min rather than the elevated count.
		dh, err := desiredHealthyAt(pdb.Spec, eas.Status.MinReplicas)
		if err != nil {
			return 0, false, err
		}
		floor = dh
	}
	if floor <= 0 {
		// PDB expresses no availability requirement at baseline — nothing to protect.
		return 0, false, nil
	}

	// Add the restore finalizer first. Update refreshes the object (including
	// status) from the server, so we deliberately set no in-memory status before
	// this point that we need to keep.
	if controllerutil.AddFinalizer(eas, PDBFloorFinalizer) {
		if err := r.Update(ctx, eas); err != nil {
			return 0, false, err
		}
	}

	// Pin the floor if the PDB is not already carrying it. When it is not, the
	// current spec is the partner's intent (original, or a mid-drain change), so
	// snapshot it before overwriting.
	if !pdbCarriesFloor(pdb, floor) {
		if err := snapshotPDBSpec(pdb); err != nil {
			return 0, false, err
		}
		pinPDBFloor(pdb, floor)
		if err := r.Update(ctx, pdb); err != nil {
			return 0, false, err
		}
	}

	return floor, true, nil
}

// desiredHealthyAt computes a PDB's desired-healthy count at a given replica count,
// mirroring the built-in disruption controller: minAvailable (an int, or a percentage
// of replicas rounded up), or replicas minus maxUnavailable (percentage rounded up),
// clamped at zero. It lets the pinned floor be derived synchronously from the PDB spec
// plus the baseline replica count, instead of the asynchronously-populated
// Status.DesiredHealthy. A PDB expressing no budget yields 0.
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

// revertPDBFloor restores the partner's PDB spec, removes the restore finalizer
// and clears the persisted floor (in memory — the caller persists it via a
// status update). Safe to call when nothing is pinned.
//
// It issues up to two writes across two objects — a PDB Update (restore or, in the
// snapshot-missing path, marker cleanup) and a CR Update to drop the finalizer.
// Each is idempotent, so a partial failure is safe and converges on the next
// reconcile: restorePDBSpec returns (false, nil) once the snapshot annotation is
// already cleared, and RemoveFinalizer is a no-op once the finalizer is gone — so a
// conflict on either write simply re-runs without leaving the PDB or CR inconsistent.
func (r *EvictionAutoScalerReconciler) revertPDBFloor(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) error {
	changed, err := restorePDBSpec(pdb)
	if err != nil {
		// Corrupt snapshot — leave the mutated spec in place for an operator
		// rather than dropping the partner's config.
		return err
	}
	if changed {
		if err := r.Update(ctx, pdb); err != nil {
			return err
		}
	} else if floor, ok := pinnedFloorFromPDB(pdb); ok {
		// The PDB still carries our pinned floor but the snapshot annotation is
		// gone (e.g. a partner stripped only original-pdb-spec), so we cannot
		// restore their original spec. Surface it to an operator and stop claiming
		// the pin by clearing our marker annotation; the (stricter) floor spec is
		// left in place as the fail-safe direction.
		log.FromContext(ctx).Error(nil, "cannot restore PDB: snapshot annotation missing while pinned floor is set; leaving minAvailable in place",
			"pdb", pdb.Name, "snapshotAnnotation", AnnotationOriginalPDBSpec, "pinnedFloor", floor)
		metrics.PDBFloorRestoreFailureCounter.WithLabelValues(pdb.Namespace, pdb.Name, metrics.PDBFloorRestoreReasonSnapshotMissing).Inc()
		delete(pdb.Annotations, AnnotationPinnedFloor)
		if err := r.Update(ctx, pdb); err != nil {
			return err
		}
	}
	if controllerutil.RemoveFinalizer(eas, PDBFloorFinalizer) {
		if err := r.Update(ctx, eas); err != nil {
			return err
		}
	}
	// Set after the finalizer Update above, which would otherwise refresh the
	// status back from the server.
	eas.Status.PinnedPDBFloor = nil
	return nil
}
