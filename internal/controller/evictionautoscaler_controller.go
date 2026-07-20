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
		if controllerutil.ContainsFinalizer(EvictionAutoScaler, PDBFloorFinalizer) {
			pdb := &policyv1.PodDisruptionBudget{}
			err := r.Get(ctx, types.NamespacedName{Name: EvictionAutoScaler.Name, Namespace: EvictionAutoScaler.Namespace}, pdb)
			switch {
			case err == nil:
				if changed, rerr := restorePDBSpec(pdb); rerr != nil {
					// Can't restore a snapshot we can't parse — do NOT block CR
					// deletion on it. Log and proceed to drop the finalizer.
					logger.Error(rerr, "failed to restore PDB on deletion; removing finalizer anyway to avoid a stuck CR", "pdb", pdb.Name)
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
			controllerutil.RemoveFinalizer(EvictionAutoScaler, PDBFloorFinalizer)
			if err := r.Update(ctx, EvictionAutoScaler); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
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
	if isMutationStale(pdb, time.Now()) {
		logger.Info("PDB floor mutation is stale, restoring partner spec", "pdb", pdb.Name)
		if err := r.revertPDBFloor(ctx, EvictionAutoScaler, pdb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.Status().Update(ctx, EvictionAutoScaler)
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
		if isMutated(pdb) || EvictionAutoScaler.Status.PinnedPDBFloor != nil {
			logger.Info("Drain handled, restoring partner PDB", "pdb", pdb.Name)
			if err := r.revertPDBFloor(ctx, EvictionAutoScaler, pdb); err != nil {
				return ctrl.Result{}, err
			}
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
	pinnedFloor, floorKnown := pinnedFloorFromPDB(pdb)
	if !floorKnown && EvictionAutoScaler.Status.PinnedPDBFloor != nil {
		pinnedFloor, floorKnown = *EvictionAutoScaler.Status.PinnedPDBFloor, true
	}
	if floorKnown && pdb.Status.DisruptionsAllowed == 0 && !pdbCarriesFloor(pdb, pinnedFloor) {
		allowed, allowErr := r.pdbFloorMutationAllowed(ctx, EvictionAutoScaler.Namespace)
		if allowErr != nil {
			return ctrl.Result{}, allowErr
		}
		if allowed {
			logger.Info("Re-pinning PDB floor after partner change", "pdb", pdb.Name, "floor", pinnedFloor)
			if _, _, err := r.ensurePDBFloor(ctx, EvictionAutoScaler, pdb, true); err != nil {
				return ctrl.Result{}, err
			}
		}
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

		// Pin an absolute PDB floor before surging so the surge headroom converts
		// into DisruptionsAllowed instead of being absorbed by a floor that tracks
		// the surged replica count. Captured pre-surge (DesiredHealthy) and held.
		// Requires the master flag AND the namespace opt-in; only a first capture
		// at baseline replicas.
		var floor int32
		var pinned bool
		allowed, allowErr := r.pdbFloorMutationAllowed(ctx, EvictionAutoScaler.Namespace)
		if allowErr != nil {
			return ctrl.Result{}, allowErr
		}
		if allowed {
			atBaseline := target.GetReplicas() == EvictionAutoScaler.Status.MinReplicas
			var pinErr error
			floor, pinned, pinErr = r.ensurePDBFloor(ctx, EvictionAutoScaler, pdb, atBaseline)
			if pinErr != nil {
				logger.Error(pinErr, "failed to pin PDB floor before surge", "pdb", pdb.Name)
				return ctrl.Result{}, pinErr
			}
		}

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
		if pinned {
			EvictionAutoScaler.Status.PinnedPDBFloor = &floor
		}
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

		// Restore the partner's PDB (and drop the finalizer) now the drain is done.
		if err := r.revertPDBFloor(ctx, EvictionAutoScaler, pdb); err != nil {
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
	if !pdbFloorMutationEnabled {
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
// The floor F is captured once — from the PDB's pre-surge DesiredHealthy — and
// then persisted on the CR status by the caller; on subsequent reconciles F is
// read back from the status so it is never recomputed against surged replicas.
// The partner's current spec is snapshotted onto the PDB (so a mid-drain partner
// change is preserved for revert) and the PDB is re-pinned whenever it no longer
// carries F (partner-overwrite protection). A restore finalizer is added so the
// partner PDB is restored even if the CR is deleted mid-drain.
//
// Returns the pinned floor, or (0,false,nil) if there is nothing safe to pin
// (floor resolves to <= 0, or a first capture is requested when the target is
// not at its baseline replica count), in which case the caller should not
// record a floor.
func (r *EvictionAutoScalerReconciler) ensurePDBFloor(ctx context.Context, eas *myappsv1.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget, atBaseline bool) (int32, bool, error) {
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
		// First capture. DesiredHealthy is only the partner's true floor when the
		// target is at its baseline replica count; if replicas are already surged
		// (e.g. an overlapping drain), DesiredHealthy for a percentage PDB would be
		// inflated. Refuse to capture an inflated floor.
		if !atBaseline {
			return 0, false, nil
		}
		floor = pdb.Status.DesiredHealthy
	}
	if floor <= 0 {
		// Nothing to protect (e.g. DesiredHealthy not yet populated) — skip.
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
