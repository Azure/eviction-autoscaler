package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var _ = Describe("calculateSurge", func() {
	ctx := context.Background()

	It("should add integer maxSurge to minReplicas", func() {
		surge := intstr.FromInt(1)
		maxUnavailable := intstr.FromInt(0)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge:       &surge,
						MaxUnavailable: &maxUnavailable,
					},
				},
			},
		}
		target := &DeploymentWrapper{obj: dep}
		result, err := calculateSurge(ctx, target, 2)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal(int32(3))) // 2 + 1
	})

	It("should add percentage maxSurge with ceiling", func() {
		surge := intstr.FromString("25%")
		maxUnavailable := intstr.FromInt(0)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge:       &surge,
						MaxUnavailable: &maxUnavailable,
					},
				},
			},
		}
		target := &DeploymentWrapper{obj: dep}
		result, err := calculateSurge(ctx, target, 3)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal(int32(4))) // 3 + ceil(3*0.25) = 3 + 1
	})

	It("should round up percentage surge", func() {
		surge := intstr.FromString("50%")
		maxUnavailable := intstr.FromInt(0)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge:       &surge,
						MaxUnavailable: &maxUnavailable,
					},
				},
			},
		}
		target := &DeploymentWrapper{obj: dep}
		result, err := calculateSurge(ctx, target, 1)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal(int32(2))) // 1 + ceil(1*0.5) = 1 + 1
	})

	It("should return error for invalid percentage string", func() {
		surge := intstr.FromString("abc%")
		maxUnavailable := intstr.FromInt(0)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge:       &surge,
						MaxUnavailable: &maxUnavailable,
					},
				},
			},
		}
		target := &DeploymentWrapper{obj: dep}
		_, err := calculateSurge(ctx, target, 2)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid surge percentage"))
	})

	It("should return 0 surge when maxSurge is 0", func() {
		surge := intstr.FromInt(0)
		maxUnavailable := intstr.FromInt(0)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge:       &surge,
						MaxUnavailable: &maxUnavailable,
					},
				},
			},
		}
		target := &DeploymentWrapper{obj: dep}
		result, err := calculateSurge(ctx, target, 2)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal(int32(2))) // 2 + 0
	})

	It("should return minReplicas when strategy is Recreate (no maxSurge)", func() {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					Type: appsv1.RecreateDeploymentStrategyType,
				},
			},
		}
		target := &DeploymentWrapper{obj: dep}
		result, err := calculateSurge(ctx, target, 2)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal(int32(2))) // No surge, maxSurge defaults to 0
	})
})
