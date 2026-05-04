package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func createHPA(name, namespace, targetDeployment string, minReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       targetDeployment,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: 10,
		},
	}
}

var _ = Describe("HPASurgeApplier", func() {
	var (
		namespace string
		ctx       context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespaceObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-hpa-surge-",
			},
		}
		Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
		namespace = namespaceObj.Name
	})

	It("should update HPA minReplicas and annotate HPA on ApplySurge", func() {
		// Create deployment and HPA
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-apply", namespace, "hpa-apply", 1, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		hpa := createHPA("hpa-apply", namespace, "hpa-apply", 1)
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &HPASurgeApplier{client: k8sClient, hpa: hpa, target: target}

		err := applier.ApplySurge(ctx, 3)
		Expect(err).ToNot(HaveOccurred())

		// Verify HPA was updated
		var updatedHPA autoscalingv2.HorizontalPodAutoscaler
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hpa), &updatedHPA)).To(Succeed())
		Expect(*updatedHPA.Spec.MinReplicas).To(Equal(int32(3)))
		Expect(updatedHPA.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "3"))
		Expect(updatedHPA.Annotations).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "1"))

		// Verify deployment replicas were set directly
		var updatedDep appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updatedDep)).To(Succeed())
		Expect(*updatedDep.Spec.Replicas).To(Equal(int32(3)))
	})

	It("should be idempotent on retry (skip HPA update if already surged)", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-idempotent", namespace, "hpa-idempotent", 1, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		hpa := createHPA("hpa-idempotent", namespace, "hpa-idempotent", 1)
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &HPASurgeApplier{client: k8sClient, hpa: hpa, target: target}

		// First call
		Expect(applier.ApplySurge(ctx, 3)).To(Succeed())

		// Re-fetch to get fresh objects (simulating a retry reconcile)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hpa), hpa)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), dep)).To(Succeed())
		applier = &HPASurgeApplier{client: k8sClient, hpa: hpa, target: &DeploymentWrapper{obj: dep}}

		// Second call should succeed without conflict
		Expect(applier.ApplySurge(ctx, 3)).To(Succeed())

		// Values should still be correct
		var updatedHPA autoscalingv2.HorizontalPodAutoscaler
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hpa), &updatedHPA)).To(Succeed())
		Expect(*updatedHPA.Spec.MinReplicas).To(Equal(int32(3)))
		Expect(updatedHPA.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "3"))
	})

	It("should store original minReplicas in annotation", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-original", namespace, "hpa-original", 1, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		// HPA starts with minReplicas=2
		hpa := createHPA("hpa-original", namespace, "hpa-original", 2)
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &HPASurgeApplier{client: k8sClient, hpa: hpa, target: target}

		Expect(applier.ApplySurge(ctx, 4)).To(Succeed())

		var updatedHPA autoscalingv2.HorizontalPodAutoscaler
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hpa), &updatedHPA)).To(Succeed())
		// Original was 2, surged to 4
		Expect(updatedHPA.Annotations).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "2"))
		Expect(*updatedHPA.Spec.MinReplicas).To(Equal(int32(4)))
	})

	It("should revert HPA minReplicas using original annotation on RevertSurge", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-revert", namespace, "hpa-revert", 3, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		// HPA is in surged state: minReplicas=3, annotations set
		hpa := createHPA("hpa-revert", namespace, "hpa-revert", 3)
		hpa.Annotations = map[string]string{
			EvictionSurgeReplicasAnnotationKey: "3",
			OriginalMinReplicasAnnotationKey:   "1",
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &HPASurgeApplier{client: k8sClient, hpa: hpa, target: target}

		// RevertSurge passes EA.Status.MinReplicas but HPASurgeApplier reads from annotation
		Expect(applier.RevertSurge(ctx, 999)).To(Succeed()) // 999 should be ignored

		var updatedHPA autoscalingv2.HorizontalPodAutoscaler
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hpa), &updatedHPA)).To(Succeed())
		// Should revert to 1 (from annotation), not 999
		Expect(*updatedHPA.Spec.MinReplicas).To(Equal(int32(1)))
		Expect(updatedHPA.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
		Expect(updatedHPA.Annotations).ToNot(HaveKey(OriginalMinReplicasAnnotationKey))
	})

	It("should fall back to passed originalMinReplicas when annotation is missing", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-fallback", namespace, "hpa-fallback", 3, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		// HPA in surged state but missing the original annotation
		hpa := createHPA("hpa-fallback", namespace, "hpa-fallback", 3)
		hpa.Annotations = map[string]string{
			EvictionSurgeReplicasAnnotationKey: "3",
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &HPASurgeApplier{client: k8sClient, hpa: hpa, target: target}

		Expect(applier.RevertSurge(ctx, 2)).To(Succeed())

		var updatedHPA autoscalingv2.HorizontalPodAutoscaler
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hpa), &updatedHPA)).To(Succeed())
		// Falls back to passed value since annotation is missing
		Expect(*updatedHPA.Spec.MinReplicas).To(Equal(int32(2)))
	})

	It("should not set deployment replicas on RevertSurge (let HPA handle scale-down)", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-no-scaledown", namespace, "hpa-no-scaledown", 3, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		hpa := createHPA("hpa-no-scaledown", namespace, "hpa-no-scaledown", 3)
		hpa.Annotations = map[string]string{
			EvictionSurgeReplicasAnnotationKey: "3",
			OriginalMinReplicasAnnotationKey:   "1",
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &HPASurgeApplier{client: k8sClient, hpa: hpa, target: target}

		Expect(applier.RevertSurge(ctx, 1)).To(Succeed())

		// Deployment replicas should NOT have changed (still 3)
		var updatedDep appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updatedDep)).To(Succeed())
		Expect(*updatedDep.Spec.Replicas).To(Equal(int32(3)))
	})

	Context("IsSurgeActive", func() {
		It("should return true when HPA has surge annotation", func() {
			hpa := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						EvictionSurgeReplicasAnnotationKey: "3",
					},
				},
			}
			applier := &HPASurgeApplier{hpa: hpa}
			Expect(applier.IsSurgeActive()).To(BeTrue())
		})

		It("should return false when HPA has no surge annotation", func() {
			hpa := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"other": "value"},
				},
			}
			applier := &HPASurgeApplier{hpa: hpa}
			Expect(applier.IsSurgeActive()).To(BeFalse())
		})

		It("should return false when HPA has nil annotations", func() {
			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			applier := &HPASurgeApplier{hpa: hpa}
			Expect(applier.IsSurgeActive()).To(BeFalse())
		})
	})

	It("should return 'hpa' as Name", func() {
		applier := &HPASurgeApplier{}
		Expect(applier.Name()).To(Equal("hpa"))
	})
})
