package controllers

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// createScaledObject creates a typed KEDA ScaledObject for testing.
//
//nolint:unparam // test helper — namespace may vary in future tests
func createScaledObject(name, namespace, targetDeployment string, minReplicaCount, maxReplicaCount int32) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{
				Name: targetDeployment,
				Kind: "Deployment",
			},
			MinReplicaCount: new(minReplicaCount),
			MaxReplicaCount: new(maxReplicaCount),
		},
	}
}

var _ = Describe("KEDASurgeApplier", func() {
	It("should return 'keda' as Name", func() {
		applier := &KEDASurgeApplier{}
		Expect(applier.Name()).To(Equal("keda"))
	})

	Describe("IsSurgeActive", func() {
		It("should return false when no annotations", func() {
			obj := createScaledObject("test-so", "default", "test-deploy", 1, 5)
			applier := &KEDASurgeApplier{scaledObject: obj}
			Expect(applier.IsSurgeActive()).To(BeFalse())
		})

		It("should return false when annotation is absent", func() {
			obj := createScaledObject("test-so", "default", "test-deploy", 1, 5)
			obj.SetAnnotations(map[string]string{"other": "value"})
			applier := &KEDASurgeApplier{scaledObject: obj}
			Expect(applier.IsSurgeActive()).To(BeFalse())
		})

		It("should return true when evictionSurgeReplicas annotation is present", func() {
			obj := createScaledObject("test-so", "default", "test-deploy", 1, 5)
			obj.SetAnnotations(map[string]string{EvictionSurgeReplicasAnnotationKey: "3"})
			applier := &KEDASurgeApplier{scaledObject: obj}
			Expect(applier.IsSurgeActive()).To(BeTrue())
		})
	})

	Describe("ApplySurge with fake client", func() {
		var (
			ctx     context.Context
			so      *kedav1alpha1.ScaledObject
			deploy  *appsv1.Deployment
			applier *KEDASurgeApplier
		)

		BeforeEach(func() {
			ctx = context.Background()
			so = createScaledObject("test-so", "default", "test-deploy", 1, 5)
			deploy = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(1))},
			}
			scheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(deploy, so).
				WithStatusSubresource(deploy).
				Build()

			target := &DeploymentWrapper{obj: deploy}
			applier = &KEDASurgeApplier{client: fc, scaledObject: so, target: target}
		})

		It("should set evictionSurgeReplicas and original-min-replicas on ScaledObject", func() {
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())

			// Re-read ScaledObject from fake client
			var updated kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &updated)).To(Succeed())

			Expect(updated.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "2"))
			Expect(updated.Annotations).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "1"))
			Expect(updated.Spec.MinReplicaCount).ToNot(BeNil())
			Expect(*updated.Spec.MinReplicaCount).To(Equal(int32(2)))
		})

		It("should set deployment replicas directly for immediate effect", func() {
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())

			var dep appsv1.Deployment
			Expect(applier.client.Get(ctx, keyFor(deploy), &dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
		})

		It("should NOT place surge annotations on the deployment", func() {
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())

			var dep appsv1.Deployment
			Expect(applier.client.Get(ctx, keyFor(deploy), &dep)).To(Succeed())
			if dep.Annotations != nil {
				Expect(dep.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
				Expect(dep.Annotations).ToNot(HaveKey(OriginalMinReplicasAnnotationKey))
			}
		})

		It("should be idempotent when called twice with same surge value", func() {
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())
			// Re-read to get fresh resourceVersion
			var fresh kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &fresh)).To(Succeed())
			applier.scaledObject = &fresh

			Expect(applier.ApplySurge(ctx, 2)).To(Succeed()) // should not error
		})

		It("should preserve original-min annotation when re-surging with a different value", func() {
			// First surge: 1 -> 2
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())

			var afterFirst kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &afterFirst)).To(Succeed())
			Expect(afterFirst.Annotations).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "1"))
			Expect(afterFirst.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "2"))

			// Simulate re-surge with a different value (e.g., if controller logic
			// changes in the future to allow re-surging while a surge is active).
			applier.scaledObject = &afterFirst
			Expect(applier.ApplySurge(ctx, 3)).To(Succeed())

			var afterSecond kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &afterSecond)).To(Succeed())
			// The surge annotation should reflect the new surge value
			Expect(afterSecond.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "3"))
			Expect(*afterSecond.Spec.MinReplicaCount).To(Equal(int32(3)))
			// But original-min must still be 1, NOT the intermediate surged value of 2
			Expect(afterSecond.Annotations).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "1"))
		})

		It("should revert to the true original after re-surging with a different value", func() {
			// First surge: 1 -> 2
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())

			// Re-surge: 2 -> 3
			var afterFirst kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &afterFirst)).To(Succeed())
			applier.scaledObject = &afterFirst
			Expect(applier.ApplySurge(ctx, 3)).To(Succeed())

			// Revert — should restore to 1 (the true original), not 2
			var afterSecond kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &afterSecond)).To(Succeed())
			applier.scaledObject = &afterSecond
			Expect(applier.RevertSurge(ctx, 99)).To(Succeed()) // 99 should be overridden by annotation

			var reverted kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &reverted)).To(Succeed())
			Expect(*reverted.Spec.MinReplicaCount).To(Equal(int32(1)))
			Expect(reverted.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
			Expect(reverted.Annotations).ToNot(HaveKey(OriginalMinReplicasAnnotationKey))
		})
	})

	Describe("RevertSurge with fake client", func() {
		var (
			ctx     context.Context
			so      *kedav1alpha1.ScaledObject
			deploy  *appsv1.Deployment
			applier *KEDASurgeApplier
		)

		BeforeEach(func() {
			ctx = context.Background()
			// Start with a surged ScaledObject
			so = createScaledObject("test-so", "default", "test-deploy", 2, 5)
			so.Annotations = map[string]string{
				EvictionSurgeReplicasAnnotationKey: "2",
				OriginalMinReplicasAnnotationKey:   "1",
			}
			deploy = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(2))},
			}
			scheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(deploy, so).
				Build()

			target := &DeploymentWrapper{obj: deploy}
			applier = &KEDASurgeApplier{client: fc, scaledObject: so, target: target}
		})

		It("should restore minReplicaCount from original-min-replicas annotation", func() {
			Expect(applier.RevertSurge(ctx, 99)).To(Succeed()) // 99 should be overridden by annotation

			var updated kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &updated)).To(Succeed())
			Expect(updated.Spec.MinReplicaCount).ToNot(BeNil())
			Expect(*updated.Spec.MinReplicaCount).To(Equal(int32(1))) // from annotation, not 99
		})

		It("should remove both surge annotations after revert", func() {
			Expect(applier.RevertSurge(ctx, 1)).To(Succeed())

			var updated kedav1alpha1.ScaledObject
			Expect(applier.client.Get(ctx, keyFor(so), &updated)).To(Succeed())
			Expect(updated.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
			Expect(updated.Annotations).ToNot(HaveKey(OriginalMinReplicasAnnotationKey))
		})

		It("should fall back to passed-in value when annotation is missing", func() {
			noAnnSO := createScaledObject("no-ann-so", "default", "test-deploy", 3, 5)
			scheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, noAnnSO).Build()

			target := &DeploymentWrapper{obj: deploy}
			noAnnApplier := &KEDASurgeApplier{client: fc, scaledObject: noAnnSO, target: target}

			Expect(noAnnApplier.RevertSurge(ctx, 2)).To(Succeed())

			var updated kedav1alpha1.ScaledObject
			Expect(fc.Get(ctx, keyFor(noAnnSO), &updated)).To(Succeed())
			Expect(updated.Spec.MinReplicaCount).ToNot(BeNil())
			Expect(*updated.Spec.MinReplicaCount).To(Equal(int32(2))) // passed-in fallback
		})
	})
})

func keyFor(obj interface {
	GetName() string
	GetNamespace() string
}) client.ObjectKey {
	return client.ObjectKey{Name: obj.GetName(), Namespace: obj.GetNamespace()}
}
