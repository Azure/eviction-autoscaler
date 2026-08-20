package controllers

import (
	"context"
	"testing"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// reconcileSurgeTeardown selects the applier that actually owns the surge and reverts it on
// deletion. These cases exercise the two ownsActiveSurge arms the reconcile-level teardown
// relies on: the HPA/KEDA arm ("active marker is enough") and the deployment-marker fingerprint
// winning over a late-added HPA (so detectSurgeApplier can't mis-select and strand a surge).
// Kept as fast fake-client unit tests (no envtest): the Ginkgo suite covers the end-to-end
// pin/restore path, while these isolate the applier-selection branches.

const stdTeardownName, stdTeardownNS = "td-app", "td-ns"

func surgeTeardownReconciler(g *WithT, objs ...client.Object) *EvictionAutoScalerReconciler {
	scheme := runtime.NewScheme()
	g.Expect(v1.AddToScheme(scheme)).To(Succeed())
	g.Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(autoscalingv2.AddToScheme(scheme)).To(Succeed())
	g.Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &EvictionAutoScalerReconciler{Client: fc, Scheme: scheme}
}

func teardownEA() *v1.EvictionAutoScaler {
	return &v1.EvictionAutoScaler{
		ObjectMeta: metav1.ObjectMeta{Name: stdTeardownName, Namespace: stdTeardownNS, Finalizers: []string{EASSurgeFinalizer}},
		Spec:       v1.EvictionAutoScalerSpec{TargetName: stdTeardownName, TargetKind: "deployment"},
		// MinReplicas is deliberately distinct from the recorded original-min-replicas ("1") so
		// that a revert to 1 proves the durable-annotation baseline was used, not this fallback.
		Status: v1.EvictionAutoScalerStatus{MinReplicas: 9},
	}
}

// TestReconcileSurgeTeardownHPAOwnedSurge: the surge marker lives on the HPA (HPA/KEDA appliers
// mark their own object, not the deployment), so ownsActiveSurge takes the "active marker is
// enough" arm and RevertSurge resets the HPA floor.
func TestReconcileSurgeTeardownHPAOwnedSurge(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	key := client.ObjectKey{Name: stdTeardownName, Namespace: stdTeardownNS}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: stdTeardownName, Namespace: stdTeardownNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: stdTeardownName, Namespace: stdTeardownNS,
			Annotations: map[string]string{
				EvictionSurgeReplicasAnnotationKey: "3",
				OriginalMinReplicasAnnotationKey:   "1",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas:    ptr.To(int32(3)),
			MaxReplicas:    5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: stdTeardownName, APIVersion: "apps/v1"},
		},
	}
	ea := teardownEA()
	r := surgeTeardownReconciler(g, dep, hpa, ea)

	g.Expect(r.reconcileSurgeTeardown(ctx, ea)).To(Succeed())

	// HPA floor reverted to the recorded baseline (1); surge annotations cleared.
	var gotHPA autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(ctx, key, &gotHPA)).To(Succeed())
	g.Expect(gotHPA.Spec.MinReplicas).ToNot(BeNil())
	g.Expect(*gotHPA.Spec.MinReplicas).To(Equal(int32(1)))
	g.Expect(gotHPA.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	// Surge finalizer released.
	var gotEA v1.EvictionAutoScaler
	g.Expect(r.Get(ctx, key, &gotEA)).To(Succeed())
	g.Expect(gotEA.Finalizers).ToNot(ContainElement(EASSurgeFinalizer))
}

// TestReconcileSurgeTeardownDeploymentMarkerWins: a plain-Deployment surge marks the deployment;
// if an HPA is added after the surge, detectSurgeApplier would now pick the HPA — but
// hasTargetAnnotation must win so we revert the actually-surged deployment, not reset the HPA.
func TestReconcileSurgeTeardownDeploymentMarkerWins(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	key := client.ObjectKey{Name: stdTeardownName, Namespace: stdTeardownNS}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: stdTeardownName, Namespace: stdTeardownNS,
			Annotations: map[string]string{
				EvictionSurgeReplicasAnnotationKey: "3",
				OriginalMinReplicasAnnotationKey:   "1",
			},
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: stdTeardownName, Namespace: stdTeardownNS},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas:    ptr.To(int32(2)),
			MaxReplicas:    5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: stdTeardownName, APIVersion: "apps/v1"},
		},
	}
	ea := teardownEA()
	r := surgeTeardownReconciler(g, dep, hpa, ea)

	g.Expect(r.reconcileSurgeTeardown(ctx, ea)).To(Succeed())

	// Deployment reverted to the recorded baseline (1) via its own marker; annotations cleared.
	var gotDep appsv1.Deployment
	g.Expect(r.Get(ctx, key, &gotDep)).To(Succeed())
	g.Expect(gotDep.Spec.Replicas).ToNot(BeNil())
	g.Expect(*gotDep.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(gotDep.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	// The late HPA was left untouched (not selected as the applier).
	var gotHPA autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(ctx, key, &gotHPA)).To(Succeed())
	g.Expect(gotHPA.Spec.MinReplicas).ToNot(BeNil())
	g.Expect(*gotHPA.Spec.MinReplicas).To(Equal(int32(2)))
	// Surge finalizer released.
	var gotEA v1.EvictionAutoScaler
	g.Expect(r.Get(ctx, key, &gotEA)).To(Succeed())
	g.Expect(gotEA.Finalizers).ToNot(ContainElement(EASSurgeFinalizer))
}
