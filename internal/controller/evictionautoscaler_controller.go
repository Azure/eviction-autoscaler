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

	// Handle deletion: if we still hold the PDB-floor restore finalizer, restore
	// the partner's PDB before letting the CR be removed.
	if !EvictionAutoScaler.DeletionTimestamp.IsZero() {
		return r.reconcileFloorOnDeletion(ctx, EvictionAutoScaler)
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
		return r.restoreStaleFloor(ctx, EvictionAutoScaler, pdb)
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

	// surgeTarget = minReplicas + displaced, capped at minReplicas + maxSurge.
	// If displaced == 0 the formula yields minReplicas, so no scale-up fires and
	// we fall through to the cooldown/scale-down path — which is correct.
	maxSurgeTarget, surgeErr := calculateSurge(ctx, target, EvictionAutoScaler.Status.MinReplicas)
	if surgeErr != nil {
		switch {
		case errors.Is(surgeErr, errMaxSurgeZero):
			// maxSurge is 0 (explicit or not configured) — can't surge, degrade.
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
		return r.handleScaleDown(ctx, EvictionAutoScaler, pdb, target, surgeApplier)
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
func (r *EvictionAutoScalerReconciler) handleScaleDown(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget, target Surger, surgeApplier SurgeApplier) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Track scaling opportunity
	metrics.ScalingOpportunityCounter.WithLabelValues(eas.Namespace, eas.Spec.TargetName, metrics.ScaleDownAction, metrics.CooldownElapsedSignal).Inc()

	// Revert the target to the original state now that disruptions are allowed again.
	if err := surgeApplier.RevertSurge(ctx, eas.Status.MinReplicas); err != nil {
		return ctrl.Result{}, err
	}

	// Restore the partner's PDB (and drop the finalizer) now the drain is done.
	if err := r.revertPDBFloor(ctx, eas, pdb); err != nil {
		return ctrl.Result{}, err
	}

	// Track actual scaling action
	metrics.ActualScalingCounter.WithLabelValues(eas.Namespace, eas.Spec.TargetName, metrics.ScaleDownAction).Inc()

	logger.Info(fmt.Sprintf("Reverted surge on %s %s/%s (via %s)", eas.Spec.TargetKind, target.Obj().GetNamespace(), target.Obj().GetName(), surgeApplier.Name()))
	logger.Info(fmt.Sprintf("TargetGeneration moving from %d->%d", eas.Status.TargetGeneration, target.Obj().GetGeneration()))
	eas.Status.TargetGeneration = target.Obj().GetGeneration()
	eas.Status.LastEviction = eas.Spec.LastEviction //we could still keep a log here if thats useful
	logger.Info(fmt.Sprintf("Handled eviction %s", eas.Spec.LastEviction))

	ready(&eas.Status.Conditions, "Reconciled", "evictions hit cooldown so scaled down")
	return ctrl.Result{}, r.Status().Update(ctx, eas)
}

// reconcileFloorOnDeletion restores the partner PDB (best effort) and drops the
// restore finalizer when the CR is being deleted, so a mid-drain deletion never
// leaves a partner PDB pinned.
func (r *EvictionAutoScalerReconciler) reconcileFloorOnDeletion(ctx context.Context, eas *myappsv1.EvictionAutoScaler) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(eas, PDBFloorFinalizer) {
		return ctrl.Result{}, nil
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
			r.recordPDBWarning(pdb, "PDBFloorRestoreFailed",
				fmt.Sprintf("cannot restore PDB on EvictionAutoScaler deletion (%v); PDB may remain pinned and needs manual review", rerr))
		} else if changed {
			if uerr := r.Update(ctx, pdb); uerr != nil {
				return ctrl.Result{}, uerr
			}
		}
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(eas, PDBFloorFinalizer)
	if err := r.Update(ctx, eas); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// restoreStaleFloor reverts a PDB left mutated past the stale window and persists
// the cleared floor on the CR status.
func (r *EvictionAutoScalerReconciler) restoreStaleFloor(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) (ctrl.Result, error) {
	if err := r.revertPDBFloor(ctx, eas, pdb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.Status().Update(ctx, eas)
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
)

// calculateSurge returns the maximum replica count after surge (minReplicas + maxSurge).
// Returns a sentinel error to distinguish:
//   - errMaxSurgeZero: maxSurge resolves to 0 (explicitly set or not configured)
//   - errInvalidPercentage: percentage string could not be parsed
func calculateSurge(_ context.Context, target Surger, minrepicas int32) (int32, error) {

	surge := target.GetMaxSurge()
	if surge.Type == intstr.Int {
		if surge.IntVal == 0 {
			return minrepicas, errMaxSurgeZero
		}
		return minrepicas + surge.IntVal, nil
	}

	if surge.Type == intstr.String {
		percentageStr := strings.TrimSuffix(surge.StrVal, "%")
		percentage, err := strconv.Atoi(percentageStr)
		if err != nil {
			return minrepicas, fmt.Errorf("%w: %q: %w", errInvalidPercentage, surge.StrVal, err)
		}
		if percentage == 0 {
			return minrepicas, errMaxSurgeZero
		}
		return minrepicas + int32(math.Ceil((float64(minrepicas)*float64(percentage))/100.0)), nil
	}

	// Unreachable for well-formed intstr values, but handle gracefully
	return minrepicas, errMaxSurgeZero
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

// recordPDBWarning emits a Warning event on the PDB if an event recorder is
// configured. Nil-safe so tests (and any setup without a recorder) don't panic.
func (r *EvictionAutoScalerReconciler) recordPDBWarning(pdb *policyv1.PodDisruptionBudget, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(pdb, corev1.EventTypeWarning, reason, message)
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
		r.recordPDBWarning(pdb, "PDBFloorRestoreFailed",
			fmt.Sprintf("cannot restore PDB: snapshot annotation %q missing while pinned floor is %d; leaving minAvailable in place", AnnotationOriginalPDBSpec, floor))
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
