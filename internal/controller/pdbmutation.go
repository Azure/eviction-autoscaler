/*
PDB-floor mutation.

During a PDB-blocked drain the controller pins the target's PDB to an absolute
minAvailable floor (the partner's required-healthy count at the pre-surge baseline,
Status.MinReplicas) so a replica surge converts into DisruptionsAllowed instead of
being tracked away by a relative floor, then restores the partner's spec when the
drain finishes. The floor is captured once and persisted (on the PDB annotation and
Status.PinnedPDBFloor); a partner overwriting the PDB mid-drain is honored, not
clobbered. Helpers here are pure functions over the PDB; the reconcile loop performs
the client Update.
*/
package controllers

import (
	"encoding/json"
	"fmt"
	"strconv"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// AnnotationOriginalPDBSpec holds the JSON snapshot of the partner's original
	// disruption fields; its presence marks a PDB as mutated by us.
	AnnotationOriginalPDBSpec = "eviction-autoscaler.azure.com/original-pdb-spec"

	// AnnotationPinnedFloor records the pinned floor on the PDB so it survives a lost
	// CR status write (Status.PinnedPDBFloor).
	AnnotationPinnedFloor = "eviction-autoscaler.azure.com/pinned-floor"

	// AnnotationNamespacePDBFloorOptIn is the per-namespace opt-in; required (in
	// addition to the master switch) before the controller mutates any PDB.
	AnnotationNamespacePDBFloorOptIn = "eviction-autoscaler.azure.com/pdb-floor-mutation"
)

// isMutated reports whether the PDB carries our original-spec snapshot annotation.
func isMutated(pdb *policyv1.PodDisruptionBudget) bool {
	if pdb == nil || pdb.Annotations == nil {
		return false
	}
	_, ok := pdb.Annotations[AnnotationOriginalPDBSpec]
	return ok
}

// pdbCarriesFloor reports whether the PDB's live spec is still our pinned floor
// (minAvailable == floor, no maxUnavailable) — false once a partner overwrites it.
func pdbCarriesFloor(pdb *policyv1.PodDisruptionBudget, floor int32) bool {
	if pdb == nil || pdb.Spec.MaxUnavailable != nil || pdb.Spec.MinAvailable == nil {
		return false
	}
	ma := pdb.Spec.MinAvailable
	return ma.Type == intstr.Int && ma.IntVal == floor
}

// pdbFloorSnapshot is the restore snapshot: only the two mutually-exclusive fields
// we ever mutate, so restore never rolls back a partner's edit to a field we don't
// manage (Selector, UnhealthyPodEvictionPolicy).
type pdbFloorSnapshot struct {
	MinAvailable   *intstr.IntOrString `json:"minAvailable,omitempty"`
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// snapshotPDBSpec captures the partner's disruption fields as the restore snapshot.
// Callers must only invoke this when the current spec is the partner's intent (not
// already our pinned floor); the reconcile loop guarantees this via pdbCarriesFloor.
func snapshotPDBSpec(pdb *policyv1.PodDisruptionBudget) error {
	snap := pdbFloorSnapshot{
		MinAvailable:   pdb.Spec.MinAvailable,
		MaxUnavailable: pdb.Spec.MaxUnavailable,
	}
	specBytes, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("snapshotPDBSpec: marshal snapshot: %w", err)
	}
	if pdb.Annotations == nil {
		pdb.Annotations = map[string]string{}
	}
	pdb.Annotations[AnnotationOriginalPDBSpec] = string(specBytes)
	return nil
}

// pinPDBFloor rewrites the PDB to minAvailable: floor (clearing maxUnavailable) and
// records the floor on the PDB so it survives a lost CR status write.
func pinPDBFloor(pdb *policyv1.PodDisruptionBudget, floor int32) {
	ma := intstr.FromInt32(floor)
	pdb.Spec.MinAvailable = &ma
	pdb.Spec.MaxUnavailable = nil
	if pdb.Annotations == nil {
		pdb.Annotations = map[string]string{}
	}
	pdb.Annotations[AnnotationPinnedFloor] = strconv.Itoa(int(floor))
}

// pinnedFloorFromPDB reads the floor recorded by pinPDBFloor; (0,false) when absent
// or unparseable.
func pinnedFloorFromPDB(pdb *policyv1.PodDisruptionBudget) (int32, bool) {
	if pdb == nil || pdb.Annotations == nil {
		return 0, false
	}
	v, ok := pdb.Annotations[AnnotationPinnedFloor]
	if !ok {
		return 0, false
	}
	// bitSize 32 rejects overflow; reject non-positive (a floor is always a positive
	// minAvailable) — the annotation is user-editable.
	f, err := strconv.ParseInt(v, 10, 32)
	if err != nil || f <= 0 {
		return 0, false
	}
	return int32(f), true
}

// restorePDBSpec restores the snapshotted disruption fields and clears our
// annotations; only minAvailable/maxUnavailable are written, so a partner's edits to
// other fields are preserved. With no snapshot it still drops a stale pinned-floor
// annotation (returning changed=true) so a pin can't be left half-recorded. Returns
// false when nothing changed, or an error when the snapshot is corrupt (caller
// surfaces it rather than dropping the spec).
func restorePDBSpec(pdb *policyv1.PodDisruptionBudget) (bool, error) {
	if !isMutated(pdb) {
		if pdb != nil && pdb.Annotations != nil {
			if _, ok := pdb.Annotations[AnnotationPinnedFloor]; ok {
				delete(pdb.Annotations, AnnotationPinnedFloor)
				return true, nil
			}
		}
		return false, nil
	}
	var snap pdbFloorSnapshot
	if err := json.Unmarshal([]byte(pdb.Annotations[AnnotationOriginalPDBSpec]), &snap); err != nil {
		return false, fmt.Errorf("restorePDBSpec: unmarshal snapshot: %w", err)
	}
	pdb.Spec.MinAvailable = snap.MinAvailable
	pdb.Spec.MaxUnavailable = snap.MaxUnavailable
	delete(pdb.Annotations, AnnotationOriginalPDBSpec)
	delete(pdb.Annotations, AnnotationPinnedFloor)
	return true, nil
}
