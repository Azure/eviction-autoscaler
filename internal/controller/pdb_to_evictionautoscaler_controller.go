package controllers

import (
	"context"
	"fmt"

	types "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8s_types "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var errOwnerNotFound error = fmt.Errorf("owner not found")

// PDBToEvictionAutoScalerReconciler reconciles a PodDisruptionBudget object.
type PDBToEvictionAutoScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Filter   filter
}

// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;create;watch;update
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;update;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;update;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

// Reconcile reads the state of the cluster for a PDB and creates/deletes EvictionAutoScalers accordingly.
func (r *PDBToEvictionAutoScalerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	defer recordPanic("pdbtoevictionautoscaler", req.Namespace, &req.Name)
	logger := log.FromContext(ctx)
	logger = logger.WithValues("pdb", req.Name, "namespace", req.Namespace)
	ctx = log.IntoContext(ctx, logger)
	// Fetch the PDB and EAS independently (they share the request key); tolerate NotFound
	// so a terminating EAS whose PDB is already gone can still shed its finalizer.
	var pdb policyv1.PodDisruptionBudget
	err := r.Get(ctx, req.NamespacedName, &pdb)
	if err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	pdbFound := err == nil

	var EvictionAutoScaler types.EvictionAutoScaler
	easErr := r.Get(ctx, req.NamespacedName, &EvictionAutoScaler)
	if easErr != nil && !apierrors.IsNotFound(easErr) {
		return reconcile.Result{}, easErr
	}
	easFound := easErr == nil

	// Teardown-first: restore a terminating EAS's partner PDB before the ownership/namespace
	// early-returns below, so a mid-drain CR delete can never strand a pinned PDB.
	if easFound && !EvictionAutoScaler.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, r.reconcileEASDeletion(ctx, &EvictionAutoScaler, &pdb, pdbFound)
	}

	// Orphaned mutation (EAS gone, PDB still pinned): restore, then fall through to recreate.
	if pdbFound && !easFound && pdbCarriesFloorAnnotations(&pdb) {
		if err := r.actuatePDBFloor(ctx, &pdb, nil); err != nil {
			return reconcile.Result{}, err
		}
	}

	if !pdbFound {
		return reconcile.Result{}, nil
	}

	// Handle ownership transfer based on ownedBy annotation
	err = r.handleOwnershipTransfer(ctx, &pdb)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Update PDB metrics to check if this PDB was created by our deployment controller
	createdByUsStr := metrics.GetPDBCreatedByUsLabel(pdb.Annotations)
	// Track PDB existence
	metrics.PDBCounter.WithLabelValues(pdb.Namespace, createdByUsStr).Inc()

	// Check if eviction autoscaler should be enabled for this PDB
	isEnabled, err := r.Filter.Filter(ctx, r.Client, pdb.Namespace)
	if err != nil {
		logger.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", pdb.Namespace)
		return reconcile.Result{}, err
	}
	if !isEnabled {
		// Namespace disabled: restore the partner PDB if we left a floor pinned.
		if pdbCarriesFloorAnnotations(&pdb) {
			if err := r.actuatePDBFloor(ctx, &pdb, nil); err != nil {
				return reconcile.Result{}, err
			}
		}
		logger.V(1).Info("Eviction autoscaler not enabled for namespace", "namespace", pdb.Namespace)
		// Only delete EvictionAutoScaler for user-owned PDbs
		// Controller-owned PDbs will be deleted by DeploymentToPDBReconciler, which cascade-deletes the EvictionAutoScaler
		isControllerOwned := pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName
		if !isControllerOwned && easFound {
			logger.Info("Deleting EvictionAutoScaler for user-owned PDB in disabled namespace", "eas", EvictionAutoScaler.Name)
			if err := r.Delete(ctx, &EvictionAutoScaler); err != nil {
				return reconcile.Result{}, client.IgnoreNotFound(err)
			}
		}
		return reconcile.Result{}, nil
	}

	// If the PDB exists, create a corresponding EvictionAutoScaler if it does not exist
	if !easFound {
		deploymentName, _, e := r.discoverDeployment(ctx, &pdb)
		if e != nil {
			return reconcile.Result{}, e
		}

		// EvictionAutoScaler not found, create it
		//variables
		controller := true
		blockOwnerDeletion := true

		// Create a new EvictionAutoScaler
		EvictionAutoScaler = types.EvictionAutoScaler{
			TypeMeta: metav1.TypeMeta{
				Kind:       "EvictionAutoScaler",
				APIVersion: "eviction-autoscaler.azure.com/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      pdb.Name,
				Namespace: pdb.Namespace,
				Annotations: map[string]string{
					"ownedBy": "EvictionAutoScaler",
					"target":  deploymentName,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "policy/v1",
						Kind:               "PodDisruptionBudget",
						Name:               pdb.Name,
						UID:                pdb.UID,
						Controller:         &controller,         // Mark as managed by this controller
						BlockOwnerDeletion: &blockOwnerDeletion, // Prevent deletion of the EvictionAutoScaler until the controller is deleted
					},
				},
			},
			Spec: types.EvictionAutoScalerSpec{
				TargetName: deploymentName,
				TargetKind: deploymentKind,
			},
		}

		err := r.Create(ctx, &EvictionAutoScaler)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("unable to create EvictionAutoScaler: %v", err)
		}
		// Track EvictionAutoScaler creation
		metrics.EvictionAutoScalerCreationCounter.WithLabelValues(pdb.Namespace, pdb.Name, deploymentName).Inc()
		logger.Info("Created EvictionAutoScaler")
	}

	// UID guard: a PDB deleted and recreated with the same name gets a new UID. Never
	// actuate that replacement with the stale EAS — retire the stale EAS (releasing its
	// finalizer) and let a fresh one be created for the replacement.
	if !easOwnsPDB(&EvictionAutoScaler, &pdb) {
		return reconcile.Result{}, r.retireStaleEAS(ctx, &EvictionAutoScaler, &pdb)
	}

	// Converge the PDB floor toward the EAS policy (pin / edit-honor / restore).
	if err := r.actuatePDBFloor(ctx, &pdb, &EvictionAutoScaler); err != nil {
		return reconcile.Result{}, err
	}
	// Align the floor finalizer with the resulting mutation state (held while the PDB
	// carries our floor, released once it is clean).
	if err := r.reconcileFloorFinalizer(ctx, &EvictionAutoScaler, &pdb); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func pdbCarriesFloorAnnotations(pdb *policyv1.PodDisruptionBudget) bool {
	if isMutated(pdb) {
		return true
	}
	_, ok := pinnedFloorFromPDB(pdb)
	return ok
}

