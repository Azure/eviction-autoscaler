package controllers

import (
	"context"
	"errors"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// White-box test of the finalizer-before-pin crash-safety invariant in actuatePDBFloor:
// the PDBFloorFinalizer must be persisted on the EAS *before* the PDB pin write, so a crash
// (here, a failing pin write) can never leave a pinned PDB with no finalizer to drive teardown.
var _ = Describe("actuatePDBFloor finalizer-before-pin ordering", func() {
	It("persists the PDB-floor finalizer even when the pin write fails, and leaves the PDB unpinned", func() {
		ctx := context.Background()
		const nm, nsp = "order-ea", "order-ns"
		key := client.ObjectKey{Name: nm, Namespace: nsp}

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp},
			Status:     v1.EvictionAutoScalerStatus{PDBFloorPinned: true, MinReplicas: 5},
		}
		mu := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &mu,
				Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "order"}},
			},
		}

		scheme := runtime.NewScheme()
		Expect(v1.AddToScheme(scheme)).To(Succeed())
		Expect(policyv1.AddToScheme(scheme)).To(Succeed())

		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ea, pdb).Build()
		// Reject the pin write (PDB Update) but allow the finalizer write (EAS Update), so we
		// can observe the state after the finalizer persisted but before the pin lands.
		blocked := interceptor.NewClient(fc, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*policyv1.PodDisruptionBudget); ok {
					return errors.New("pin update blocked")
				}
				return c.Update(ctx, obj, opts...)
			},
		})

		r := &PDBToEvictionAutoScalerReconciler{Client: blocked, Scheme: scheme}

		// The pin write fails ...
		Expect(r.actuatePDBFloor(ctx, pdb, ea)).To(HaveOccurred())

		// ... but the finalizer was already persisted on the EAS before the pin (happens-before).
		gotEA := &v1.EvictionAutoScaler{}
		Expect(fc.Get(ctx, key, gotEA)).To(Succeed())
		Expect(gotEA.Finalizers).To(ContainElement(PDBFloorFinalizer))

		// ... and the partner PDB was left un-pinned — never a pinned PDB without a finalizer.
		gotPDB := &policyv1.PodDisruptionBudget{}
		Expect(fc.Get(ctx, key, gotPDB)).To(Succeed())
		Expect(gotPDB.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(gotPDB.Spec.MinAvailable).To(BeNil())
	})
})

// White-box test of the delete-time gauge cleanup: this controller holds no finalizer on the
// PDB, so a still-mutated/pinned PDB can be deleted at any time. The reconcile's !pdbFound path
// must clear pdb_mutated and pdb_floor_pinned so they don't leak a stuck ==1 series against a
// PDB that no longer exists and can never be reconciled back to 0.
var _ = Describe("PDB-floor gauge cleanup on PDB deletion", func() {
	It("clears pdb_mutated and pdb_floor_pinned when the PDB no longer exists", func() {
		ctx := context.Background()
		const nm, nsp = "gone-pdb", "gone-ns"

		scheme := runtime.NewScheme()
		Expect(v1.AddToScheme(scheme)).To(Succeed())
		Expect(policyv1.AddToScheme(scheme)).To(Succeed())
		// Neither the PDB nor an EAS exists: the reconcile takes the !pdbFound early return.
		fc := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &PDBToEvictionAutoScalerReconciler{Client: fc, Scheme: scheme}

		// Isolate the vecs so CollectAndCount reflects only this test's series — ToFloat64 +
		// WithLabelValues would recreate a deleted series at 0 and mask a missing-cleanup bug.
		metrics.PDBMutated.Reset()
		metrics.PDBFloorPinned.Reset()
		// Seed stuck gauges as if a mutated + pinned PDB had just been deleted.
		metrics.PDBMutated.WithLabelValues(nsp, nm).Set(1)
		metrics.PDBFloorPinned.WithLabelValues(nsp, nm, nm).Set(1)
		Expect(testutil.CollectAndCount(metrics.PDBMutated)).To(Equal(1), "precondition: one pdb_mutated series present")
		Expect(testutil.CollectAndCount(metrics.PDBFloorPinned)).To(Equal(1), "precondition: one pdb_floor_pinned series present")

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: nm, Namespace: nsp}})
		Expect(err).NotTo(HaveOccurred())

		Expect(testutil.CollectAndCount(metrics.PDBMutated)).
			To(Equal(0), "pdb_mutated series must be deleted (absent), not merely 0, when the PDB is gone")
		Expect(testutil.CollectAndCount(metrics.PDBFloorPinned)).
			To(Equal(0), "pdb_floor_pinned series must be deleted (absent), not merely 0, when the PDB is gone")
	})
})

