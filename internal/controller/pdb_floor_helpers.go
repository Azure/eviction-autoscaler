/*
PDB-floor policy helpers.

These are the policy-side counterparts to the actuation helpers in pdbmutation.go:
pure functions over the EvictionAutoScaler that decide *whether* a PDB floor should be
pinned and publish that intent on Status.PDBFloorPinned. They never touch the PDB — the
PDBToEvictionAutoScaler reconciler actuates the marker.
*/
package controllers

import (
	myappsv1 "github.com/azure/eviction-autoscaler/api/v1"
	policyv1 "k8s.io/api/policy/v1"
)

// clearPinnedFloorIfDisabled clears a recorded pinned-floor policy when the master switch is
// off. Returns true if it cleared one (the caller then persists status).
func (r *EvictionAutoScalerReconciler) clearPinnedFloorIfDisabled(eas *myappsv1.EvictionAutoScaler) bool {
	if r.PDBFloorMutationEnabled || !eas.Status.PDBFloorPinned {
		return false
	}
	eas.Status.PDBFloorPinned = false
	return true
}

// clearPinIfHeld drops a held pinned-floor policy so the PDB actuator restores the partner PDB.
// Used on degraded paths where we can no longer manage the surge: leaving the PDB frozen at an
// absolute floor with no path back to its original spec is unsafe.
func clearPinIfHeld(eas *myappsv1.EvictionAutoScaler) {
	if eas.Status.PDBFloorPinned {
		eas.Status.PDBFloorPinned = false
	}
}

// bailPinnedFloorOnExternalChange clears a pinned-floor policy when a replica change we did not
// make is detected while we hold one. Returns true if it cleared (the caller then persists status).
func bailPinnedFloorOnExternalChange(eas *myappsv1.EvictionAutoScaler, target Surger, surgeApplier SurgeApplier) bool {
	if !eas.Status.PDBFloorPinned || !externalReplicaChange(eas, target, surgeApplier) {
		return false
	}
	eas.Status.PDBFloorPinned = false
	return true
}

// shouldPinFloorForOwnSurge reports whether we should pin a PDB floor because WE are driving the
// surge — either sitting at the frozen baseline about to surge, or already holding a surge we
// ourselves applied. Coupling to "our surge" (baseline OR our recorded surge) instead of the
// fleeting live==MinReplicas instant makes the pin idempotent across reconciles: a lost status
// write self-heals on the next pass while we still hold the surge. A replica change we didn't make
// (HPA-above-min, manual scale) matches neither and returns false (no pin). Returns false when the
// feature is off or the derived floor is not positive.
func (r *EvictionAutoScalerReconciler) shouldPinFloorForOwnSurge(eas *myappsv1.EvictionAutoScaler, target Surger, surgeApplier SurgeApplier, pdb *policyv1.PodDisruptionBudget) bool {
	if !r.PDBFloorMutationEnabled {
		return false
	}
	recordedSurge, hasRecordedSurge := surgeApplier.RecordedSurge()
	ourSurge := (target.GetReplicas() == eas.Status.MinReplicas) ||
		(hasRecordedSurge && target.GetReplicas() == recordedSurge)
	if !ourSurge {
		return false
	}
	dh, err := desiredHealthyAt(pdb.Spec, eas.Status.MinReplicas)
	if err != nil || dh <= 0 {
		return false
	}
	return true
}
