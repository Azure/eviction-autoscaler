package controllers

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("getMaxSurgeAttempts", func() {
	makeTarget := func(annotations map[string]string) Surger {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
		}
		return &DeploymentWrapper{obj: dep}
	}

	It("returns the default when no annotation is set", func() {
		target := makeTarget(nil)
		Expect(getMaxSurgeAttempts(target)).To(Equal(int32(defaultTimeToReadyMinutes)))
	})

	It("returns the value from the time-to-ready annotation", func() {
		target := makeTarget(map[string]string{TimeToReadyAnnotationKey: "10"})
		Expect(getMaxSurgeAttempts(target)).To(Equal(int32(10)))
	})

	It("returns 1 from a time-to-ready annotation of 1", func() {
		target := makeTarget(map[string]string{TimeToReadyAnnotationKey: "1"})
		Expect(getMaxSurgeAttempts(target)).To(Equal(int32(1)))
	})

	It("returns the default when the annotation value is not a number", func() {
		target := makeTarget(map[string]string{TimeToReadyAnnotationKey: "invalid"})
		Expect(getMaxSurgeAttempts(target)).To(Equal(int32(defaultTimeToReadyMinutes)))
	})

	It("returns the default when the annotation value is zero", func() {
		target := makeTarget(map[string]string{TimeToReadyAnnotationKey: "0"})
		Expect(getMaxSurgeAttempts(target)).To(Equal(int32(defaultTimeToReadyMinutes)))
	})

	It("returns the default when the annotation value is negative", func() {
		target := makeTarget(map[string]string{TimeToReadyAnnotationKey: "-5"})
		Expect(getMaxSurgeAttempts(target)).To(Equal(int32(defaultTimeToReadyMinutes)))
	})
})

var _ = Describe("validateTimeToReadyAnnotation", func() {
	makeTarget := func(annotations map[string]string) Surger {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
		}
		return &DeploymentWrapper{obj: dep}
	}

	It("returns nil when annotation is absent", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(nil))).To(Succeed())
	})

	It("returns nil when annotation is absent from a non-nil map", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(map[string]string{"other": "val"}))).To(Succeed())
	})

	It("returns nil for minimum valid value 1", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(map[string]string{TimeToReadyAnnotationKey: "1"}))).To(Succeed())
	})

	It("returns nil for maximum valid value 10", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(map[string]string{TimeToReadyAnnotationKey: "10"}))).To(Succeed())
	})

	It("returns nil for a value in the middle of the range", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(map[string]string{TimeToReadyAnnotationKey: "5"}))).To(Succeed())
	})

	It("returns an error for value 0", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(map[string]string{TimeToReadyAnnotationKey: "0"}))).To(HaveOccurred())
	})

	It("returns an error for value 11 (above maximum)", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(map[string]string{TimeToReadyAnnotationKey: "11"}))).To(HaveOccurred())
	})

	It("returns an error for a negative value", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(map[string]string{TimeToReadyAnnotationKey: "-1"}))).To(HaveOccurred())
	})

	It("returns an error for a non-numeric value", func() {
		Expect(validateTimeToReadyAnnotation(makeTarget(map[string]string{TimeToReadyAnnotationKey: "fast"}))).To(HaveOccurred())
	})

	It("error message names the annotation key and the bad value", func() {
		err := validateTimeToReadyAnnotation(makeTarget(map[string]string{TimeToReadyAnnotationKey: "99"}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(TimeToReadyAnnotationKey))
		Expect(err.Error()).To(ContainSubstring("99"))
	})
})

var _ = Describe("GetReadyReplicas", func() {
	It("returns ReadyReplicas from deployment status", func() {
		dep := &appsv1.Deployment{
			Status: appsv1.DeploymentStatus{ReadyReplicas: 3},
		}
		Expect((&DeploymentWrapper{obj: dep}).GetReadyReplicas()).To(Equal(int32(3)))
	})

	It("returns 0 when no pods are ready", func() {
		dep := &appsv1.Deployment{}
		Expect((&DeploymentWrapper{obj: dep}).GetReadyReplicas()).To(Equal(int32(0)))
	})
})

