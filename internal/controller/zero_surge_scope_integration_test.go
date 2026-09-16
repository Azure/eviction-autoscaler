package controllers

import (
	"context"
	"time"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Exercises the ZeroSurgeOverride namespace-scope contract end-to-end through the real
// reconcile loop against envtest: a maxSurge:0 workload can only be surged where the
// override applies, and an owned surge is safely reverted if the namespace later leaves
// the override scope.
var _ = Describe("EvictionAutoScaler zero-maxSurge override namespace scope", func() {
	ctx := context.Background()
	const resourceName = "zso-resource"
	const deploymentName = "zso-deployment"

	var namespace string
	var eaKey, depKey types.NamespacedName

	scopeOf := func(ns ...string) map[string]struct{} {
		m := make(map[string]struct{}, len(ns))
		for _, n := range ns {
			m[n] = struct{}{}
		}
		return m
	}

	BeforeEach(func() {
		nsObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "zso-",
				Annotations:  map[string]string{namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true"},
			},
		}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		namespace = nsObj.Name
		eaKey = types.NamespacedName{Name: resourceName, Namespace: namespace}
		depKey = types.NamespacedName{Name: deploymentName, Namespace: namespace}

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: deploymentName, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		// A maxSurge:0 deployment cannot surge on its own — only the override can drive it.
		zeroSurge := intstr.FromInt(0)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "zso-example"}},
				Strategy: appsv1.DeploymentStrategy{RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &zeroSurge}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "zso-example"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &intstr.IntOrString{IntVal: 1},
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "zso-example"}},
			},
			Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "zso-node-" + namespace},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		for i := 0; i < 2; i++ {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "zso-pod-", Namespace: namespace, Labels: map[string]string{"app": "zso-example"}},
				Spec:       corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		}
	})

	// newReconciler builds a reconciler with the given override scope (nil scope ⇒ fleet-wide).
	newReconciler := func(scope map[string]struct{}) *EvictionAutoScalerReconciler {
		override := intstr.FromString("100%") // 100% of minReplicas ⇒ generous ceiling so a surge fires
		return &EvictionAutoScalerReconciler{
			Client:                      k8sClient,
			Scheme:                      k8sClient.Scheme(),
			Filter:                      &evictionTestFilter{},
			ZeroSurgeOverride:           &override,
			ZeroSurgeOverrideNamespaces: scope,
		}
	}

	// driveSurge runs a baseline reconcile, records an eviction, then reconciles again so the
	// blocked-drain path can attempt a surge.
	driveSurge := func(r *EvictionAutoScalerReconciler) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: eaKey})
		Expect(err).NotTo(HaveOccurred())

		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaKey, ea)).To(Succeed())
		ea.Spec.LastEviction = v1.Eviction{PodName: "displaced-pod", EvictionTime: metav1.Now()}
		Expect(k8sClient.Update(ctx, ea)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: eaKey})
		Expect(err).NotTo(HaveOccurred())
	}

	reconcileOnce := func(r *EvictionAutoScalerReconciler) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: eaKey})
		Expect(err).NotTo(HaveOccurred())
	}

	surgeAnnotationPresent := func() bool {
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		_, ok := dep.Annotations[EvictionSurgeReplicasAnnotationKey]
		return ok
	}

	replicas := func() int32 {
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		return *dep.Spec.Replicas
	}

	It("surges an in-scope maxSurge:0 workload", func() {
		driveSurge(newReconciler(scopeOf(namespace)))
		Expect(replicas()).To(BeNumerically(">", int32(1)))
		Expect(surgeAnnotationPresent()).To(BeTrue())
	})

	It("is fleet-wide when the scope is empty", func() {
		driveSurge(newReconciler(scopeOf()))
		Expect(replicas()).To(BeNumerically(">", int32(1)))
		Expect(surgeAnnotationPresent()).To(BeTrue())
	})

	It("degrades an out-of-scope maxSurge:0 workload without surging", func() {
		driveSurge(newReconciler(scopeOf("some-other-namespace")))
		Expect(replicas()).To(Equal(int32(1)))
		Expect(surgeAnnotationPresent()).To(BeFalse())

		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaKey, ea)).To(Succeed())
		cond := meta.FindStatusCondition(ea.Status.Conditions, "Degraded")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("UnsupportedAutoscalerConfiguration"))
	})

	It("reverts an owned surge when the namespace leaves the override scope", func() {
		By("surging while in scope")
		driveSurge(newReconciler(scopeOf(namespace)))
		Expect(replicas()).To(BeNumerically(">", int32(1)))
		Expect(surgeAnnotationPresent()).To(BeTrue())

		By("reconciling after the namespace has left scope until the surge is torn down")
		// The first pass re-syncs the surge-bumped generation; a later pass reaches the surge
		// decision and reverts. Reconcile until the state converges rather than assuming a
		// fixed pass count.
		outOfScope := newReconciler(scopeOf("some-other-namespace"))
		Eventually(func(g Gomega) {
			reconcileOnce(outOfScope)
			g.Expect(replicas()).To(Equal(int32(1)))
			g.Expect(surgeAnnotationPresent()).To(BeFalse())
		}, 10*time.Second, 10*time.Millisecond).Should(Succeed())

		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaKey, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeFalse())
	})

	It("does not report a revert (nor clear the pin/gauges) when RevertSurge refuses a lost baseline", func() {
		By("simulating an owned Deployment surge whose baseline is unrecoverable")
		// Surge marker == live replicas so ownsActiveSurge passes; original-min-replicas="0"
		// combined with Status.MinReplicas=0 makes DeploymentSurgeApplier.RevertSurge refuse
		// (it will not scale to a non-positive baseline) and return nil WITHOUT reverting.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		dep.Spec.Replicas = ptr.To(int32(2))
		if dep.Annotations == nil {
			dep.Annotations = map[string]string{}
		}
		dep.Annotations[EvictionSurgeReplicasAnnotationKey] = "2"
		dep.Annotations[OriginalMinReplicasAnnotationKey] = "0"
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())

		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaKey, ea)).To(Succeed())
		ea.Status.MinReplicas = 0
		ea.Status.PDBFloorPinned = true
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		target, err := GetSurger("deployment")
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, depKey, target.Obj())).To(Succeed())

		reverted, err := newReconciler(scopeOf("some-other-namespace")).revertOwnedSurgeIfHeld(ctx, ea, target)
		Expect(err).NotTo(HaveOccurred())
		Expect(reverted).To(BeFalse(), "a refused RevertSurge must not be reported as a revert")

		By("verifying the surge is intentionally left intact")
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
		_, hasMarker := dep.Annotations[EvictionSurgeReplicasAnnotationKey]
		Expect(hasMarker).To(BeTrue())
	})
})
