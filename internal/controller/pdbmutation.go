/*
PDB-floor mutation.

The eviction autoscaler relieves a PDB-blocked drain by surging replicas. That
only raises the PDB's DisruptionsAllowed when the PDB uses a *percentage*
minAvailable. With an absolute PDB — maxUnavailable: 1 (the COM17 safe-deployment
config) or an absolute minAvailable — the PDB's implied floor rises with the
surged replica count, so once the surge pods are Ready DisruptionsAllowed is
still 1 and the drain crawls one pod per wave.

To make the surge effective, during a drain the controller temporarily pins the
target's PDB to an absolute minAvailable floor equal to the partner's required
healthy count *at the pre-surge moment*, then restores the partner's original
spec when the drain finishes. This preserves the partner's absolute disruption
tolerance exactly — it changes the shape of the guarantee (relative -> absolute,
so it stops tracking surged replicas), never the number of pods required healthy.

Model (see the reconcile wiring in evictionautoscaler_controller.go):

  - Floor F: captured ONCE at first mutation, derived from the partner's PDB spec
    at the baseline replica count (EvictionAutoScaler Status.MinReplicas) — the
    same value the built-in controller writes to Status.DesiredHealthy, but
    computed synchronously so it is immune to that field lagging or reading 0.
    Held constant for the whole drain and persisted on the EvictionAutoScaler CR
    status (Status.PinnedPDBFloor) so a partner stripping the PDB annotations
    cannot lose it. Never recomputed, so it is immune to surge inflation.
  - Snapshot S: the partner's PDB spec to restore, stored on the PDB itself in an
    annotation so restore works even if the CR is deleted. Re-captured from the
    live spec whenever the partner changes the PDB mid-drain, so S always tracks
    the partner's latest intent.
  - Every reconcile while the drain is active the controller re-pins F if the PDB
    no longer carries it (partner overwrite protection).
  - On revert the original spec S is restored and the annotations are cleared.
  - A finalizer restores the PDB if the CR is deleted mid-drain; a stale-window
    backstop restores any PDB left mutated longer than staleMutationWindow.

The helpers in this file are pure functions over the PDB object; the reconcile
loop performs the client Update after calling them.
*/
package controllers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// AnnotationOriginalPDBSpec stores the JSON snapshot of the partner's original
	// disruption fields (minAvailable/maxUnavailable) to restore on revert
	// (snapshot S). Its presence is the single source of truth for "this PDB is
	// mutated by us".
	AnnotationOriginalPDBSpec = "eviction-autoscaler.azure.com/original-pdb-spec"

	// AnnotationMutatedAt stores the RFC3339 timestamp of the first mutation.
	// Used by the stale-window backstop to detect a mutation that has outlived
	// its drain.
	AnnotationMutatedAt = "eviction-autoscaler.azure.com/mutated-at"

	// AnnotationPinnedFloor stores the pinned floor F on the PDB itself, so the
	// re-mutation guard can recover F even if the CR status write that mirrors it
	// (Status.PinnedPDBFloor) was lost to a conflict/restart. Co-located with the
	// snapshot so a single PDB Update persists both.
	AnnotationPinnedFloor = "eviction-autoscaler.azure.com/pinned-floor"

	// AnnotationNamespacePDBFloorOptIn is the namespace-level opt-in. Even with the
	// ENABLE_PDB_FLOOR_MUTATION master switch on, a namespace must set this
	// annotation to a truthy value on its Namespace object before the controller
	// will mutate any PDB in it.
	AnnotationNamespacePDBFloorOptIn = "eviction-autoscaler.azure.com/pdb-floor-mutation"

	// PDBFloorFinalizer is added to the EvictionAutoScaler CR while a PDB-floor
	// mutation is active so the controller can restore the partner's PDB before
	// the CR is deleted.
	PDBFloorFinalizer = "eviction-autoscaler.azure.com/pdb-floor-restore"

	// DefaultStaleMutationWindow bounds how long a PDB may stay mutated before the
	// backstop restores it unconditionally. Generous by default because a healthy
	// drain reverts on its own well within it; this only catches orphaned
	// mutations (e.g. the CR deleted without the finalizer running). Overridable at
	// install time via PDB_MUTATION_STALE_WINDOW (parsed/validated in main.go and
	// threaded onto EvictionAutoScalerReconciler.StaleMutationWindow).
	DefaultStaleMutationWindow = 2 * time.Hour
)

// The ENABLE_PDB_FLOOR_MUTATION master switch is parsed, validated and logged in
// main.go and threaded onto the reconciler as
// EvictionAutoScalerReconciler.PDBFloorMutationEnabled — see pdbFloorMutationAllowed.

// isMutated reports whether the PDB carries our original-spec snapshot annotation.
func isMutated(pdb *policyv1.PodDisruptionBudget) bool {
	if pdb == nil || pdb.Annotations == nil {
		return false
	}
	_, ok := pdb.Annotations[AnnotationOriginalPDBSpec]
	return ok
}

