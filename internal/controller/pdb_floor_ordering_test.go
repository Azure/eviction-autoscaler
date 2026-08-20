package controllers

import (
	"context"
	"errors"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
