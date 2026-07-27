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

func TestUDWrapper_SurgeDefaultSubsetIsDominant(t *testing.T) {
	w := &UnitedDeploymentWrapper{obj: makeTestUD()}
	w.SetReplicas(118) // surge total by 6

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
	w.SetReplicas(118) // surge by 6, onto spot

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
	w.SetReplicas(118)
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
	w.SetReplicas(118)
	w.SetReplicas(118) // re-apply same surge

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
	w.SetReplicas(118)                // surge by 6

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
	w.SetReplicas(118)                // ...but annotation forces regular

	got := w.Obj().(*kruiseappsv1alpha1.UnitedDeployment)
	if r := subsetRepl(got, "regular"); r == nil || r.IntValue() != 101 {
		t.Fatalf("regular replicas = %v, want 101", r)
	}
	if s := subsetRepl(got, "spot"); s == nil || s.IntValue() != 17 {
		t.Fatalf("spot replicas = %v, want 17", s)
	}
}