// isMutationStale reports whether a mutated PDB's timestamp is older than the
// given window. Returns false for unmutated PDBs. Returns true if the timestamp
// annotation is missing or malformed — restoring is the safe action for a
// mutation we can no longer reason about.
func isMutationStale(pdb *policyv1.PodDisruptionBudget, now time.Time, window time.Duration) bool {
	if !isMutated(pdb) {
		return false
	}
	atStr := pdb.Annotations[AnnotationMutatedAt]
	if atStr == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, atStr)
	if err != nil {
		return true
	}
	return now.Sub(at) > window
}

// pdbCarriesFloor reports whether the PDB's spec currently is our pinned floor:
// minAvailable == floor and no maxUnavailable. Used by the re-mutation guard to
// detect a partner overwriting the PDB mid-drain.
func pdbCarriesFloor(pdb *policyv1.PodDisruptionBudget, floor int32) bool {
	if pdb == nil || pdb.Spec.MaxUnavailable != nil || pdb.Spec.MinAvailable == nil {
		return false
	}
	ma := pdb.Spec.MinAvailable
	return ma.Type == intstr.Int && ma.IntVal == floor
}

// pdbFloorSnapshot is the restore snapshot S: only the two mutually-exclusive
// disruption fields the controller ever mutates. Storing just these — instead of
// the whole PodDisruptionBudgetSpec — keeps the annotation small and, more
// importantly, means restore never reverts a partner's concurrent change to a
// field we don't manage (Selector, UnhealthyPodEvictionPolicy): our re-snapshot
// guard (pdbCarriesFloor) only detects changes to the floor fields, so a partner
// edit to another field while our pin is intact would otherwise be silently
// rolled back on restore.
type pdbFloorSnapshot struct {
	MinAvailable   *intstr.IntOrString `json:"minAvailable,omitempty"`
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// snapshotPDBSpec captures the partner's disruption fields as the restore
// snapshot S and stamps the mutation time (only stamped on the first snapshot, so
// the stale window measures from the original mutation, not a mid-drain refresh).
//
// Callers must only invoke this when the current spec is the partner's intent
// (i.e. not already our pinned floor), otherwise S would capture our own
// mutation. The reconcile loop guarantees this via pdbCarriesFloor.
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
	if _, stamped := pdb.Annotations[AnnotationMutatedAt]; !stamped {
		pdb.Annotations[AnnotationMutatedAt] = time.Now().UTC().Format(time.RFC3339)
	}
	return nil
}

// pinPDBFloor rewrites the PDB spec to enforce minAvailable: floor and clears
// maxUnavailable (the two fields are mutually exclusive), so the absolute floor
// is the sole gating signal and does not drift with surged replicas. It also
// records the floor on the PDB (AnnotationPinnedFloor) so it survives a lost CR
// status write.
func pinPDBFloor(pdb *policyv1.PodDisruptionBudget, floor int32) {
	ma := intstr.FromInt32(floor)
	pdb.Spec.MinAvailable = &ma
	pdb.Spec.MaxUnavailable = nil
	if pdb.Annotations == nil {
		pdb.Annotations = map[string]string{}
	}
	pdb.Annotations[AnnotationPinnedFloor] = strconv.Itoa(int(floor))
}

// pinnedFloorFromPDB reads the floor recorded on the PDB by pinPDBFloor. Returns
// (0,false) when absent or unparseable.
func pinnedFloorFromPDB(pdb *policyv1.PodDisruptionBudget) (int32, bool) {
	if pdb == nil || pdb.Annotations == nil {
		return 0, false
	}
	v, ok := pdb.Annotations[AnnotationPinnedFloor]
	if !ok {
		return 0, false
	}
	// ParseInt with bitSize 32 rejects values that would overflow int32, since
	// the annotation is user-editable and a bad value must not silently truncate
	// into the wrong pinned floor.
	f, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(f), true
}

// restorePDBSpec restores the snapshotted disruption fields S onto the PDB and
// clears our annotations. Only minAvailable/maxUnavailable are written — the
// only fields we mutate — so a partner's concurrent edits to other spec fields
// are preserved. Returns false (no change) if the PDB is not mutated. Returns an
// error only if the snapshot is corrupt — the caller should surface it so the
// mutated spec is left in place for an operator rather than silently dropped.
func restorePDBSpec(pdb *policyv1.PodDisruptionBudget) (bool, error) {
	if !isMutated(pdb) {
		return false, nil
	}
	var snap pdbFloorSnapshot
	if err := json.Unmarshal([]byte(pdb.Annotations[AnnotationOriginalPDBSpec]), &snap); err != nil {
		return false, fmt.Errorf("restorePDBSpec: unmarshal snapshot: %w", err)
	}
	// Write both fields so the partner's original (min- OR max-based) disruption
	// budget is restored exactly; they are mutually exclusive, so at most one is
	// non-nil in a valid snapshot.
	pdb.Spec.MinAvailable = snap.MinAvailable
	pdb.Spec.MaxUnavailable = snap.MaxUnavailable
	delete(pdb.Annotations, AnnotationOriginalPDBSpec)
	delete(pdb.Annotations, AnnotationMutatedAt)
	delete(pdb.Annotations, AnnotationPinnedFloor)
	return true, nil
}