// White-box tests of pdb_mutated transition timing: it must flip to 1 immediately after a
// successful pin (not lag a reconcile), and must stay 1 when a restore FAILS (a genuine stuck
// mutation), since the post-restore Set(0) only runs after the restore actually persists.
var _ = Describe("pdb_mutated transition timing", func() {
	It("sets pdb_mutated=1 immediately after a successful pin", func() {
		ctx := context.Background()
		const nm, nsp = "pin-ok-ea", "pin-ok-ns"
		scheme := runtime.NewScheme()
		Expect(v1.AddToScheme(scheme)).To(Succeed())
		Expect(policyv1.AddToScheme(scheme)).To(Succeed())

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp},
			Status:     v1.EvictionAutoScalerStatus{PDBFloorPinned: true, MinReplicas: 5},
		}
		mu := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &mu,
				Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pinok"}},
			},
		}
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ea, pdb).Build()
		r := &PDBToEvictionAutoScalerReconciler{Client: fc, Scheme: scheme}

		metrics.PDBMutated.WithLabelValues(nsp, nm).Set(0)
		Expect(r.actuatePDBFloor(ctx, pdb, ea)).To(Succeed())

		// The pin persisted, so the PDB now carries our mutation and the gauge reflects it now.
		Expect(isMutated(pdb)).To(BeTrue(), "the PDB should carry the floor mutation after a pin")
		Expect(testutil.ToFloat64(metrics.PDBMutated.WithLabelValues(nsp, nm))).
			To(Equal(1.0), "pdb_mutated must be 1 immediately after a successful pin")
	})

	It("keeps pdb_mutated=1 when a restore fails (stuck mutation)", func() {
		ctx := context.Background()
		const nm, nsp = "restore-fail-ea", "restore-fail-ns"
		scheme := runtime.NewScheme()
		Expect(v1.AddToScheme(scheme)).To(Succeed())
		Expect(policyv1.AddToScheme(scheme)).To(Succeed())

		// A PDB that already carries our floor (snapshot + pinned annotations).
		mu := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &mu,
				Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "restore"}},
			},
		}
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		pinPDBFloor(pdb, 4)
		Expect(isMutated(pdb)).To(BeTrue())

		// Pin intent is cleared → restore path; but block the PDB Update so the restore fails.
		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: nm, Namespace: nsp},
			Status:     v1.EvictionAutoScalerStatus{PDBFloorPinned: false, MinReplicas: 5},
		}
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ea).Build()
		blocked := interceptor.NewClient(fc, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*policyv1.PodDisruptionBudget); ok {
					return errors.New("restore update blocked")
				}
				return c.Update(ctx, obj, opts...)
			},
		})
		r := &PDBToEvictionAutoScalerReconciler{Client: blocked, Scheme: scheme}

		metrics.PDBMutated.WithLabelValues(nsp, nm).Set(1)
		Expect(r.actuatePDBFloor(ctx, pdb, ea)).To(HaveOccurred())

		// Restore failed → the PDB is still mutated, so the gauge must remain 1 (the post-restore
		// Set(0) only runs after a successful persist).
		Expect(testutil.ToFloat64(metrics.PDBMutated.WithLabelValues(nsp, nm))).
			To(Equal(1.0), "pdb_mutated must stay 1 when the restore fails (genuine stuck mutation)")
	})
})
