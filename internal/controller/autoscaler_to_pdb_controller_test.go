package controllers

import (
	"context"

	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// asAlwaysEnabledFilter enables the eviction autoscaler for every namespace.
type asAlwaysEnabledFilter struct{}

func (asAlwaysEnabledFilter) Filter(_ context.Context, _ namespacefilter.Reader, _ string) (bool, error) {
	return true, nil
}

// These specs exercise the HPA/KEDA -> PDB reconciler's floor-pin awareness: when a
// user PDB carries our pin, an autoscaler-floor change must re-derive the floor at the
// new base (mirroring DeploymentToPDBReconciler), not overwrite it with the raw min.
var _ = Describe("AutoscalerToPDBReconciler re-pin", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(autoscalingv2.AddToScheme(scheme)).To(Succeed())
		Expect(policyv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
		pdbFloorMutationEnabled = true
	})

	AfterEach(func() {
		pdbFloorMutationEnabled = false
	})

	labels := map[string]string{"app": "app"}

	deployment := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(12)),
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
				},
			},
		}
	}

	hpa := func(minReplicas int32, surging bool) *autoscalingv2.HorizontalPodAutoscaler {
		h := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "app"},
				MinReplicas:    ptr.To(minReplicas),
				MaxReplicas:    50,
			},
		}
		if surging {
			h.Annotations = map[string]string{EvictionSurgeReplicasAnnotationKey: "20"}
		}
		return h
	}

	// pinnedUserPDB is a user-owned PDB (no ownedBy) whose original was maxUnavailable:25%,
	// currently pinned to an absolute floor computed at the old base.
	pinnedUserPDB := func(oldFloor int32) *policyv1.PodDisruptionBudget {
		mu := intstr.FromString("25%")
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector:       &metav1.LabelSelector{MatchLabels: labels},
				MaxUnavailable: &mu,
			},
		}
		Expect(snapshotPDBSpec(pdb)).To(Succeed())
		pinPDBFloor(pdb, oldFloor)
		return pdb
	}

	reconcileFor := func(objs ...runtime.Object) (*AutoscalerToPDBReconciler, error) {
		builder := fake.NewClientBuilder().WithScheme(scheme)
		for _, o := range objs {
			builder = builder.WithRuntimeObjects(o)
		}
		r := &AutoscalerToPDBReconciler{Client: builder.Build(), Scheme: scheme, Filter: asAlwaysEnabledFilter{}}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "app", Namespace: "default"}})
		return r, err
	}

	getPDB := func(r *AutoscalerToPDBReconciler) *policyv1.PodDisruptionBudget {
		out := &policyv1.PodDisruptionBudget{}
		Expect(r.Get(ctx, types.NamespacedName{Name: "app", Namespace: "default"}, out)).To(Succeed())
		return out
	}

	It("re-pins a pinned user PDB at the new HPA floor", func() {
		// Old base 8 -> floor 6 (8 - ceil(25% of 8)). New HPA min 12 -> floor 9 (12 - ceil(25% of 12)).
		r, err := reconcileFor(deployment(), hpa(12, false), pinnedUserPDB(6))
		Expect(err).NotTo(HaveOccurred())

		got := getPDB(r)
		Expect(got.Spec.MaxUnavailable).To(BeNil())
		Expect(got.Spec.MinAvailable).NotTo(BeNil())
		Expect(got.Spec.MinAvailable.IntVal).To(Equal(int32(9)))
		Expect(got.Annotations).To(HaveKey(AnnotationOriginalPDBSpec))
		Expect(got.Annotations[AnnotationPinnedFloor]).To(Equal("9"))
	})

	It("is idempotent when the pin already matches the new floor", func() {
		r, err := reconcileFor(deployment(), hpa(12, false), pinnedUserPDB(9))
		Expect(err).NotTo(HaveOccurred())

		got := getPDB(r)
		Expect(got.Spec.MinAvailable.IntVal).To(Equal(int32(9)))
		Expect(got.ResourceVersion).To(Equal("999")) // fake client seeds RV 999; no write
	})

	It("leaves a non-pinned user PDB untouched", func() {
		mu := intstr.FromString("25%")
		user := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector:       &metav1.LabelSelector{MatchLabels: labels},
				MaxUnavailable: &mu,
			},
		}
		r, err := reconcileFor(deployment(), hpa(12, false), user)
		Expect(err).NotTo(HaveOccurred())

		got := getPDB(r)
		Expect(got.Spec.MinAvailable).To(BeNil())
		Expect(got.Spec.MaxUnavailable.StrVal).To(Equal("25%"))
		Expect(got.ResourceVersion).To(Equal("999")) // untouched
	})

	It("still updates a controller-owned PDB to the raw autoscaler min", func() {
		three := intstr.FromInt32(3)
		owned := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "app",
				Namespace:   "default",
				Annotations: map[string]string{PDBOwnedByAnnotationKey: ControllerName},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector:     &metav1.LabelSelector{MatchLabels: labels},
				MinAvailable: &three,
			},
		}
		r, err := reconcileFor(deployment(), hpa(5, false), owned)
		Expect(err).NotTo(HaveOccurred())

		got := getPDB(r)
		Expect(got.Spec.MinAvailable.IntVal).To(Equal(int32(5)))
	})

	It("skips re-pin while a surge is active on the autoscaler", func() {
		r, err := reconcileFor(deployment(), hpa(12, true), pinnedUserPDB(6))
		Expect(err).NotTo(HaveOccurred())

		got := getPDB(r)
		Expect(got.Spec.MinAvailable.IntVal).To(Equal(int32(6))) // pin preserved during surge
		Expect(got.ResourceVersion).To(Equal("999"))
	})
})