// easOwnsPDB reports whether the live PDB is the same object the EAS was created for: a
// same-name replacement PDB gets a new UID, and must not be mutated by the stale EAS. The
// EAS is owned by its PDB, so the recorded UID is the owner ref's; a legacy EAS with no
// recorded UID falls back to name identity (all that is available).
func easOwnsPDB(eas *types.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) bool {
	for _, ref := range eas.OwnerReferences {
		if ref.Kind == "PodDisruptionBudget" {
			return ref.UID == pdb.UID
		}
	}
	return true
}

// removeFloorFinalizer removes the PDB-floor finalizer from the EAS if present,
// persisting the change. Idempotent: a no-op when already absent.
func (r *PDBToEvictionAutoScalerReconciler) removeFloorFinalizer(ctx context.Context, eas *types.EvictionAutoScaler) error {
	if controllerutil.RemoveFinalizer(eas, PDBFloorFinalizer) {
		return r.Update(ctx, eas)
	}
	return nil
}

// reconcileFloorFinalizer keeps the EAS floor finalizer present iff the PDB still carries
// our floor annotations — so a mid-drain CR delete is held until we restore, and a missing
// finalizer on an already-held mutation is repaired. Add/RemoveFinalizer skip the write
// when already in the desired state, so a steady-state reconcile is a no-op.
func (r *PDBToEvictionAutoScalerReconciler) reconcileFloorFinalizer(ctx context.Context, eas *types.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) error {
	if hasFloorAnnotation(pdb) {
		if controllerutil.AddFinalizer(eas, PDBFloorFinalizer) {
			return r.Update(ctx, eas)
		}
		return nil
	}
	return r.removeFloorFinalizer(ctx, eas)
}

