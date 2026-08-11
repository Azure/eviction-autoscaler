/*
PDB-floor policy helpers.

These are the policy-side counterparts to the actuation helpers in pdbmutation.go:
pure functions over the EvictionAutoScaler that decide *whether* a PDB floor should be
pinned and publish that intent on Status.PinnedPDBFloor. They never touch the PDB — the
PDBToEvictionAutoScaler reconciler actuates the marker.
*/
package controllers

import (
	myappsv1 "github.com/azure/eviction-autoscaler/api/v1"
	policyv1 "k8s.io/api/policy/v1"
)

// clearPinnedFloorIfDisabled clears a recorded pinned-floor policy when the master switch is
// off. Returns true if it cleared one (the caller then persists status).
func clearPinnedFloorIfDisabled(eas *myappsv1.EvictionAutoScaler) bool {
	if pdbFloorMutationEnabled || eas.Status.PinnedPDBFloor == nil {
		return false
	}
	eas.Status.PinnedPDBFloor = nil
	return true
}

// bailPinnedFloorOnExternalChange clears a pinned-floor policy when a replica change we did not
// make is detected while we hold one. Returns true if it cleared (the caller then persists status).
func bailPinnedFloorOnExternalChange(eas *myappsv1.EvictionAutoScaler, target Surger, surgeApplier SurgeApplier) bool {
	if eas.Status.PinnedPDBFloor == nil || !externalReplicaChange(eas, target, surgeApplier) {
		return false
	}
	eas.Status.PinnedPDBFloor = nil
	return true
}

// pinnedFloorForOwnSurge returns the PDB floor to pin when WE are driving the surge — either
// sitting at the frozen baseline about to surge, or already holding a surge we ourselves
// applied. Coupling to "our surge" (baseline OR our recorded surge) instead of the fleeting
// live==MinReplicas instant makes the pin idempotent across reconciles: a lost status write
// self-heals on the next pass while we still hold the surge. A replica change we didn't make
// (HPA-above-min, manual scale) matches neither and returns nil (no pin). Returns nil when the
// feature is off or the derived floor is not positive.
func pinnedFloorForOwnSurge(eas *myappsv1.EvictionAutoScaler, target Surger, surgeApplier SurgeApplier, pdb *policyv1.PodDisruptionBudget) *int32 {
	if !pdbFloorMutationEnabled {
		return nil
	}
	recordedSurge, hasRecordedSurge := surgeApplier.RecordedSurge()
	ourSurge := (target.GetReplicas() == eas.Status.MinReplicas) ||
		(hasRecordedSurge && target.GetReplicas() == recordedSurge)
	if !ourSurge {
		return nil
	}
	dh, err := desiredHealthyAt(pdb.Spec, eas.Status.MinReplicas)
	if err != nil || dh <= 0 {
		return nil
	}
	return &dh
}
