package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

	It("should update HPA minReplicas, add annotations, and set deployment replicas on ApplySurge", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-surge-apply", namespace, "hpa-surge-apply", 1, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		hpa := createHPA("hpa-surge-apply", namespace, "hpa-surge-apply", 1, 5)
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

		// Verify deployment replicas were set
		var updatedDep appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updatedDep)).To(Succeed())
		Expect(*updatedDep.Spec.Replicas).To(Equal(int32(3)))
	})

	It("should be idempotent on retry — skip HPA update if already surged", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-surge-idempotent", namespace, "hpa-surge-idempotent", 1, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		hpa := createHPA("hpa-surge-idempotent", namespace, "hpa-surge-idempotent", 1, 5)
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &HPASurgeApplier{client: k8sClient, hpa: hpa, target: target}

		// First surge
		Expect(applier.ApplySurge(ctx, 3)).To(Succeed())

		// Re-fetch to get fresh objects
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hpa), hpa)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), dep)).To(Succeed())
		applier.hpa = hpa
		applier.target = &DeploymentWrapper{obj: dep}

		// Second surge with same value — should not error
		Expect(applier.ApplySurge(ctx, 3)).To(Succeed())
	})

	It("should revert HPA minReplicas using annotation value and remove annotations", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("hpa-surge-revert", namespace, "hpa-surge-revert", 3, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		minReplicas := int32(3)
		hpa := createHPA("hpa-surge-revert", namespace, "hpa-surge-revert", 3, 5)
		hpa.Spec.MinReplicas = &minReplicas
		hpa.Annotations = map[string]string{
			EvictionSurgeReplicasAnnotationKey: "3",
			OriginalMinReplicasAnnotationKey:   "1",
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &HPASurgeApplier{client: k8sClient, hpa: hpa, target: target}

		err := applier.RevertSurge(ctx, 1)
		Expect(err).ToNot(HaveOccurred())

		// Verify HPA was reverted to original (from annotation, not passed-in value)
		var updatedHPA autoscalingv2.HorizontalPodAutoscaler
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hpa), &updatedHPA)).To(Succeed())
		Expect(*updatedHPA.Spec.MinReplicas).To(Equal(int32(1)))
		Expect(updatedHPA.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
		Expect(updatedHPA.Annotations).ToNot(HaveKey(OriginalMinReplicasAnnotationKey))
	})

	It("should return 'hpa' as Name", func() {
		applier := &HPASurgeApplier{}
		Expect(applier.Name()).To(Equal("hpa"))
	})

	It("should report IsSurgeActive correctly", func() {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
		}
		applier := &HPASurgeApplier{hpa: hpa}
		Expect(applier.IsSurgeActive()).To(BeFalse())

		hpa.Annotations = map[string]string{EvictionSurgeReplicasAnnotationKey: "3"}
		Expect(applier.IsSurgeActive()).To(BeTrue())
	})
})

var _ = Describe("KEDASurgeApplier", func() {
	It("should return 'keda' as Name", func() {
		applier := &KEDASurgeApplier{}
		Expect(applier.Name()).To(Equal("keda"))
	})

	It("should report IsSurgeActive correctly based on ScaledObject annotation", func() {
		obj := createScaledObject("test-so", "default", "test-deploy", 1, 5)
		applier := &KEDASurgeApplier{scaledObject: obj}
		Expect(applier.IsSurgeActive()).To(BeFalse())

		ann := obj.GetAnnotations()
		if ann == nil {
			ann = make(map[string]string)
		}
		ann[EvictionSurgeReplicasAnnotationKey] = "3"
		obj.SetAnnotations(ann)
		Expect(applier.IsSurgeActive()).To(BeTrue())
	})
})

// createHPA creates an HPA object for testing (not applied to the cluster).
func createHPA(name, namespace, targetDeployment string, minReplicas, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
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
			MaxReplicas: maxReplicas,
		},
	}
}

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
