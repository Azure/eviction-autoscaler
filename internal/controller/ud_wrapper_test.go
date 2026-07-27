package controllers

import (
	"testing"

	kruiseappsv1alpha1 "github.com/openkruise/kruise-api/apps/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// makeTestUD builds a UD matching the spot experiment: total 112, regular subset
// with implicit remainder (nil replicas -> 95), spot subset "15%" (-> 17), a
// deploymentTemplate maxSurge of 10%, and live per-subset counts in status.
func makeTestUD() *kruiseappsv1alpha1.UnitedDeployment {
	total := int32(112)
	spotPct := intstr.FromString("15%")
	maxSurge := intstr.FromString("10%")
	return &kruiseappsv1alpha1.UnitedDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "spot-app", Namespace: "spot-exp"},
		Spec: kruiseappsv1alpha1.UnitedDeploymentSpec{
			Replicas: &total,
			Topology: kruiseappsv1alpha1.Topology{
				Subsets: []kruiseappsv1alpha1.Subset{
					{Name: "regular"}, // nil replicas -> remainder
					{Name: "spot", Replicas: &spotPct},
				},
			},
			Template: kruiseappsv1alpha1.SubsetTemplate{
				DeploymentTemplate: &kruiseappsv1alpha1.DeploymentTemplateSpec{
					Spec: appsv1.DeploymentSpec{
						Strategy: appsv1.DeploymentStrategy{
							RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &maxSurge},
						},
					},
				},
			},
		},
		Status: kruiseappsv1alpha1.UnitedDeploymentStatus{
			SubsetReplicas: map[string]int32{"regular": 95, "spot": 17},
		},
	}
}

func subsetRepl(ud *kruiseappsv1alpha1.UnitedDeployment, name string) *intstr.IntOrString {
	for i := range ud.Spec.Topology.Subsets {
		if ud.Spec.Topology.Subsets[i].Name == name {
			return ud.Spec.Topology.Subsets[i].Replicas
		}
	}
	return nil
}

// mustSurge runs the PrepareSurge -> SetReplicas contract, failing the test if
// the snapshot guard rejects the surge.
func mustSurge(t *testing.T, w *UnitedDeploymentWrapper, newTotal int32) {
	t.Helper()
	if err := w.PrepareSurge(); err != nil {
		t.Fatalf("PrepareSurge failed: %v", err)
	}
	w.SetReplicas(newTotal)
}

func TestUDWrapper_SurgeDefaultSubsetIsDominant(t *testing.T) {
	w := &UnitedDeploymentWrapper{obj: makeTestUD()}
	mustSurge(t, w, 118) // surge total by 6

	ud := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	if *ud.Spec.Replicas != 118 {
		t.Fatalf("spec.replicas = %d, want 118", *ud.Spec.Replicas)
	}
	// Default surge subset is the dominant one (regular, 95 baseline).
	if r := subsetRepl(ud, "regular"); r == nil || r.IntValue() != 101 {
		t.Fatalf("regular replicas = %v, want 101", r)
	}
	// Other subsets pinned to their absolute baseline.
	if s := subsetRepl(ud, "spot"); s == nil || s.IntValue() != 17 {
		t.Fatalf("spot replicas = %v, want 17", s)
	}
}

func TestUDWrapper_SurgeSubsetAnnotationRoutesToSpot(t *testing.T) {
	ud := makeTestUD()
	ud.Annotations = map[string]string{SurgeSubsetAnnotationKey: "spot"}
	w := &UnitedDeploymentWrapper{obj: ud}
	mustSurge(t, w, 118) // surge by 6, onto spot

	got := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	if *got.Spec.Replicas != 118 {
		t.Fatalf("spec.replicas = %d, want 118", *got.Spec.Replicas)
	}
	if s := subsetRepl(got, "spot"); s == nil || s.IntValue() != 23 {
		t.Fatalf("spot replicas = %v, want 23", s)
	}
	if r := subsetRepl(got, "regular"); r == nil || r.IntValue() != 95 {
		t.Fatalf("regular replicas = %v, want 95", r)
	}
}

func TestUDWrapper_RestoreOriginalTopology(t *testing.T) {
	w := &UnitedDeploymentWrapper{obj: makeTestUD()}
	mustSurge(t, w, 118)
	w.RestoreOriginal()

	ud := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	if *ud.Spec.Replicas != 112 {
		t.Fatalf("restored spec.replicas = %d, want 112", *ud.Spec.Replicas)
	}
	// regular returns to nil (controller-managed remainder).
	if r := subsetRepl(ud, "regular"); r != nil {
		t.Fatalf("regular replicas = %v, want nil (remainder)", r)
	}
	// spot returns to the original "15%".
	if s := subsetRepl(ud, "spot"); s == nil || s.String() != "15%" {
		t.Fatalf("spot replicas = %v, want \"15%%\"", s)
	}
	// Snapshot annotation cleared.
	if _, ok := ud.Annotations[udSurgeSnapshotAnnotationKey]; ok {
		t.Fatalf("snapshot annotation should be removed after restore")
	}
}

func TestUDWrapper_SurgeIsIdempotent(t *testing.T) {
	w := &UnitedDeploymentWrapper{obj: makeTestUD()}
	mustSurge(t, w, 118)
	w.SetReplicas(118) // re-apply same surge (snapshot already present)

	ud := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	if *ud.Spec.Replicas != 118 {
		t.Fatalf("spec.replicas = %d, want 118", *ud.Spec.Replicas)
	}
	if r := subsetRepl(ud, "regular"); r == nil || r.IntValue() != 101 {
		t.Fatalf("regular replicas after re-apply = %v, want 101", r)
	}
}

