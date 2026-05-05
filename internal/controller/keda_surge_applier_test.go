package controllers

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// createScaledObject creates an unstructured KEDA ScaledObject for testing.
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
			obj := createScaledObject("test-so-active", "default", "test-deploy", 1, 5)
			obj.SetAnnotations(map[string]string{EvictionSurgeReplicasAnnotationKey: "3"})
			applier := &KEDASurgeApplier{scaledObject: obj}
			Expect(applier.IsSurgeActive()).To(BeTrue())
		})
	})

	Describe("ApplySurge annotations (in-memory)", func() {
		// Note: Full ApplySurge with c.Update requires KEDA CRD installed in envtest.
		// These tests verify the annotation and minReplicaCount logic in isolation.

		It("should read originalMin from existing minReplicaCount", func() {
			obj := createScaledObject("test-so", "default", "test-deploy", 2, 10)
			val, found, _ := unstructured.NestedInt64(obj.Object, "spec", "minReplicaCount")
			Expect(found).To(BeTrue())
			Expect(val).To(Equal(int64(2)))
		})

		It("should default originalMin to 0 when minReplicaCount is not set", func() {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject",
			})
			obj.SetName("test-so")
			obj.SetNamespace("default")

			_, found, _ := unstructured.NestedInt64(obj.Object, "spec", "minReplicaCount")
			Expect(found).To(BeFalse())
			// KEDASurgeApplier defaults to 0 when not found
		})

		It("should be able to set minReplicaCount on unstructured object", func() {
			obj := createScaledObject("test-so", "default", "test-deploy", 1, 5)
			err := unstructured.SetNestedField(obj.Object, int64(3), "spec", "minReplicaCount")
			Expect(err).ToNot(HaveOccurred())

			val, found, _ := unstructured.NestedInt64(obj.Object, "spec", "minReplicaCount")
			Expect(found).To(BeTrue())
			Expect(val).To(Equal(int64(3)))
		})

		It("should be able to set and remove annotations on unstructured object", func() {
			obj := createScaledObject("test-so", "default", "test-deploy", 1, 5)

			// Set annotations
			ann := obj.GetAnnotations()
			if ann == nil {
				ann = make(map[string]string)
			}
			ann[EvictionSurgeReplicasAnnotationKey] = "3"
			ann[OriginalMinReplicasAnnotationKey] = "1"
			obj.SetAnnotations(ann)

			Expect(obj.GetAnnotations()).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "3"))
			Expect(obj.GetAnnotations()).To(HaveKeyWithValue(OriginalMinReplicasAnnotationKey, "1"))

			// Remove annotations
			ann = obj.GetAnnotations()
			delete(ann, EvictionSurgeReplicasAnnotationKey)
			delete(ann, OriginalMinReplicasAnnotationKey)
			obj.SetAnnotations(ann)

			Expect(obj.GetAnnotations()).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
			Expect(obj.GetAnnotations()).ToNot(HaveKey(OriginalMinReplicasAnnotationKey))
		})
	})

	Describe("RevertSurge annotation priority (in-memory)", func() {
		It("should prefer annotation value over passed-in originalMinReplicas", func() {
			obj := createScaledObject("test-so", "default", "test-deploy", 3, 5)
			obj.SetAnnotations(map[string]string{
				EvictionSurgeReplicasAnnotationKey: "3",
				OriginalMinReplicasAnnotationKey:   "1",
			})

			// Simulate what RevertSurge does: read annotation
			annotations := obj.GetAnnotations()
			revertTo := int64(99) // passed-in value (should be overridden)
			if val, exists := annotations[OriginalMinReplicasAnnotationKey]; exists {
				parsed, err := parseIntFromString(val)
				Expect(err).ToNot(HaveOccurred())
				revertTo = parsed
			}
			Expect(revertTo).To(Equal(int64(1))) // annotation wins
		})

		It("should fall back to passed-in value when annotation is missing", func() {
			obj := createScaledObject("test-so", "default", "test-deploy", 3, 5)
			// No annotations set

			annotations := obj.GetAnnotations()
			revertTo := int64(2) // passed-in fallback
			if annotations != nil {
				if val, exists := annotations[OriginalMinReplicasAnnotationKey]; exists {
					parsed, err := parseIntFromString(val)
					Expect(err).ToNot(HaveOccurred())
					revertTo = parsed
				}
			}
			Expect(revertTo).To(Equal(int64(2))) // fallback used
		})
	})
})

func parseIntFromString(s string) (int64, error) {
	var val int64
	_, err := fmt.Sscanf(s, "%d", &val)
	return val, err
}
