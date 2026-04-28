package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("DeploymentSurgeApplier", func() {
	var (
		namespace string
		ctx       context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespaceObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-surge-",
			},
		}
		Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
		namespace = namespaceObj.Name
	})

	It("should set replicas and add annotation on ApplySurge", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("surge-apply", namespace, "surge-apply", 1, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &DeploymentSurgeApplier{client: k8sClient, target: target}

		err := applier.ApplySurge(ctx, 3)
		Expect(err).ToNot(HaveOccurred())

		// Re-fetch the deployment from the API
		var updated appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(3)))
		Expect(updated.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "3"))
	})

	It("should revert replicas and remove annotation on RevertSurge", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("surge-revert", namespace, "surge-revert", 3, &maxUnavailable)
		dep.Annotations = map[string]string{EvictionSurgeReplicasAnnotationKey: "3"}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &DeploymentSurgeApplier{client: k8sClient, target: target}

		err := applier.RevertSurge(ctx, 1)
		Expect(err).ToNot(HaveOccurred())

		var updated appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(1)))
		Expect(updated.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	})

	It("should return 'deployment' as Name", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("surge-name", namespace, "surge-name", 1, &maxUnavailable)
		target := &DeploymentWrapper{obj: dep}
		applier := &DeploymentSurgeApplier{client: k8sClient, target: target}
		Expect(applier.Name()).To(Equal("deployment"))
	})
})

var _ = Describe("CompositeSurgeApplier", func() {
	var (
		namespace string
		ctx       context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespaceObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-composite-",
			},
		}
		Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
		namespace = namespaceObj.Name
	})

	It("should apply surge through all appliers in order", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("composite-apply", namespace, "composite-apply", 1, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		composite := &CompositeSurgeApplier{
			appliers: []SurgeApplier{&DeploymentSurgeApplier{client: k8sClient, target: target}},
			target:   target,
		}

		err := composite.ApplySurge(ctx, 4)
		Expect(err).ToNot(HaveOccurred())

		var updated appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(4)))
		Expect(updated.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "4"))
	})

	It("should revert surge through all appliers in order", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("composite-revert", namespace, "composite-revert", 4, &maxUnavailable)
		dep.Annotations = map[string]string{EvictionSurgeReplicasAnnotationKey: "4"}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		composite := &CompositeSurgeApplier{
			appliers: []SurgeApplier{&DeploymentSurgeApplier{client: k8sClient, target: target}},
			target:   target,
		}

		err := composite.RevertSurge(ctx, 1)
		Expect(err).ToNot(HaveOccurred())

		var updated appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(1)))
		Expect(updated.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	})

	It("should return composite name with all applier names", func() {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment("composite-name", namespace, "composite-name", 1, &maxUnavailable)
		target := &DeploymentWrapper{obj: dep}
		composite := &CompositeSurgeApplier{
			appliers: []SurgeApplier{&DeploymentSurgeApplier{client: k8sClient, target: target}},
			target:   target,
		}
		Expect(composite.Name()).To(Equal("composite(deployment)"))
	})
})

var _ = Describe("hasTargetAnnotation", func() {
	It("should return false when annotations are nil", func() {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
		target := &DeploymentWrapper{obj: dep}
		Expect(hasTargetAnnotation(target)).To(BeFalse())
	})

	It("should return false when annotation is not present", func() {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Annotations: map[string]string{"other": "value"},
		}}
		target := &DeploymentWrapper{obj: dep}
		Expect(hasTargetAnnotation(target)).To(BeFalse())
	})

	It("should return true when annotation is present", func() {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Annotations: map[string]string{EvictionSurgeReplicasAnnotationKey: "3"},
		}}
		target := &DeploymentWrapper{obj: dep}
		Expect(hasTargetAnnotation(target)).To(BeTrue())
	})
})

var _ = Describe("hasTargetAnnotationWithValue", func() {
	It("should return false when annotations are nil", func() {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
		target := &DeploymentWrapper{obj: dep}
		Expect(hasTargetAnnotationWithValue(target, "3")).To(BeFalse())
	})

	It("should return false when value does not match", func() {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Annotations: map[string]string{EvictionSurgeReplicasAnnotationKey: "2"},
		}}
		target := &DeploymentWrapper{obj: dep}
		Expect(hasTargetAnnotationWithValue(target, "3")).To(BeFalse())
	})

	It("should return true when value matches", func() {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Annotations: map[string]string{EvictionSurgeReplicasAnnotationKey: "3"},
		}}
		target := &DeploymentWrapper{obj: dep}
		Expect(hasTargetAnnotationWithValue(target, "3")).To(BeTrue())
	})
})
