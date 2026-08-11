package controllers

import (
	"context"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These specs exercise the actuator in isolation (fake client). They encode the same
// intents PR 170 verified in-EAS, now at the actuation layer of the split design:
//   - pin a relative PDB when a floor is desired (PR170: "pins on surge")
//   - restore the partner's original when the floor is cleared (PR170: "restores on scale-down")
//   - stay passive on a partner overwrite and preserve their edit (PR170: "does not defend")
var _ = Describe("actuatePDBFloor", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(policyv1.AddToScheme(scheme)).To(Succeed())
		Expect(v1.AddToScheme(scheme)).To(Succeed())
	})

	relativePDB := func() *policyv1.PodDisruptionBudget {
		mu := intstr.FromInt32(1)
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu},
		}
	}

	// seed puts the PDB in a fake client and returns the reconciler plus the live copy
	// (with a resourceVersion so Update works).
	seed := func(pdb *policyv1.PodDisruptionBudget) (*PDBToEvictionAutoScalerReconciler, *policyv1.PodDisruptionBudget) {
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pdb).Build()
		r := &PDBToEvictionAutoScalerReconciler{Client: fc, Scheme: scheme}
		live := &policyv1.PodDisruptionBudget{}
		Expect(r.Get(ctx, types.NamespacedName{Name: pdb.Name, Namespace: pdb.Namespace}, live)).To(Succeed())
		return r, live
	}

	get := func(r *PDBToEvictionAutoScalerReconciler) *policyv1.PodDisruptionBudget {
		out := &policyv1.PodDisruptionBudget{}
		Expect(r.Get(ctx, types.NamespacedName{Name: "app", Namespace: "default"}, out)).To(Succeed())
		return out
	}

	easFloor := func(f *int32) *v1.EvictionAutoScaler {
		return &v1.EvictionAutoScaler{Status: v1.EvictionAutoScalerStatus{PinnedPDBFloor: f}}
	}

	It("pins a relative PDB when a floor is desired", func() {
		r, live := seed(relativePDB())
		Expect(r.actuatePDBFloor(ctx, live, easFloor(ptr.To(int32(4))))).To(Succeed())

		got := get(r)
		Expect(got.Spec.MaxUnavailable).To(BeNil())
		Expect(got.Spec.MinAvailable).NotTo(BeNil())
		Expect(got.Spec.MinAvailable.IntVal).To(Equal(int32(4)))
		Expect(got.Annotations).To(HaveKey(AnnotationOriginalPDBSpec))
		Expect(got.Annotations).To(HaveKey(AnnotationPinnedFloor))
	})

	It("is idempotent when the PDB already carries the floor (no spec churn)", func() {
		pdb := relativePDB()
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		pinPDBFloor(pdb, 4)
		r, live := seed(pdb)
		rv := get(r).ResourceVersion

		Expect(r.actuatePDBFloor(ctx, live, easFloor(ptr.To(int32(4))))).To(Succeed())
		Expect(get(r).ResourceVersion).To(Equal(rv)) // no write
	})

	It("stays passive when a partner overwrote our pin mid-drain", func() {
		pdb := relativePDB()
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		pinPDBFloor(pdb, 4)
		// Partner overwrites the live spec back to a relative PDB, keeping our annotations.
		mu := intstr.FromString("25%")
		pdb.Spec.MinAvailable = nil
		pdb.Spec.MaxUnavailable = &mu
		r, live := seed(pdb)
		rv := get(r).ResourceVersion

		Expect(r.actuatePDBFloor(ctx, live, easFloor(ptr.To(int32(4))))).To(Succeed())
		got := get(r)
		Expect(got.ResourceVersion).To(Equal(rv)) // untouched
		Expect(got.Spec.MaxUnavailable.StrVal).To(Equal("25%"))
	})

	It("restores the partner's original when the floor is cleared", func() {
		pdb := relativePDB()
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		pinPDBFloor(pdb, 4)
		r, live := seed(pdb)

		Expect(r.actuatePDBFloor(ctx, live, easFloor(nil))).To(Succeed())
		got := get(r)
		Expect(got.Spec.MinAvailable).To(BeNil())
		Expect(got.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(got.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(got.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(got.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
	})

	It("keeps the partner's edit and only drops tracking when they overwrote the pin", func() {
		pdb := relativePDB()
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		pinPDBFloor(pdb, 4)
		mu := intstr.FromString("25%")
		pdb.Spec.MinAvailable = nil
		pdb.Spec.MaxUnavailable = &mu
		r, live := seed(pdb)

		Expect(r.actuatePDBFloor(ctx, live, easFloor(nil))).To(Succeed())
		got := get(r)
		Expect(got.Spec.MaxUnavailable.StrVal).To(Equal("25%")) // partner edit preserved
		Expect(got.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(got.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
	})

	It("is a no-op when no floor is desired and nothing is pinned", func() {
		r, live := seed(relativePDB())
		rv := get(r).ResourceVersion
		Expect(r.actuatePDBFloor(ctx, live, easFloor(nil))).To(Succeed())
		Expect(get(r).ResourceVersion).To(Equal(rv))
	})
})

var _ = Describe("originalSpecFromSnapshot", func() {
	It("reconstructs the snapshotted disruption spec", func() {
		mu := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu},
		}
		Expect(snapshotPDBSpec(pdb)).To(Succeed())

		spec, ok, err := originalSpecFromSnapshot(pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(spec.MinAvailable).To(BeNil())
		Expect(spec.MaxUnavailable).NotTo(BeNil())
		Expect(spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
	})

	It("returns ok=false when the PDB carries no snapshot", func() {
		pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
		_, ok, err := originalSpecFromSnapshot(pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})
})