// retireStaleEAS deletes an EAS whose recorded partner PDB UID no longer matches the live
// PDB (recreated with the same name), releasing the finalizer without ever mutating the
// replacement. A fresh EAS is created for the replacement on a later reconcile.
func (r *PDBToEvictionAutoScalerReconciler) retireStaleEAS(ctx context.Context, eas *types.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget) error {
	log.FromContext(ctx).Info("EAS partner UID mismatch (PDB recreated); retiring stale EAS without mutating replacement",
		"pdb", pdb.Name, "namespace", pdb.Namespace)
	if err := r.removeFloorFinalizer(ctx, eas); err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Delete(ctx, eas))
}

// reconcileEASDeletion tears down a terminating EAS that holds the floor finalizer:
// restore the partner PDB, then release the finalizer for GC. Restore is skipped when moot
// (PDB gone, deleting, or a UID-mismatched replacement) and force-released with a metric
// when the restore snapshot is missing, so a tampered PDB cannot wedge the CR in Terminating.
func (r *PDBToEvictionAutoScalerReconciler) reconcileEASDeletion(ctx context.Context, eas *types.EvictionAutoScaler, pdb *policyv1.PodDisruptionBudget, pdbFound bool) error {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(eas, PDBFloorFinalizer) {
		return nil // not ours to hold — nothing blocks GC
	}

	// Restore is moot: no live, same-identity partner PDB to restore.
	if !pdbFound || !pdb.DeletionTimestamp.IsZero() || !easOwnsPDB(eas, pdb) {
		logger.Info("EAS terminating with no restorable partner PDB, releasing floor finalizer",
			"pdb", eas.Name, "namespace", eas.Namespace, "pdbFound", pdbFound)
		return r.removeFloorFinalizer(ctx, eas)
	}

	// Snapshot-missing while still pinned (external tampering): we cannot recover the
	// original spec. Emit a metric and release the finalizer rather than wedge the CR in
	// Terminating — the stranded floor is over-protective (never below baseline). Uses raw
	// annotation presence so a malformed pinned-floor value is cleaned here too (rather
	// than tripping the teardown-incomplete guard below).
	if !isMutated(pdb) && hasFloorAnnotation(pdb) {
		logger.Info("EAS terminating with a pinned PDB but no restore snapshot (tampered); releasing finalizer, floor left in place",
			"pdb", pdb.Name, "namespace", pdb.Namespace)
		metrics.PDBFloorTeardownUnrestorableCounter.WithLabelValues(pdb.Namespace, pdb.Name).Inc()
		// Drop our stale pin annotation so we stop recognizing the object as ours.
		if _, err := restorePDBSpec(pdb); err != nil {
			return err
		}
		if err := r.updatePDBConflictAware(ctx, pdb); err != nil {
			return err
		}
		return r.removeFloorFinalizer(ctx, eas)
	}

	// Live, same-identity partner PDB: restore it. A transient error keeps the finalizer.
	if pdbCarriesFloorAnnotations(pdb) {
		if err := r.actuatePDBFloor(ctx, pdb, nil); err != nil {
			return err
		}
	}
	if hasFloorAnnotation(pdb) {
		return fmt.Errorf("floor teardown incomplete for pdb %s/%s, requeueing", pdb.Namespace, pdb.Name)
	}
	logger.Info("Restored partner PDB on EAS deletion, releasing floor finalizer", "pdb", pdb.Name, "namespace", pdb.Namespace)
	return r.removeFloorFinalizer(ctx, eas)
}

