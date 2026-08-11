/*
PDB-floor actuation.

The PDBToEvictionAutoScalerReconciler is the single writer of the PDB-floor pin. The EAS
reconciler latches intent on EvictionAutoScaler.Status.PDBFloorPinned (policy); this actuator
derives the concrete floor and converges the PDB toward it (actuation):
  - desired set & PDB not yet floored -> snapshot the partner spec, pin minAvailable: floor.
  - desired cleared & PDB still pinned -> restore the partner spec (or, if a partner
    overwrote our pin mid-drain, drop only our tracking and keep their spec).

It is idempotent and level-triggered: a no-op once the PDB already matches the desired
state, so its own writes converge without a feedback loop (self-writes are recognized via
pdbCarriesFloor). A partner edit that flips minAvailable away from the floor is left passive.
*/
package controllers

import (
	"context"

	types "github.com/azure/eviction-autoscaler/api/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// actuatePDBFloor converges the PDB toward the EAS desired floor. It is the only place that
// writes the PDB-floor pin.
func (r *PDBToEvictionAutoScalerReconciler) actuatePDBFloor(ctx context.Context, pdb *policyv1.PodDisruptionBudget, eas *types.EvictionAutoScaler) error {
	logger := log.FromContext(ctx)

	if eas.Status.PDBFloorPinned {
		// A partner overwrote our pin mid-drain (isMutated but the live spec is no longer our
		// floor), or we already pinned — stay passive in both cases.
		//
		// TODO: honor-vs-re-pin is currently decided by whether the user's write kept our
		// snapshot annotation (isMutated), not by intent — a spec-only edit is honored, but a
		// full `kubectl apply` (strips annotations) gets re-pinned, so semantically identical
		// user actions behave differently. Make it consistent: either (a) an ownership marker
		// so a user takeover always honors (never re-pin over a stripped PDB), or (b) recompute
		// the floor from the user's new spec and re-pin, skipping when the result is nonsensical.
		if isMutated(pdb) {
			return nil
		}
		// Not yet pinned: pdb.Spec is still the partner's original, so derive the absolute floor
		// at the frozen baseline (Status.MinReplicas) and pin it — the value lives on the PDB,
		// status carries only intent.
		//
		// Only user-owned relative PDBs are materially pinned here. A controller-owned PDB
		// already carries an absolute minAvailable == F, so pdbCarriesFloor short-circuits below
		// (no snapshot). This is why there's no conflict with the sibling reconcilers:
		// DeploymentToPDBReconciler/AutoscalerToPDBReconciler only write controller-owned PDBs
		// (and the autoscaler one also skips during surge), while the pin's real target is
		// exactly the user-owned PDBs they never touch.
		floor, err := desiredHealthyAt(pdb.Spec, eas.Status.MinReplicas)
		if err != nil {
			return err
		}
		if floor <= 0 || pdbCarriesFloor(pdb, floor) {
			return nil
		}
		if err := snapshotPDBSpec(pdb); err != nil {
			return err
		}
		pinPDBFloor(pdb, floor)
		logger.Info("Pinning PDB floor", "pdb", pdb.Name, "floor", floor)
		return r.updatePDBConflictAware(ctx, pdb)
	}

	// Not pinned. Restore the partner's spec only when our pin is intact; if a partner
	// overwrote it, preserve their live spec and just drop our tracking annotations.
	floor, floorKnown := pinnedFloorFromPDB(pdb)
	switch {
	case floorKnown && pdbCarriesFloor(pdb, floor):
		changed, err := restorePDBSpec(pdb)
		if err != nil {
			return err
		}
		if changed {
			logger.Info("Restoring partner PDB", "pdb", pdb.Name)
			return r.updatePDBConflictAware(ctx, pdb)
		}
	case isMutated(pdb):
		delete(pdb.Annotations, AnnotationOriginalPDBSpec)
		delete(pdb.Annotations, AnnotationPinnedFloor)
		logger.Info("Partner overwrote pin; dropping tracking annotations", "pdb", pdb.Name)
		return r.updatePDBConflictAware(ctx, pdb)
	}
	return nil
}

// updatePDBConflictAware writes the PDB, treating a resourceVersion conflict as an expected
// concurrent write (e.g. the deployment reconciler re-pinning). It returns the error so the
// reconcile requeues and re-reads, converging without logging the conflict as a failure.
func (r *PDBToEvictionAutoScalerReconciler) updatePDBConflictAware(ctx context.Context, pdb *policyv1.PodDisruptionBudget) error {
	err := r.Update(ctx, pdb)
	if apierrors.IsConflict(err) {
		log.FromContext(ctx).V(1).Info("PDB write conflict, requeueing to re-converge", "pdb", pdb.Name)
	}
	return err
}
