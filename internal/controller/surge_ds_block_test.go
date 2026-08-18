package controllers

import (
	"context"
	"errors"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// rejectWritesInNamespace rejects main-resource updates in the given namespace using the supplied
// error. Status-subresource writes still reach the underlying client.
func rejectWritesInNamespace(namespace string, rejection func(client.Object) error) interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetNamespace() == namespace {
				return rejection(obj)
			}
			return c.Update(ctx, obj, opts...)
		},
	}
}

// forbidWritesInNamespace simulates Deployment Safeguards rejecting every main-resource write in
// an AKS-owned namespace while allowing the controller to persist status.
func forbidWritesInNamespace(namespace string) interceptor.Funcs {
	return rejectWritesInNamespace(namespace, func(obj client.Object) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "resource"}, obj.GetName(), nil)
	})
}

// Drives the real EvictionAutoScaler reconcile loop against envtest with Deployment Safeguards
// blocking writes to the (protected) namespace, across all three surge strategies (Deployment,
// HPA, KEDA). Each path must record the forbidden write as terminal, not requeue, not scale the
// deployment, and not panic.
var _ = Describe("EvictionAutoScaler controller when Deployment Safeguards blocks writes to the namespace", func() {
	ctx := context.Background()
	const resourceName = "ds-block-resource"
	const deploymentName = "ds-block-deployment"

	var namespace string
	var eaKey, depKey types.NamespacedName

	BeforeEach(func() {
		nsObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "ds-block-",
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

		surge := intstr.FromInt(1)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ds-example"}},
				Strategy: appsv1.DeploymentStrategy{RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ds-example"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &intstr.IntOrString{IntVal: 1},
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ds-example"}},
			},
			Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

		// A cordoned node with 2 matching pods makes the controller see displaced pods and surge.
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "ds-block-node-" + namespace},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		for i := 0; i < 2; i++ {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "ds-block-pod-", Namespace: namespace, Labels: map[string]string{"app": "ds-example"}},
				Spec:       corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		}
	})

	reconcileSurgeWith := func(intercepts interceptor.Funcs) (reconcile.Result, error) {
		setupReconciler := &EvictionAutoScalerReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Filter: &evictionTestFilter{},
		}
		_, err := setupReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaKey})
		Expect(err).NotTo(HaveOccurred())

		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaKey, ea)).To(Succeed())
		ea.Spec.LastEviction = v1.Eviction{PodName: "displaced-pod", EvictionTime: metav1.Now()}
		Expect(k8sClient.Update(ctx, ea)).To(Succeed())

		watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		dsReconciler := &EvictionAutoScalerReconciler{
			Client: interceptor.NewClient(watchClient, intercepts),
			Scheme: k8sClient.Scheme(),
			Filter: &evictionTestFilter{},
		}
		return dsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaKey})
	}

	// assertBlockedSurge runs a setup reconcile, logs an eviction, then runs the surge reconcile
	// with all writes to the namespace blocked. It asserts the reconcile does not panic or
	// requeue, records a Degraded condition, and leaves the deployment unscaled.
	assertBlockedSurge := func() {
		var result reconcile.Result
		var reconcileErr error
		Expect(func() {
			result, reconcileErr = reconcileSurgeWith(forbidWritesInNamespace(namespace))
		}).NotTo(Panic())
		Expect(reconcileErr).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))

		updatedEA := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaKey, updatedEA)).To(Succeed())
		condition := meta.FindStatusCondition(updatedEA.Status.Conditions, "Degraded")
		Expect(meta.FindStatusCondition(updatedEA.Status.Conditions, "Ready")).To(BeNil())
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		Expect(condition.Reason).To(Equal("SurgeForbidden"))
		Expect(condition.Message).To(ContainSubstring("forbidden"))
	}

	It("returns non-forbidden write errors so controller-runtime retries them", func() {
		result, err := reconcileSurgeWith(rejectWritesInNamespace(namespace, func(obj client.Object) error {
			return apierrors.NewConflict(
				schema.GroupResource{Resource: "resource"},
				obj.GetName(),
				errors.New("simulated conflict"),
			)
		}))

		Expect(apierrors.IsConflict(err)).To(BeTrue())
		Expect(result).To(Equal(reconcile.Result{}))

		updatedEA := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaKey, updatedEA)).To(Succeed())
		condition := meta.FindStatusCondition(updatedEA.Status.Conditions, "Degraded")
		Expect(condition).To(BeNil())
	})

	It("Deployment surge path: records forbidden, does not retry, scale, or panic", func() {
		assertBlockedSurge()
	})

	It("HPA surge path: records forbidden, does not retry, scale, or panic", func() {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "ds-hpa", Namespace: namespace},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: deploymentName},
				MinReplicas:    ptr.To(int32(1)),
				MaxReplicas:    5,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())
		assertBlockedSurge()
	})

	It("KEDA surge path: records forbidden, does not retry, scale, or panic", func() {
		so := &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Name: "ds-so", Namespace: namespace},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef:  &kedav1alpha1.ScaleTarget{Name: deploymentName, Kind: "Deployment"},
				MinReplicaCount: ptr.To(int32(1)),
				MaxReplicaCount: ptr.To(int32(5)),
				Triggers: []kedav1alpha1.ScaleTriggers{{
					Type:     "cpu",
					Metadata: map[string]string{"type": "Utilization", "value": "50"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, so)).To(Succeed())
		assertBlockedSurge()
	})
})