// actuatePDBFloor converges the partner PDB toward the EAS's pin policy. It honors the user's
// intent: the user's disruption policy is authoritative at all times, and we only ever express
// it *temporarily* as an absolute minAvailable floor so a replica surge converts into real
// DisruptionsAllowed (a relative policy would scale its required-healthy count up with the surge
// and eat the headroom). Whatever the user's current policy is, that is what we restore to.
//
// Why honor intent (re-baseline) rather than "own the PDB for the drain" (hold our first floor
// and override every edit): the pin is a temporary re-expression of the user's own policy, not a
// value we author. So if the user changes that policy mid-drain — via `kubectl edit`, a Helm
// upgrade, or a GitOps reconciler re-applying a manifest — we adopt the new policy: re-snapshot
// it (it becomes what we restore to) and re-derive the floor from it. We never freeze a stale
// floor over a policy the user has since changed.
//
// The single source of truth for "is this spec our pin?" is the floor WE recorded on the PDB
// (pinnedFloor annotation), matched against the live spec — not a freshly recomputed value.
// Recognizing our own pin by identity is what lets us tell "we are holding" (leave it, the
// user's policy is safely stashed in the snapshot) from "the live spec is the user's policy"
// (first pin, or they overwrote our pin) without conflating the two.
//
// Status.PDBFloorPinned is only the enable/disable signal here; the authoritative floor and the
// policy-to-restore live on the PDB annotations, re-baselined to the user's current intent.
//
// Edge cases:
//   - User switches abs<->percentage or minAvailable<->maxUnavailable mid-drain: the live spec
//     no longer matches our recorded floor, so we treat it as the user's new policy, snapshot it,
//     and re-pin a floor derived from it. Restore later returns that new policy.
//   - GitOps re-applies the original spec every sync: each reconcile we re-snapshot (same value)
//     and re-pin — a bounded tug-of-war for the duration of the (short) drain. Consistent and
//     honoring: the floor keeps tracking the declared policy, and on release GitOps stops fighting.
//   - User sets minAvailable to a value that equals our recorded floor: indistinguishable from
//     our pin, and harmless — it *is* the floor we would hold.
//   - HPA/KEDA deleted mid-surge: the recorded surge lives on the autoscaler, so it is lost with
//     it. The PDB is only un-pinned once the EAS controller clears Status.PDBFloorPinned (via the
//     deployment-strategy fallback scale-down); restore here is gated on that clear, so if the
//     fallback cannot complete the PDB may not be reverted to the user's original.
//   - MinReplicas (the surge baseline) is frozen while a surge is active, so a held floor cannot
//     go stale against it; we therefore no-op while holding rather than recomputing every pass.
func (r *PDBToEvictionAutoScalerReconciler) actuatePDBFloor(ctx context.Context, pdb *policyv1.PodDisruptionBudget, eas *types.EvictionAutoScaler) error {
	active := eas != nil && eas.Status.PDBFloorPinned
	if active {
		// Holding our own pin? Recognize it by the floor WE recorded (identity), not by a
		// recomputed value. If the live spec still carries that floor, the user's policy is
		// safely stashed in the snapshot and there is nothing to do.
		if storedFloor, ok := pinnedFloorFromPDB(pdb); ok && pdbCarriesFloor(pdb, storedFloor) {
			return nil
		}
		// Not holding our pin, so the live spec IS the user's current policy — either the first
		// time we pin, or they have overwritten our pin with a new policy. Honor it: snapshot the
		// live spec as the policy we will restore to (re-baseline to the user's latest intent),
		// then express it as an absolute floor so the surge yields headroom.
		floor, err := desiredHealthyAt(pdb.Spec, eas.Status.MinReplicas)
		if err != nil {
			return err
		}
		if floor <= 0 {
			return nil
		}
		if err := snapshotPDBSpec(pdb); err != nil {
			return err
		}
		pinPDBFloor(pdb, floor)
		return r.updatePDBConflictAware(ctx, pdb)
	}

	// Pin cleared (drain handled, feature disabled, or EAS/namespace gone): restore the user's
	// current policy.
	if f, ok := pinnedFloorFromPDB(pdb); ok && pdbCarriesFloor(pdb, f) {
		// We are still holding our pin — restore the stashed policy from the snapshot.
		changed, err := restorePDBSpec(pdb)
		if err != nil {
			return err
		}
		if changed {
			return r.updatePDBConflictAware(ctx, pdb)
		}
	} else if isMutated(pdb) {
		// The user overwrote our pin with their own spec before we released — that spec is
		// already their intent, so honor it: just drop our annotations and leave it in place.
		delete(pdb.Annotations, AnnotationOriginalPDBSpec)
		delete(pdb.Annotations, AnnotationPinnedFloor)
		return r.updatePDBConflictAware(ctx, pdb)
	}
	return nil
}

