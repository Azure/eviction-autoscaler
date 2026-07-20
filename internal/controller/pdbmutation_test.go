package controllers

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func pdbWithMaxUnavailable(mu intstr.IntOrString) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu},
	}
}

var _ = Describe("isMutated", func() {
	It("is false for a nil PDB", func() {
		Expect(isMutated(nil)).To(BeFalse())
	})
	It("is false when there are no annotations", func() {
		Expect(isMutated(pdbWithMaxUnavailable(intstr.FromInt32(1)))).To(BeFalse())
	})
	It("is true when the original-spec annotation is present", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationOriginalPDBSpec: "{}"}
		Expect(isMutated(pdb)).To(BeTrue())
	})
})

var _ = Describe("isMutationStale", func() {
	var origWindow time.Duration
	BeforeEach(func() { origWindow = staleMutationWindow; staleMutationWindow = time.Hour })
	AfterEach(func() { staleMutationWindow = origWindow })

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	It("is false for an unmutated PDB", func() {
		Expect(isMutationStale(pdbWithMaxUnavailable(intstr.FromInt32(1)), now)).To(BeFalse())
	})
	It("is true when mutated with no timestamp", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationOriginalPDBSpec: "{}"}
		Expect(isMutationStale(pdb, now)).To(BeTrue())
	})
	It("is true when the timestamp is malformed", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationOriginalPDBSpec: "{}", AnnotationMutatedAt: "not-a-time"}
		Expect(isMutationStale(pdb, now)).To(BeTrue())
	})
	It("is false within the window", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationOriginalPDBSpec: "{}", AnnotationMutatedAt: now.Add(-30 * time.Minute).Format(time.RFC3339)}
		Expect(isMutationStale(pdb, now)).To(BeFalse())
	})
	It("is true beyond the window", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationOriginalPDBSpec: "{}", AnnotationMutatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)}
		Expect(isMutationStale(pdb, now)).To(BeTrue())
	})
})

var _ = Describe("pdbCarriesFloor", func() {
	It("is true for minAvailable==floor with no maxUnavailable", func() {
		ma := intstr.FromInt32(101)
		pdb := &policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &ma}}
		Expect(pdbCarriesFloor(pdb, 101)).To(BeTrue())
	})
	It("is false when maxUnavailable is set", func() {
		ma := intstr.FromInt32(101)
		mu := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &ma, MaxUnavailable: &mu}}
		Expect(pdbCarriesFloor(pdb, 101)).To(BeFalse())
	})
	It("is false for a different floor", func() {
		ma := intstr.FromInt32(100)
		pdb := &policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &ma}}
		Expect(pdbCarriesFloor(pdb, 101)).To(BeFalse())
	})
	It("is false for a percentage minAvailable", func() {
		ma := intstr.FromString("90%")
		pdb := &policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &ma}}
		Expect(pdbCarriesFloor(pdb, 90)).To(BeFalse())
	})
	It("is false when minAvailable is nil", func() {
		Expect(pdbCarriesFloor(pdbWithMaxUnavailable(intstr.FromInt32(1)), 101)).To(BeFalse())
	})
})

var _ = Describe("pinPDBFloor", func() {
	It("sets minAvailable and clears maxUnavailable", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pinPDBFloor(pdb, 101)
		Expect(pdb.Spec.MaxUnavailable).To(BeNil())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.Type).To(Equal(intstr.Int))
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(101)))
	})
})

var _ = Describe("pinnedFloorFromPDB", func() {
	It("round-trips a floor written by pinPDBFloor", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pinPDBFloor(pdb, 42)
		f, ok := pinnedFloorFromPDB(pdb)
		Expect(ok).To(BeTrue())
		Expect(f).To(Equal(int32(42)))
	})
	It("returns false when the annotation is absent", func() {
		_, ok := pinnedFloorFromPDB(pdbWithMaxUnavailable(intstr.FromInt32(1)))
		Expect(ok).To(BeFalse())
	})
	It("rejects a value that overflows int32 rather than truncating", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationPinnedFloor: "3000000000"} // > int32 max
		_, ok := pinnedFloorFromPDB(pdb)
		Expect(ok).To(BeFalse())
	})
	It("rejects a non-numeric value", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationPinnedFloor: "abc"}
		_, ok := pinnedFloorFromPDB(pdb)
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("snapshot + restore round-trip", func() {
	It("restores the original spec and clears annotations", func() {
		orig := pdbWithMaxUnavailable(intstr.FromInt32(1))

		Expect(snapshotPDBSpec(orig)).To(Succeed())
		Expect(isMutated(orig)).To(BeTrue())
		Expect(orig.Annotations).To(HaveKey(AnnotationMutatedAt))
		pinPDBFloor(orig, 101)
		Expect(pdbCarriesFloor(orig, 101)).To(BeTrue())

		changed, err := restorePDBSpec(orig)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		Expect(orig.Spec.MinAvailable).To(BeNil())
		Expect(orig.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(orig.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(orig.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(orig.Annotations).NotTo(HaveKey(AnnotationMutatedAt))
	})

	It("does not re-stamp the mutation time on a second snapshot (partner refresh)", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		firstStamp := pdb.Annotations[AnnotationMutatedAt]

		// Partner changes the PDB; we re-snapshot their new intent.
		mu := intstr.FromString("15%")
		pdb.Spec.MaxUnavailable = &mu
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		Expect(pdb.Annotations[AnnotationMutatedAt]).To(Equal(firstStamp))

		// Restore returns the partner's LATEST intent (15%).
		changed, err := restorePDBSpec(pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		Expect(pdb.Spec.MaxUnavailable.StrVal).To(Equal("15%"))
	})

	It("is a no-op for an unmutated PDB", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		changed, err := restorePDBSpec(pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeFalse())
	})

	It("returns an error for a corrupt snapshot", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationOriginalPDBSpec: "{not-json"}
		_, err := restorePDBSpec(pdb)
		Expect(err).To(HaveOccurred())
	})
})
