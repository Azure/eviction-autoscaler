package controllers

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var _ = Describe("deploymentReplicas", func() {
	It("uses the Kubernetes default when replicas is omitted", func() {
		Expect(deploymentReplicas(&appsv1.Deployment{})).To(Equal(int32(1)))
	})

	It("preserves an explicit zero", func() {
		replicas := int32(0)
		deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: &replicas}}
		Expect(deploymentReplicas(deployment)).To(Equal(int32(0)))
	})
})

var _ = Describe("DeploymentWrapper.GetMaxSurge", func() {
	It("returns the explicit maxSurge when RollingUpdate strategy is fully specified", func() {
		surge := intstr.FromInt32(5)
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					Type: appsv1.RollingUpdateDeploymentStrategyType,
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge: &surge,
					},
				},
			},
		}
		w := &DeploymentWrapper{obj: dep}
		Expect(w.GetMaxSurge()).To(Equal(intstr.FromInt32(5)))
	})

	It("returns the explicit percentage maxSurge", func() {
		surge := intstr.FromString("50%")
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					Type: appsv1.RollingUpdateDeploymentStrategyType,
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge: &surge,
					},
				},
			},
		}
		w := &DeploymentWrapper{obj: dep}
		Expect(w.GetMaxSurge()).To(Equal(intstr.FromString("50%")))
	})

	It("returns explicit maxSurge 0 when set to 0", func() {
		surge := intstr.FromInt32(0)
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					Type: appsv1.RollingUpdateDeploymentStrategyType,
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxSurge: &surge,
					},
				},
			},
		}
		w := &DeploymentWrapper{obj: dep}
		Expect(w.GetMaxSurge()).To(Equal(intstr.FromInt32(0)))
	})

	It("returns 0 for a Recreate strategy", func() {
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					Type: appsv1.RecreateDeploymentStrategyType,
				},
			},
		}
		w := &DeploymentWrapper{obj: dep}
		Expect(w.GetMaxSurge()).To(Equal(intstr.FromInt(0)))
	})

	It("returns the Kubernetes default 25% when RollingUpdate is set but maxSurge is nil", func() {
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					Type:          appsv1.RollingUpdateDeploymentStrategyType,
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						// MaxSurge intentionally nil
					},
				},
			},
		}
		w := &DeploymentWrapper{obj: dep}
		Expect(w.GetMaxSurge()).To(Equal(intstr.FromString("25%")))
	})

	It("returns the Kubernetes default 25% when RollingUpdate type is set but rollingUpdate field is nil", func() {
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Strategy: appsv1.DeploymentStrategy{
					Type: appsv1.RollingUpdateDeploymentStrategyType,
					// RollingUpdate field nil
				},
			},
		}
		w := &DeploymentWrapper{obj: dep}
		Expect(w.GetMaxSurge()).To(Equal(intstr.FromString("25%")))
	})

	It("returns the Kubernetes default 25% for a bare Deployment (completely unset strategy)", func() {
		dep := &appsv1.Deployment{}
		w := &DeploymentWrapper{obj: dep}
		Expect(w.GetMaxSurge()).To(Equal(intstr.FromString("25%")))
	})
})

var _ = Describe("StatefulSetWrapper.GetMaxSurge", func() {
	It("always returns 10%", func() {
		ss := &appsv1.StatefulSet{}
		w := &StatefulSetWrapper{obj: ss}
		Expect(w.GetMaxSurge()).To(Equal(intstr.FromString("10%")))
	})
})