func (r *PDBToEvictionAutoScalerReconciler) updatePDBConflictAware(ctx context.Context, pdb *policyv1.PodDisruptionBudget) error {
	err := r.Update(ctx, pdb)
	if apierrors.IsConflict(err) {
		log.FromContext(ctx).V(1).Info("PDB update conflict, requeueing", "pdb", pdb.Name, "namespace", pdb.Namespace)
	}
	return err
}

// handleOwnershipTransfer manages the owner reference based on the ownedBy annotation
func (r *PDBToEvictionAutoScalerReconciler) handleOwnershipTransfer(ctx context.Context, pdb *policyv1.PodDisruptionBudget) error {
	logger := log.FromContext(ctx)

	// Check if PDB has the ownedBy annotation
	hasAnnotation := pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName

	// Check if PDB has an owner reference to a deployment
	hasOwnerRef := false
	var deploymentOwnerIdx int
	for idx, ownerRef := range pdb.OwnerReferences {
		if ownerRef.Kind == ResourceTypeDeployment {
			hasOwnerRef = true
			deploymentOwnerIdx = idx
			break
		}
	}

	// Handle annotation and owner reference synchronization
	if !hasAnnotation && hasOwnerRef {
		// User removed annotation - remove owner reference to transfer ownership
		logger.Info("Removing owner reference from PDB - user has taken ownership",
			"namespace", pdb.Namespace, "name", pdb.Name)

		// Remove the deployment owner reference
		newOwnerRefs := []metav1.OwnerReference{}
		for idx, ownerRef := range pdb.OwnerReferences {
			if idx != deploymentOwnerIdx {
				newOwnerRefs = append(newOwnerRefs, ownerRef)
			}
		}
		pdb.OwnerReferences = newOwnerRefs

		if err := r.Update(ctx, pdb); err != nil {
			logger.Error(err, "Failed to remove owner reference from PDB",
				"namespace", pdb.Namespace, "name", pdb.Name)
			return err
		}
		logger.Info("Successfully removed owner reference from PDB",
			"namespace", pdb.Namespace, "name", pdb.Name)
	} else if hasAnnotation && !hasOwnerRef {
		// Annotation is present but owner reference is missing - add it back
		logger.Info("Adding owner reference to PDB - controller taking control back",
			"namespace", pdb.Namespace, "name", pdb.Name)

		deploymentName, deploymentUID, err := r.discoverDeployment(ctx, pdb)
		if err != nil {
			logger.Error(err, "Failed to get deployment",
				"namespace", pdb.Namespace, "name", deploymentName)
			return err
		}

		controller := true
		blockOwnerDeletion := true

		pdb.OwnerReferences = append(pdb.OwnerReferences, metav1.OwnerReference{
			APIVersion:         "apps/v1",
			Kind:               ResourceTypeDeployment,
			Name:               deploymentName,
			UID:                deploymentUID,
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		})

		if err := r.Update(ctx, pdb); err != nil {
			logger.Error(err, "Failed to add owner reference to PDB",
				"namespace", pdb.Namespace, "name", pdb.Name)
			return err
		}
		logger.Info("Successfully added owner reference to PDB",
			"namespace", pdb.Namespace, "name", pdb.Name)
	}

	return nil
}

