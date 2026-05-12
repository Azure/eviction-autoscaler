package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("HPASurgeApplier", func() {
	Describe("ApplySurge with fake client", func() {
		var (
			ctx     context.Context
			hpa     *autoscalingv2.HorizontalPodAutoscaler
			deploy  *appsv1.Deployment
			applier *HPASurgeApplier
		)

		BeforeEach(func() {
			ctx = context.Background()
			hpa = &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "test-hpa", Namespace: "default"},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					MinReplicas: ptr.To(int32(1)),
					MaxReplicas: 5,
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "test-deploy",
					},
				},
			}
			deploy = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
			}
			scheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			Expect(autoscalingv2.AddToScheme(scheme)).To(Succeed())
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(deploy, hpa).
				Build()

			target := &DeploymentWrapper{obj: deploy}
			applier = &HPASurgeApplier{client: fc, hpa: hpa, target: target}
		})

		It("should preserve original-min annotation when re-surging with a different value", func() {
			// First surge: 1 -> 2
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())

			var afterFirst autoscalingv2.HorizontalPodAutoscaler
			Expect(applier.client.Get(ctx, keyFor(hpa), &afterFirst)).To(Succeed())
			Expect(afterFirst.Annotations).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "1"))
			Expect(afterFirst.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "2"))

			// Simulate re-surge with a different value
			applier.hpa = &afterFirst
			Expect(applier.ApplySurge(ctx, 3)).To(Succeed())

			var afterSecond autoscalingv2.HorizontalPodAutoscaler
			Expect(applier.client.Get(ctx, keyFor(hpa), &afterSecond)).To(Succeed())
			// The surge annotation should reflect the new surge value
			Expect(afterSecond.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "3"))
			Expect(*afterSecond.Spec.MinReplicas).To(Equal(int32(3)))
			// But original-min must still be 1, NOT the intermediate surged value of 2
			Expect(afterSecond.Annotations).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "1"))
		})

		It("should revert to the true original after re-surging with a different value", func() {
			// First surge: 1 -> 2
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())

			// Re-surge: 2 -> 3
			var afterFirst autoscalingv2.HorizontalPodAutoscaler
			Expect(applier.client.Get(ctx, keyFor(hpa), &afterFirst)).To(Succeed())
			applier.hpa = &afterFirst
			Expect(applier.ApplySurge(ctx, 3)).To(Succeed())

			// Revert — should restore to 1 (the true original), not 2
			var afterSecond autoscalingv2.HorizontalPodAutoscaler
			Expect(applier.client.Get(ctx, keyFor(hpa), &afterSecond)).To(Succeed())
			applier.hpa = &afterSecond
			Expect(applier.RevertSurge(ctx, 99)).To(Succeed()) // 99 should be overridden by annotation

			var reverted autoscalingv2.HorizontalPodAutoscaler
			Expect(applier.client.Get(ctx, keyFor(hpa), &reverted)).To(Succeed())
			Expect(*reverted.Spec.MinReplicas).To(Equal(int32(1)))
			Expect(reverted.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
			Expect(reverted.Annotations).ToNot(HaveKey(OriginalMinReplicasAnnotationKey))
		})
	})
})
