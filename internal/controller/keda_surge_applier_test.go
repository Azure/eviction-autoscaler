package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// createScaledObject creates an unstructured KEDA ScaledObject for testing.
//
//nolint:unparam // test helper — parameters vary across test suites, not just this file
func createScaledObject(name, namespace, targetDeployment string, minReplicaCount, maxReplicaCount int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "keda.sh",
		Version: "v1alpha1",
		Kind:    "ScaledObject",
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)
	_ = unstructured.SetNestedField(obj.Object, targetDeployment, "spec", "scaleTargetRef", "name")
	_ = unstructured.SetNestedField(obj.Object, "Deployment", "spec", "scaleTargetRef", "kind")
	_ = unstructured.SetNestedField(obj.Object, minReplicaCount, "spec", "minReplicaCount")
	_ = unstructured.SetNestedField(obj.Object, maxReplicaCount, "spec", "maxReplicaCount")
	return obj
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
			so      *unstructured.Unstructured
			deploy  *appsv1.Deployment
			applier *KEDASurgeApplier
		)

		BeforeEach(func() {
			ctx = context.Background()
			so = createScaledObject("test-so", "default", "test-deploy", 1, 5)
			deploy = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
			}
			scheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(deploy).
				WithStatusSubresource(deploy).
				Build()
			// Register ScaledObject in the fake client by creating it
			Expect(fc.Create(ctx, so)).To(Succeed())

			target := &DeploymentWrapper{obj: deploy}
			applier = &KEDASurgeApplier{client: fc, scaledObject: so, target: target}
		})

		It("should set evictionSurgeReplicas and original-min-replicas on ScaledObject", func() {
			Expect(applier.ApplySurge(ctx, 2)).To(Succeed())

			// Re-read ScaledObject from fake client
			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(so.GroupVersionKind())
			Expect(applier.client.Get(ctx, keyFor(so), updated)).To(Succeed())

			Expect(updated.GetAnnotations()).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "2"))
			Expect(updated.GetAnnotations()).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "1"))

			val, found, _ := unstructured.NestedInt64(updated.Object, "spec", "minReplicaCount")
			Expect(found).To(BeTrue())
			Expect(val).To(Equal(int64(2)))
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
			fresh := &unstructured.Unstructured{}
			fresh.SetGroupVersionKind(so.GroupVersionKind())
			Expect(applier.client.Get(ctx, keyFor(so), fresh)).To(Succeed())
			applier.scaledObject = fresh

			Expect(applier.ApplySurge(ctx, 2)).To(Succeed()) // should not error
		})

		It("should skip surge when minReplicaCount already above surge value", func() {
			// ScaledObject with minReplicaCount=5, surge to 2 should be skipped
			highSO := createScaledObject("high-so", "default", "test-deploy", 5, 10)
			scheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
			Expect(fc.Create(ctx, highSO)).To(Succeed())

			target := &DeploymentWrapper{obj: deploy}
			highApplier := &KEDASurgeApplier{client: fc, scaledObject: highSO, target: target}

			Expect(highApplier.ApplySurge(ctx, 2)).To(Succeed())

			// ScaledObject should NOT have surge annotations (skipped)
			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(highSO.GroupVersionKind())
			Expect(fc.Get(ctx, keyFor(highSO), updated)).To(Succeed())
			Expect(updated.GetAnnotations()).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
		})
	})

	Describe("RevertSurge with fake client", func() {
		var (
			ctx     context.Context
			so      *unstructured.Unstructured
			deploy  *appsv1.Deployment
			applier *KEDASurgeApplier
		)

		BeforeEach(func() {
			ctx = context.Background()
			// Start with a surged ScaledObject
			so = createScaledObject("test-so", "default", "test-deploy", 2, 5)
			so.SetAnnotations(map[string]string{
				EvictionSurgeReplicasAnnotationKey: "2",
				OriginalMinReplicasAnnotationKey:   "1",
			})
			deploy = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))},
			}
			scheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(deploy).
				Build()
			Expect(fc.Create(ctx, so)).To(Succeed())

			target := &DeploymentWrapper{obj: deploy}
			applier = &KEDASurgeApplier{client: fc, scaledObject: so, target: target}
		})

		It("should restore minReplicaCount from original-min-replicas annotation", func() {
			Expect(applier.RevertSurge(ctx, 99)).To(Succeed()) // 99 should be overridden by annotation

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(so.GroupVersionKind())
			Expect(applier.client.Get(ctx, keyFor(so), updated)).To(Succeed())

			val, found, _ := unstructured.NestedInt64(updated.Object, "spec", "minReplicaCount")
			Expect(found).To(BeTrue())
			Expect(val).To(Equal(int64(1))) // from annotation, not 99
		})

		It("should remove both surge annotations after revert", func() {
			Expect(applier.RevertSurge(ctx, 1)).To(Succeed())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(so.GroupVersionKind())
			Expect(applier.client.Get(ctx, keyFor(so), updated)).To(Succeed())

			Expect(updated.GetAnnotations()).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
			Expect(updated.GetAnnotations()).ToNot(HaveKey(OriginalMinReplicasAnnotationKey))
		})

		It("should fall back to passed-in value when annotation is missing", func() {
			// Remove annotations before revert
			noAnnSO := createScaledObject("no-ann-so", "default", "test-deploy", 3, 5)
			scheme := runtime.NewScheme()
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
			Expect(fc.Create(ctx, noAnnSO)).To(Succeed())

			target := &DeploymentWrapper{obj: deploy}
			noAnnApplier := &KEDASurgeApplier{client: fc, scaledObject: noAnnSO, target: target}

			Expect(noAnnApplier.RevertSurge(ctx, 2)).To(Succeed())

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(noAnnSO.GroupVersionKind())
			Expect(fc.Get(ctx, keyFor(noAnnSO), updated)).To(Succeed())

			val, found, _ := unstructured.NestedInt64(updated.Object, "spec", "minReplicaCount")
			Expect(found).To(BeTrue())
			Expect(val).To(Equal(int64(2))) // passed-in fallback
		})
	})
})

func keyFor(obj interface{ GetName() string; GetNamespace() string }) client.ObjectKey {
	return client.ObjectKey{Name: obj.GetName(), Namespace: obj.GetNamespace()}
}