func (r *PDBToEvictionAutoScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := mgr.GetLogger()
	// Set up the controller to watch Deployments and trigger the reconcile function
	return ctrl.NewControllerManagedBy(mgr).
		For(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(requeuePDBsOnNamespaceChange(r.Client))).
		Watches(&types.EvictionAutoScaler{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: k8s_types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}}
		})).
		WithEventFilter(predicate.Funcs{
			// Trigger for Create and Update events
			UpdateFunc: func(e event.UpdateEvent) bool {
				if _, ok := e.ObjectNew.(*policyv1.PodDisruptionBudget); ok {
					return triggerOnPDBAnnotationChange(e, logger)
				}
				// For non-PDB objects (e.g. Namespace, EvictionAutoScaler), always trigger
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// With a finalizer, an EAS deletion arrives as an Update (handled by
				// UpdateFunc); the real Delete fires only after the finalizer is removed.
				// Allow it for EvictionAutoScaler so the recreate path can run while the PDB
				// still exists. PDB deletes stay filtered.
				_, ok := e.Object.(*types.EvictionAutoScaler)
				return ok
			},
		}).
		// Owns establishes ownership relationship between this controller and EvictionAutoScalers.
		// This ensures that:
		// 1. Only ONE controller (PDBToEvictionAutoScalerReconciler) manages the EvictionAutoScaler lifecycle
		Owns(&types.EvictionAutoScaler{}).
		Complete(r)
}

func (r *PDBToEvictionAutoScalerReconciler) discoverDeployment(ctx context.Context, pdb *policyv1.PodDisruptionBudget) (string, k8s_types.UID, error) {
	logger := log.FromContext(ctx)

	// Convert PDB label selector to Kubernetes selector
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return "", "", fmt.Errorf("error converting label selector: %v", err)
	}
	logger.Info("PDB Selector", "selector", pdb.Spec.Selector)

	podList := &corev1.PodList{}
	err = r.List(ctx, podList, &client.ListOptions{Namespace: pdb.Namespace, LabelSelector: selector})
	if err != nil {
		return "", "", fmt.Errorf("error listing pods: %v", err)
	}
	logger.Info("Number of pods found", "count", len(podList.Items))

	if len(podList.Items) == 0 {
		// TODO instead of an error which leads to a backoff retry quietly for a while then error?
		return "", "", fmt.Errorf("no pods found matching the PDB selector %s; leaky pdb(?!)", pdb.Name)
	}

	// Iterate through each pod
	for _, pod := range podList.Items {
		// Check the OwnerReferences of each pod
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "ReplicaSet" {
				replicaSet := &appsv1.ReplicaSet{}
				err = r.Get(ctx, k8s_types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, replicaSet)
				if apierrors.IsNotFound(err) {
					return "", "", fmt.Errorf("error fetching ReplicaSet: %v", err)
				}

				// Log ReplicaSet details
				logger.Info("Found ReplicaSet", "replicaSet", replicaSet.Name)

				// Look for the Deployment owner of the ReplicaSet
				for _, rsOwnerRef := range replicaSet.OwnerReferences {
					if rsOwnerRef.Kind == "Deployment" {
						logger.Info("Found Deployment owner", "deployment", rsOwnerRef.Name)
						return rsOwnerRef.Name, rsOwnerRef.UID, nil
					}
				}
				// no replicaset owner just move on and see if any other pods have have something.
			}
			//// Optional: Handle StatefulSets if necessary
			//if ownerRef.Kind == "StatefulSet" {
			//	statefulSet := &appsv1.StatefulSet{}
			//	err = r.Get(ctx, k8s_types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, statefulSet)
			//	if apierrors.IsNotFound(err) {
			//		return "", "", fmt.Errorf("error fetching StatefulSet: %v", err)
			//	}
			//	logger.Info("Found StatefulSet owner", "statefulSet", statefulSet.Name)
			//	// Handle StatefulSet logic if required
			//}

		}
	}
	logger.Info("No Deployment owner found")
	return "", "", errOwnerNotFound
}
