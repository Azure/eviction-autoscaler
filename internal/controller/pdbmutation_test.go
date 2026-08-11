package controllers

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
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

var _ = Describe("pdbSpecMatchesSnapshot", func() {
	It("is false when the PDB is not mutated", func() {
		match, err := pdbSpecMatchesSnapshot(pdbWithMaxUnavailable(intstr.FromInt32(1)))
		Expect(err).NotTo(HaveOccurred())
		Expect(match).To(BeFalse())
	})

	It("detects whether live floor fields byte-match the snapshot", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		match, err := pdbSpecMatchesSnapshot(pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(match).To(BeTrue())

		pdb.Spec.MaxUnavailable = nil
		pdb.Spec.MinAvailable = ptr.To(intstr.FromInt32(4))
		match, err = pdbSpecMatchesSnapshot(pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(match).To(BeFalse())
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
	It("rejects non-positive values (a floor is always a positive minAvailable)", func() {
		for _, v := range []string{"0", "-1"} {
			pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
			pdb.Annotations = map[string]string{AnnotationPinnedFloor: v}
			_, ok := pinnedFloorFromPDB(pdb)
			Expect(ok).To(BeFalse(), "value %q should be rejected", v)
		}
	})
})

var _ = Describe("snapshot + restore round-trip", func() {
	It("restores the original spec and clears annotations", func() {
		orig := pdbWithMaxUnavailable(intstr.FromInt32(1))

		Expect(snapshotPDBSpec(orig)).To(Succeed())
		Expect(isMutated(orig)).To(BeTrue())
		pinPDBFloor(orig, 101)
		Expect(pdbCarriesFloor(orig, 101)).To(BeTrue())

		changed, err := restorePDBSpec(orig)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		Expect(orig.Spec.MinAvailable).To(BeNil())
		Expect(orig.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(orig.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(orig.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
	})

	It("preserves a partner's concurrent change to a non-floor field on restore", func() {
		orig := pdbWithMaxUnavailable(intstr.FromInt32(1))
		orig.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "old"}}

		Expect(snapshotPDBSpec(orig)).To(Succeed())
		pinPDBFloor(orig, 101)
		Expect(pdbCarriesFloor(orig, 101)).To(BeTrue())

		// Partner edits a non-floor field (selector) mid-drain while our floor pin
		// is intact. The re-snapshot guard keys on the floor fields, so this does
		// NOT re-snapshot — the surgical restore must not revert it.
		orig.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "new"}}

		changed, err := restorePDBSpec(orig)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		Expect(orig.Spec.MinAvailable).To(BeNil())
		Expect(orig.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(orig.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(orig.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app", "new"))
	})

	It("returns an error for a corrupt snapshot", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pdb.Annotations = map[string]string{AnnotationOriginalPDBSpec: "{not-json"}
		_, err := restorePDBSpec(pdb)
		Expect(err).To(HaveOccurred())
	})

	It("drops a stale pinned-floor annotation when the snapshot is absent", func() {
		// The snapshot was externally removed but the pin spec + pinned-floor
		// annotation remain — restore must still clear the stale floor annotation.
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		pinPDBFloor(pdb, 90) // sets minAvailable:90 + AnnotationPinnedFloor, no snapshot
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))

		changed, err := restorePDBSpec(pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
		// No snapshot to restore, so the live spec is left as-is.
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(90)))
	})

	It("is a no-op when neither snapshot nor pinned-floor annotation is present", func() {
		pdb := pdbWithMaxUnavailable(intstr.FromInt32(1))
		changed, err := restorePDBSpec(pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeFalse())
	})
})

var _ = Describe("desiredHealthyAt", func() {
	It("computes replicas - maxUnavailable for an absolute maxUnavailable", func() {
		mu := intstr.FromInt32(1)
		dh, err := desiredHealthyAt(policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu}, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(dh).To(Equal(int32(99)))
	})
	It("computes replicas - maxUnavailable for a percentage maxUnavailable (rounded up)", func() {
		mu := intstr.FromString("10%")
		dh, err := desiredHealthyAt(policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu}, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(dh).To(Equal(int32(90)))
	})
	It("returns the absolute minAvailable directly", func() {
		ma := intstr.FromInt32(5)
		dh, err := desiredHealthyAt(policyv1.PodDisruptionBudgetSpec{MinAvailable: &ma}, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(dh).To(Equal(int32(5)))
	})
	It("resolves a percentage minAvailable against replicas (rounded up)", func() {
		ma := intstr.FromString("90%")
		dh, err := desiredHealthyAt(policyv1.PodDisruptionBudgetSpec{MinAvailable: &ma}, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(dh).To(Equal(int32(90)))
	})
	It("clamps below zero when maxUnavailable exceeds replicas", func() {
		mu := intstr.FromInt32(5)
		dh, err := desiredHealthyAt(policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu}, 3)
		Expect(err).NotTo(HaveOccurred())
		Expect(dh).To(Equal(int32(0)))
	})
	It("returns 0 for a PDB expressing no budget", func() {
		dh, err := desiredHealthyAt(policyv1.PodDisruptionBudgetSpec{}, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(dh).To(Equal(int32(0)))
	})
})
