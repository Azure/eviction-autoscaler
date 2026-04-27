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
		dep := createTestDeployment("surge-apply", namespace, 1)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &DeploymentSurgeApplier{target: target}

		err := applier.ApplySurge(ctx, k8sClient, 3)
		Expect(err).ToNot(HaveOccurred())

		// Re-fetch the deployment from the API
		var updated appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(3)))
		Expect(updated.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "3"))
	})

	It("should revert replicas and remove annotation on RevertSurge", func() {
		dep := createTestDeployment("surge-revert", namespace, 1)
		dep.Annotations = map[string]string{EvictionSurgeReplicasAnnotationKey: "3"}
		replicas := int32(3)
		dep.Spec.Replicas = &replicas
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		applier := &DeploymentSurgeApplier{target: target}

		err := applier.RevertSurge(ctx, k8sClient, 1)
		Expect(err).ToNot(HaveOccurred())

		var updated appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(1)))
		Expect(updated.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	})

	It("should return 'deployment' as Name", func() {
		dep := createTestDeployment("surge-name", namespace, 1)
		target := &DeploymentWrapper{obj: dep}
		applier := &DeploymentSurgeApplier{target: target}
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
		dep := createTestDeployment("composite-apply", namespace, 1)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		composite := &CompositeSurgeApplier{
			appliers: []SurgeApplier{&DeploymentSurgeApplier{target: target}},
			target:   target,
		}

		err := composite.ApplySurge(ctx, k8sClient, 4)
		Expect(err).ToNot(HaveOccurred())

		var updated appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(4)))
		Expect(updated.Annotations).To(HaveKeyWithValue(EvictionSurgeReplicasAnnotationKey, "4"))
	})

	It("should revert surge through all appliers in order", func() {
		dep := createTestDeployment("composite-revert", namespace, 1)
		replicas := int32(4)
		dep.Spec.Replicas = &replicas
		dep.Annotations = map[string]string{EvictionSurgeReplicasAnnotationKey: "4"}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		target := &DeploymentWrapper{obj: dep}
		composite := &CompositeSurgeApplier{
			appliers: []SurgeApplier{&DeploymentSurgeApplier{target: target}},
			target:   target,
		}

		err := composite.RevertSurge(ctx, k8sClient, 1)
		Expect(err).ToNot(HaveOccurred())

		var updated appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &updated)).To(Succeed())
		Expect(*updated.Spec.Replicas).To(Equal(int32(1)))
		Expect(updated.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	})

	It("should return composite name with all applier names", func() {
		dep := createTestDeployment("composite-name", namespace, 1)
		target := &DeploymentWrapper{obj: dep}
		composite := &CompositeSurgeApplier{
			appliers: []SurgeApplier{&DeploymentSurgeApplier{target: target}},
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

// createTestDeployment creates a minimal deployment for testing
func createTestDeployment(name, namespace string, replicas int32) *appsv1.Deployment {
	maxUnavailable := intstr.FromInt(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "nginx", Image: "nginx:latest"},
					},
				},
			},
		},
	}
}
