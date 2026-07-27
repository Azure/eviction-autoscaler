package controllers

import (
	"encoding/json"

	kruiseappsv1alpha1 "github.com/openkruise/kruise-api/apps/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const unitedDeploymentKind = "uniteddeployment"

// SurgeSubsetAnnotationKey lets a workload owner choose which UnitedDeployment
// subset absorbs the drain-time surge, i.e. WHERE the surge lands. Value is a
// subset name (e.g. "regular" for on-demand, "spot" for spot capacity). When
// absent, the wrapper defaults to the subset with the most current replicas.
const SurgeSubsetAnnotationKey = "eviction-autoscaler.azure.com/surge-subset"

// udSurgeSnapshotAnnotationKey stores the pre-surge UnitedDeployment topology so
// the exact original per-subset config (nil/remainder or "15%") and total can be
// restored on revert. Single source of truth for restore; also makes ApplySurge
// idempotent (surge amounts are always computed from the snapshot baseline).
const udSurgeSnapshotAnnotationKey = "eviction-autoscaler.azure.com/ud-surge-snapshot"

// udSurgeSnapshot is the JSON captured on the UD at first surge.
type udSurgeSnapshot struct {
	Total       int32            `json:"total"`          // original spec.replicas
	Original    map[string]string `json:"original"`      // subset name -> original .Replicas ("" == nil/remainder)
	Baseline    map[string]int32 `json:"baseline"`       // subset name -> live replicas at snapshot (from status)
	SurgeSubset string           `json:"surgeSubset"`    // subset chosen to absorb the surge
}

// UnitedDeploymentWrapper adapts an OpenKruise UnitedDeployment to the Surger
// interface. Surging a UD by scaling its child Deployment directly does not work
// — the UD controller reverts it — so we surge through the UD's own spec: raise
// spec.replicas and route the delta to a chosen subset (pinning the others to
// absolute counts so percentage subsets don't grow with the new total). The UD
// controller then propagates the surge and does not fight it.
type UnitedDeploymentWrapper struct {
	obj *kruiseappsv1alpha1.UnitedDeployment
}

var _ Surger = &UnitedDeploymentWrapper{}

func (u *UnitedDeploymentWrapper) Obj() client.Object {
	return u.obj
}

// GetReplicas returns the UD's total desired replicas (spec.replicas).
func (u *UnitedDeploymentWrapper) GetReplicas() int32 {
	if u.obj.Spec.Replicas == nil {
		return 1
	}
	return *u.obj.Spec.Replicas
}

// GetMaxSurge reads the maxSurge from the UD's deploymentTemplate rolling-update
// strategy (the same knob a plain Deployment uses). Defaults to 0 when unset.
func (u *UnitedDeploymentWrapper) GetMaxSurge() intstr.IntOrString {
	dt := u.obj.Spec.Template.DeploymentTemplate
	if dt != nil && dt.Spec.Strategy.RollingUpdate != nil && dt.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		return *dt.Spec.Strategy.RollingUpdate.MaxSurge
	}
	return intstr.FromInt(0)
}

// SetReplicas surges the UD to a new total by routing the delta onto the chosen
// surge subset. It snapshots the original topology on first call (so revert can
// restore it) and is idempotent: the surge amount is always derived from the
// snapshot baseline, so re-applying the same total yields the same spec.
func (u *UnitedDeploymentWrapper) SetReplicas(newTotal int32) {
	u.obj = u.obj.DeepCopy() // don't mutate the cache

	snap, ok := u.readSnapshot()
	if !ok {
		snap = u.captureSnapshot()
		u.writeSnapshot(snap)
	}

	delta := newTotal - snap.Total
	total := newTotal
	u.obj.Spec.Replicas = &total

	// Pin every subset to an absolute count; the surge subset additionally
	// carries the delta. Absolute values make the totals reconcile exactly and
	// stop percentage/remainder subsets from drifting with the new total.
	for i := range u.obj.Spec.Topology.Subsets {
		s := &u.obj.Spec.Topology.Subsets[i]
		base, hasBase := snap.Baseline[s.Name]
		if !hasBase {
			continue
		}
		want := base
		if s.Name == snap.SurgeSubset {
			want = base + delta
		}
		v := intstr.FromInt(int(want))
		s.Replicas = &v
	}
}

