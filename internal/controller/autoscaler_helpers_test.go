package controllers

import (
	"context"
	"errors"

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
		result, err := calculateSurge(ctx, target, 3)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(5)))
	})

	It("returns errSurgeZero when maxSurge is 0", func() {
		target := makeTarget(intstr.FromInt32(0))
		result, err := calculateSurge(ctx, target, 5)
		Expect(errors.Is(err, errSurgeZero)).To(BeTrue())
		Expect(result).To(Equal(int32(5)))
	})

	It("returns errSurgeZero when no RollingUpdate strategy is set", func() {
		dep := &appsv1.Deployment{}
		target := &DeploymentWrapper{obj: dep}
		result, err := calculateSurge(ctx, target, 4)
		Expect(errors.Is(err, errSurgeZero)).To(BeTrue())
		Expect(result).To(Equal(int32(4)))
	})

	It("computes 25% surge with ceiling (exact)", func() {
		target := makeTarget(intstr.FromString("25%"))
		// 4 * 25% = 1.0 → ceil(1.0) = 1 → 4+1 = 5
		result, err := calculateSurge(ctx, target, 4)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(5)))
	})

	It("computes 25% surge with ceiling (fractional)", func() {
		target := makeTarget(intstr.FromString("25%"))
		// 3 * 25% = 0.75 → ceil(0.75) = 1 → 3+1 = 4
		result, err := calculateSurge(ctx, target, 3)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(4)))
	})

	It("computes 50% surge with ceiling", func() {
		target := makeTarget(intstr.FromString("50%"))
		// 3 * 50% = 1.5 → ceil(1.5) = 2 → 3+2 = 5
		result, err := calculateSurge(ctx, target, 3)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(5)))
	})

	It("computes 100% surge", func() {
		target := makeTarget(intstr.FromString("100%"))
		// 5 * 100% = 5 → ceil(5) = 5 → 5+5 = 10
		result, err := calculateSurge(ctx, target, 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(10)))
	})

	It("returns errInvalidPercentage for invalid percentage string", func() {
		target := makeTarget(intstr.FromString("abc%"))
		_, err := calculateSurge(ctx, target, 3)
		Expect(errors.Is(err, errInvalidPercentage)).To(BeTrue())
	})

	It("returns errInvalidPercentage for a string without a % suffix", func() {
		target := makeTarget(intstr.FromString("10"))
		_, err := calculateSurge(ctx, target, 3)
		Expect(errors.Is(err, errInvalidPercentage)).To(BeTrue())
	})

	It("returns errNegativeSurge for a negative int maxSurge", func() {
		target := makeTarget(intstr.FromInt32(-1))
		_, err := calculateSurge(ctx, target, 3)
		Expect(errors.Is(err, errNegativeSurge)).To(BeTrue())
	})

	It("returns errNegativeSurge for a negative percentage", func() {
		target := makeTarget(intstr.FromString("-10%"))
		_, err := calculateSurge(ctx, target, 3)
		Expect(errors.Is(err, errNegativeSurge)).To(BeTrue())
	})

	It("returns errSurgeZero for 0% string", func() {
		target := makeTarget(intstr.FromString("0%"))
		result, err := calculateSurge(ctx, target, 3)
		Expect(errors.Is(err, errSurgeZero)).To(BeTrue())
		Expect(result).To(Equal(int32(3)))
	})

	// --- surge-override annotation ---

	makeTargetAnn := func(surge intstr.IntOrString, override string) Surger {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{SurgeOverrideAnnotationKey: override},
			},
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge},
				},
			},
		}
		return &DeploymentWrapper{obj: dep}
	}

	It("uses an integer surge-override even when maxSurge is 0", func() {
		target := makeTargetAnn(intstr.FromInt32(0), "2")
		result, err := calculateSurge(ctx, target, 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(7)))
	})

	It("surge-override wins over a non-zero maxSurge", func() {
		target := makeTargetAnn(intstr.FromInt32(5), "2")
		result, err := calculateSurge(ctx, target, 3)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(5))) // 3 + override(2), NOT 3 + maxSurge(5)
	})

	It("supports a percentage surge-override", func() {
		target := makeTargetAnn(intstr.FromInt32(0), "10%")
		// 10 * 10% = 1.0 → ceil = 1 → 10 + 1 = 11
		result, err := calculateSurge(ctx, target, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(11)))
	})

	It("applies the override even with no RollingUpdate strategy", func() {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{SurgeOverrideAnnotationKey: "3"},
			},
		}
		target := &DeploymentWrapper{obj: dep}
		result, err := calculateSurge(ctx, target, 4)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(int32(7)))
	})

	It("returns errSurgeZero for a zero surge-override", func() {
		target := makeTargetAnn(intstr.FromInt32(2), "0")
		result, err := calculateSurge(ctx, target, 3)
		Expect(errors.Is(err, errSurgeZero)).To(BeTrue())
		Expect(result).To(Equal(int32(3)))
	})

	It("returns errNegativeSurge for a negative surge-override", func() {
		target := makeTargetAnn(intstr.FromInt32(2), "-1")
		_, err := calculateSurge(ctx, target, 3)
		Expect(errors.Is(err, errNegativeSurge)).To(BeTrue())
	})

	It("returns errInvalidPercentage for an unparseable surge-override", func() {
		target := makeTargetAnn(intstr.FromInt32(2), "abc")
		_, err := calculateSurge(ctx, target, 3)
		Expect(errors.Is(err, errInvalidPercentage)).To(BeTrue())
	})
})