var _ = Describe("ResolveMinReplicas", func() {
	It("should return 0 when KEDA ScaledObject has nil minReplicaCount (scale-to-zero)", func() {
		ctx := context.Background()

		so := &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "scale-to-zero-so",
				Namespace: "default",
			},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{
					Name: "my-deploy",
					Kind: "Deployment",
				},
				// MinReplicaCount intentionally omitted (nil) — KEDA defaults to 0
				MaxReplicaCount: ptr.To(int32(10)),
			},
		}

		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
		fc := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(so).
			Build()

		min, hasAutoscaler, err := ResolveMinReplicas(ctx, fc, "default", "my-deploy", "Deployment", 3)
		Expect(err).ToNot(HaveOccurred())
		Expect(hasAutoscaler).To(BeTrue())
		Expect(min).To(Equal(int32(0)))
	})

	It("should return the set minReplicaCount when KEDA ScaledObject has it", func() {
		ctx := context.Background()

		so := &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "normal-so",
				Namespace: "default",
			},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{
					Name: "my-deploy",
					Kind: "Deployment",
				},
				MinReplicaCount: ptr.To(int32(2)),
				MaxReplicaCount: ptr.To(int32(10)),
			},
		}

		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
		fc := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(so).
			Build()

		min, hasAutoscaler, err := ResolveMinReplicas(ctx, fc, "default", "my-deploy", "Deployment", 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(hasAutoscaler).To(BeTrue())
		Expect(min).To(Equal(int32(2)))
	})
})

var _ = Describe("calculateSurge", func() {
	ctx := context.Background()

	makeTarget := func(surge intstr.IntOrString) Surger {
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge: &surge,
					},
				},
			},
		}
		return &DeploymentWrapper{obj: dep}
	}

	It("adds an integer maxSurge to minReplicas", func() {
		target := makeTarget(intstr.FromInt32(2))
		Expect(calculateSurge(ctx, target, 3)).To(Equal(int32(5)))
	})

	It("returns minReplicas when maxSurge is 0", func() {
		target := makeTarget(intstr.FromInt32(0))
		Expect(calculateSurge(ctx, target, 5)).To(Equal(int32(5)))
	})

	It("returns minReplicas when no RollingUpdate strategy is set", func() {
		dep := &appsv1.Deployment{}
		target := &DeploymentWrapper{obj: dep}
		Expect(calculateSurge(ctx, target, 4)).To(Equal(int32(4)))
	})

	It("computes 25% surge with ceiling (exact)", func() {
		target := makeTarget(intstr.FromString("25%"))
		// 4 * 25% = 1.0 → ceil(1.0) = 1 → 4+1 = 5
		Expect(calculateSurge(ctx, target, 4)).To(Equal(int32(5)))
	})

	It("computes 25% surge with ceiling (fractional)", func() {
		target := makeTarget(intstr.FromString("25%"))
		// 3 * 25% = 0.75 → ceil(0.75) = 1 → 3+1 = 4
		Expect(calculateSurge(ctx, target, 3)).To(Equal(int32(4)))
	})

	It("computes 50% surge with ceiling", func() {
		target := makeTarget(intstr.FromString("50%"))
		// 3 * 50% = 1.5 → ceil(1.5) = 2 → 3+2 = 5
		Expect(calculateSurge(ctx, target, 3)).To(Equal(int32(5)))
	})

	It("computes 100% surge", func() {
		target := makeTarget(intstr.FromString("100%"))
		// 5 * 100% = 5 → ceil(5) = 5 → 5+5 = 10
		Expect(calculateSurge(ctx, target, 5)).To(Equal(int32(10)))
	})
})