func TestUDWrapper_GetMaxSurgeFromDeploymentTemplate(t *testing.T) {
	w := &UnitedDeploymentWrapper{obj: makeTestUD()}
	if ms := w.GetMaxSurge(); ms.String() != "10%" {
		t.Fatalf("GetMaxSurge = %v, want \"10%%\"", ms)
	}
}

func TestUDWrapper_SurgeDefaultsToPreferredDisplacedSubset(t *testing.T) {
	w := &UnitedDeploymentWrapper{obj: makeTestUD()}
	w.SetPreferredSurgeSubset("spot") // drain is displacing spot pods
	mustSurge(t, w, 118)              // surge by 6

	ud := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	// Surge lands on the displaced (spot) subset, not the dominant (regular) one.
	if s := subsetRepl(ud, "spot"); s == nil || s.IntValue() != 23 {
		t.Fatalf("spot replicas = %v, want 23", s)
	}
	if r := subsetRepl(ud, "regular"); r == nil || r.IntValue() != 95 {
		t.Fatalf("regular replicas = %v, want 95", r)
	}
}

func TestUDWrapper_AnnotationOverridesPreferredSubset(t *testing.T) {
	ud := makeTestUD()
	ud.Annotations = map[string]string{SurgeSubsetAnnotationKey: "regular"}
	w := &UnitedDeploymentWrapper{obj: ud}
	w.SetPreferredSurgeSubset("spot") // displaced subset is spot...
	mustSurge(t, w, 118)              // ...but annotation forces regular

	got := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	if r := subsetRepl(got, "regular"); r == nil || r.IntValue() != 101 {
		t.Fatalf("regular replicas = %v, want 101", r)
	}
	if s := subsetRepl(got, "spot"); s == nil || s.IntValue() != 17 {
		t.Fatalf("spot replicas = %v, want 17", s)
	}
}

// A surge-subset annotation naming a nonexistent subset is ignored; selection
// falls back to the preferred/dominant subset.
func TestUDWrapper_InvalidAnnotationFallsBack(t *testing.T) {
	ud := makeTestUD()
	ud.Annotations = map[string]string{SurgeSubsetAnnotationKey: "does-not-exist"}
	w := &UnitedDeploymentWrapper{obj: ud}
	mustSurge(t, w, 118)

	got := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	// Falls back to dominant (regular).
	if r := subsetRepl(got, "regular"); r == nil || r.IntValue() != 101 {
		t.Fatalf("regular replicas = %v, want 101 (dominant fallback)", r)
	}
}

// GetReplicas must derive the total from status when spec.replicas is nil, not
// default to 1 (which would massively over-provision the surge subset).
func TestUDWrapper_GetReplicasNilSpecUsesStatusSum(t *testing.T) {
	ud := makeTestUD()
	ud.Spec.Replicas = nil // Kruise allows nil; total lives in status
	w := &UnitedDeploymentWrapper{obj: ud}
	if got := w.GetReplicas(); got != 112 {
		t.Fatalf("GetReplicas() with nil spec.replicas = %d, want 112 (status sum)", got)
	}
}

// PrepareSurge must refuse to snapshot when status has not converged with spec,
// so a lagging subset is never pinned below its true count.
func TestUDWrapper_PrepareSurgeRefusesWhenStatusNotConverged(t *testing.T) {
	ud := makeTestUD()
	ud.Status.SubsetReplicas = map[string]int32{"regular": 40, "spot": 17} // sum 57 != 112
	w := &UnitedDeploymentWrapper{obj: ud}
	if err := w.PrepareSurge(); err != errStatusNotConverged {
		t.Fatalf("PrepareSurge err = %v, want errStatusNotConverged", err)
	}
}

// PrepareSurge must refuse to re-capture a snapshot when a surge is already
// active but the snapshot was lost (else it would baseline off the surged state).
func TestUDWrapper_PrepareSurgeRefusesWhenSnapshotLost(t *testing.T) {
	ud := makeTestUD()
	ud.Annotations = map[string]string{EvictionSurgeReplicasAnnotationKey: "118"} // surge marker, no snapshot
	w := &UnitedDeploymentWrapper{obj: ud}
	if err := w.PrepareSurge(); err != errSnapshotLostDuringSurge {
		t.Fatalf("PrepareSurge err = %v, want errSnapshotLostDuringSurge", err)
	}
}

// SetReplicas without a prior snapshot is a no-op (guards against re-baseline).
func TestUDWrapper_SetReplicasNoopWithoutSnapshot(t *testing.T) {
	w := &UnitedDeploymentWrapper{obj: makeTestUD()}
	w.SetReplicas(118) // no PrepareSurge

	ud := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	if *ud.Spec.Replicas != 112 {
		t.Fatalf("spec.replicas = %d, want 112 (unchanged)", *ud.Spec.Replicas)
	}
}

// ForceScaleDown lowers the total to the floor and drops absolute subset pins.
func TestUDWrapper_ForceScaleDown(t *testing.T) {
	w := &UnitedDeploymentWrapper{obj: makeTestUD()}
	w.ForceScaleDown(100)

	ud := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	if *ud.Spec.Replicas != 100 {
		t.Fatalf("spec.replicas = %d, want 100", *ud.Spec.Replicas)
	}
	for _, s := range ud.Spec.Topology.Subsets {
		if s.Replicas != nil {
			t.Fatalf("subset %s replicas = %v, want nil after ForceScaleDown", s.Name, s.Replicas)
		}
	}
}
