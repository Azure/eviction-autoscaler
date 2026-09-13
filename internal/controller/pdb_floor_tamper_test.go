package controllers

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// pinnedPDB builds a PDB pinned by us from an original policy of maxUnavailable 20%: snapshot
// {maxUnavailable:"20%"}, live spec minAvailable 8 (the floor at 10 replicas), marker "8".
func pinnedPDB(g *WithT, nm, nsp string) *policyv1.PodDisruptionBudget {
	mu := intstr.FromString("20%")
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &mu,
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
		},
	}
	g.Expect(snapshotPDBSpec(pdb)).To(Succeed())
	pinPDBFloor(pdb, 8)
	return pdb
}

func tamperReconciler(g *WithT, objs ...client.Object) *PDBToEvictionAutoScalerReconciler {
	scheme := runtime.NewScheme()
	g.Expect(v1.AddToScheme(scheme)).To(Succeed())
	g.Expect(policyv1.AddToScheme(scheme)).To(Succeed())
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PDBToEvictionAutoScalerReconciler{Client: fc, Scheme: scheme}
}

func activeEAS(nm, nsp string) *v1.EvictionAutoScaler {
	return &v1.EvictionAutoScaler{
		ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp},
		Status:     v1.EvictionAutoScalerStatus{PDBFloorPinned: true, MinReplicas: 10},
	}
}

// TestActuatePDBFloorTamperMissingMarkerPreservesSnapshot: the pinned-floor marker is removed but the
// live spec still carries our floor. The actuator must recognize this as our own pin (via the saved
// snapshot), NOT re-snapshot the pinned spec over the real original, and repair the marker.
func TestActuatePDBFloorTamperMissingMarkerPreservesSnapshot(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const nm, nsp = "tamper-miss-ea", "tamper-miss-ns"
	pdb := pinnedPDB(g, nm, nsp)
	orig := pdb.Annotations[AnnotationOriginalPDBSpec]
	delete(pdb.Annotations, AnnotationPinnedFloor) // tamper: drop the marker only

	ea := activeEAS(nm, nsp)
	r := tamperReconciler(g, ea, pdb)
	g.Expect(r.actuatePDBFloor(ctx, pdb, ea)).To(Succeed())

	g.Expect(pdb.Annotations[AnnotationOriginalPDBSpec]).To(Equal(orig), "original snapshot must be preserved, not overwritten with our floor")
	g.Expect(pdb.Annotations[AnnotationPinnedFloor]).To(Equal("8"), "pinned-floor marker must be repaired")
	g.Expect(pdbCarriesFloor(pdb, 8)).To(BeTrue())
}

// TestActuatePDBFloorTamperCorruptedMarkerPreservesSnapshot: the marker is corrupted to a different
// VALID value (9) while the live spec still carries our floor (8). Must still preserve the snapshot
// and repair the marker to 8 (the haveStored-only guard would have failed this case).
func TestActuatePDBFloorTamperCorruptedMarkerPreservesSnapshot(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const nm, nsp = "tamper-bad-ea", "tamper-bad-ns"
	pdb := pinnedPDB(g, nm, nsp)
	orig := pdb.Annotations[AnnotationOriginalPDBSpec]
	pdb.Annotations[AnnotationPinnedFloor] = "9" // tamper: corrupt to a valid-but-wrong value

	ea := activeEAS(nm, nsp)
	r := tamperReconciler(g, ea, pdb)
	g.Expect(r.actuatePDBFloor(ctx, pdb, ea)).To(Succeed())

	g.Expect(pdb.Annotations[AnnotationOriginalPDBSpec]).To(Equal(orig), "original snapshot must be preserved")
	g.Expect(pdb.Annotations[AnnotationPinnedFloor]).To(Equal("8"), "pinned-floor marker must be repaired to the real floor")
	g.Expect(pdbCarriesFloor(pdb, 8)).To(BeTrue())
}