// RestoreOriginal reverts the UD to its pre-surge topology using the snapshot,
// then clears the snapshot annotation. No-op when nothing was snapshotted.
func (u *UnitedDeploymentWrapper) RestoreOriginal() {
	snap, ok := u.readSnapshot()
	if !ok {
		return
	}
	u.obj = u.obj.DeepCopy()

	total := snap.Total
	u.obj.Spec.Replicas = &total
	for i := range u.obj.Spec.Topology.Subsets {
		s := &u.obj.Spec.Topology.Subsets[i]
		orig, hasOrig := snap.Original[s.Name]
		if !hasOrig {
			continue
		}
		if orig == "" {
			s.Replicas = nil // controller-managed remainder
			continue
		}
		v := intstr.Parse(orig)
		s.Replicas = &v
	}
	u.RemoveAnnotation(udSurgeSnapshotAnnotationKey)
}

// captureSnapshot records the current topology + live per-subset counts.
func (u *UnitedDeploymentWrapper) captureSnapshot() udSurgeSnapshot {
	snap := udSurgeSnapshot{
		Total:    u.GetReplicas(),
		Original: map[string]string{},
		Baseline: map[string]int32{},
	}
	for _, s := range u.obj.Spec.Topology.Subsets {
		if s.Replicas == nil {
			snap.Original[s.Name] = ""
		} else {
			snap.Original[s.Name] = s.Replicas.String()
		}
	}
	// Live per-subset counts come from status.subsetReplicas.
	for name, n := range u.obj.Status.SubsetReplicas {
		snap.Baseline[name] = n
	}
	// Fallback: any subset missing from status baselines gets 0 so it is still pinned.
	for _, s := range u.obj.Spec.Topology.Subsets {
		if _, ok := snap.Baseline[s.Name]; !ok {
			snap.Baseline[s.Name] = 0
		}
	}
	snap.SurgeSubset = u.resolveSurgeSubset(snap.Baseline)
	return snap
}

// resolveSurgeSubset picks the subset that absorbs the surge: an explicit
// surge-subset annotation if valid, otherwise the subset with the most current
// replicas (the dominant, typically on-demand, subset).
func (u *UnitedDeploymentWrapper) resolveSurgeSubset(baseline map[string]int32) string {
	if ann := u.obj.GetAnnotations(); ann != nil {
		if want := ann[SurgeSubsetAnnotationKey]; want != "" {
			for _, s := range u.obj.Spec.Topology.Subsets {
				if s.Name == want {
					return want
				}
			}
		}
	}
	best, bestN := "", int32(-1)
	for _, s := range u.obj.Spec.Topology.Subsets {
		if n := baseline[s.Name]; n > bestN {
			best, bestN = s.Name, n
		}
	}
	return best
}

func (u *UnitedDeploymentWrapper) readSnapshot() (udSurgeSnapshot, bool) {
	ann := u.obj.GetAnnotations()
	if ann == nil {
		return udSurgeSnapshot{}, false
	}
	raw, ok := ann[udSurgeSnapshotAnnotationKey]
	if !ok || raw == "" {
		return udSurgeSnapshot{}, false
	}
	var snap udSurgeSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return udSurgeSnapshot{}, false
	}
	return snap, true
}

func (u *UnitedDeploymentWrapper) writeSnapshot(snap udSurgeSnapshot) {
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	u.AddAnnotation(udSurgeSnapshotAnnotationKey, string(b))
}

func (u *UnitedDeploymentWrapper) AddAnnotation(key, value string) {
	if u.obj.Annotations == nil {
		u.obj.Annotations = make(map[string]string)
	}
	u.obj.Annotations[key] = value
}

func (u *UnitedDeploymentWrapper) RemoveAnnotation(key string) {
	if u.obj.Annotations != nil {
		delete(u.obj.Annotations, key)
	}
}