// TestActuatePDBFloorGenuineRebaseline: a real mid-drain user spec edit (20% → 10%, our annotations
// retained) must re-snapshot the new policy and re-pin its floor — the fix must NOT break this.
func TestActuatePDBFloorGenuineRebaseline(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const nm, nsp = "rebase-ea", "rebase-ns"
	pdb := pinnedPDB(g, nm, nsp) // snapshot 20%, marker 8, spec minAvailable 8
	// User overwrites the live spec with a new policy; our annotations survive the edit.
	newMu := intstr.FromString("10%")
	pdb.Spec.MinAvailable = nil
	pdb.Spec.MaxUnavailable = &newMu

	ea := activeEAS(nm, nsp)
	r := tamperReconciler(g, ea, pdb)
	g.Expect(r.actuatePDBFloor(ctx, pdb, ea)).To(Succeed())

	var snap pdbFloorSnapshot
	g.Expect(json.Unmarshal([]byte(pdb.Annotations[AnnotationOriginalPDBSpec]), &snap)).To(Succeed())
	g.Expect(snap.MaxUnavailable).NotTo(BeNil())
	g.Expect(snap.MaxUnavailable.String()).To(Equal("10%"), "snapshot must be re-baselined to the user's new policy")
	g.Expect(pdb.Annotations[AnnotationPinnedFloor]).To(Equal("9"))
	g.Expect(pdbCarriesFloor(pdb, 9)).To(BeTrue())
}

// TestTriggerOnPinnedFloorAnnotationChange: the PDB watch predicate must fire on a pinned-floor
// marker change (so tampering re-triggers a reconcile that re-derives pdb_mutated / repairs the
// marker), and must NOT fire when nothing relevant changed.
func TestTriggerOnPinnedFloorAnnotationChange(t *testing.T) {
	g := NewWithT(t)
	lg := logr.Discard()
	mk := func(ann map[string]string) *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "n", Annotations: ann}}
	}
	g.Expect(triggerOnPDBAnnotationChange(event.UpdateEvent{
		ObjectOld: mk(map[string]string{AnnotationPinnedFloor: "8"}),
		ObjectNew: mk(map[string]string{AnnotationPinnedFloor: "9"}),
	}, lg)).To(BeTrue(), "marker value change must trigger")
	g.Expect(triggerOnPDBAnnotationChange(event.UpdateEvent{
		ObjectOld: mk(map[string]string{AnnotationPinnedFloor: "8"}),
		ObjectNew: mk(nil),
	}, lg)).To(BeTrue(), "marker removal must trigger")
	g.Expect(triggerOnPDBAnnotationChange(event.UpdateEvent{
		ObjectOld: mk(map[string]string{AnnotationPinnedFloor: "8"}),
		ObjectNew: mk(map[string]string{AnnotationPinnedFloor: "8"}),
	}, lg)).To(BeFalse(), "no relevant change must not trigger")
}

// TestReconcileEASDeletionTamperedClearsGauges: the tampered/unrestorable teardown branch (pinned-floor
// marker present, no restore snapshot) drops the marker and abandons the floor — it must reconcile the
// PDB gauges explicitly so pdb_mutated / pdb_floor_pinned don't leak a stale ==1 series.
func TestReconcileEASDeletionTamperedClearsGauges(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const nm, nsp = "b2-ea", "b2-ns"
	scheme := runtime.NewScheme()
	g.Expect(v1.AddToScheme(scheme)).To(Succeed())
	g.Expect(policyv1.AddToScheme(scheme)).To(Succeed())

	eight := intstr.FromInt32(8)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp, Annotations: map[string]string{AnnotationPinnedFloor: "8"}},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &eight, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}},
	}
	now := metav1.Now()
	eas := &v1.EvictionAutoScaler{
		ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp, Finalizers: []string{PDBFloorFinalizer}, DeletionTimestamp: &now},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pdb, eas).Build()
	r := &PDBToEvictionAutoScalerReconciler{Client: fc, Scheme: scheme}

	metrics.PDBMutated.Reset()
	metrics.PDBFloorPinned.Reset()
	metrics.PDBMutated.WithLabelValues(nsp, nm).Set(1)
	metrics.PDBFloorPinned.WithLabelValues(nsp, nm, "app").Set(1)

	g.Expect(r.reconcileEASDeletion(ctx, eas, pdb, true)).To(Succeed())

	g.Expect(testutil.ToFloat64(metrics.PDBMutated.WithLabelValues(nsp, nm))).
		To(Equal(0.0), "pdb_mutated must be set to 0 on tampered teardown")
	g.Expect(testutil.CollectAndCount(metrics.PDBFloorPinned)).
		To(Equal(0), "pdb_floor_pinned series must be deleted on tampered teardown")
}
