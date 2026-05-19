package controllers

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("ResolveMinReplicas", func() {
	It("should return 0 when KEDA ScaledObject has nil minReplicaCount (scale-to-zero)", func() {
		ctx := context.Background()

		so := &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "scale-to-zero-so",
				Namespace: defaultNamespace,
			},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{
					Name: "my-deploy",
					Kind: ResourceTypeDeployment,
				},
				// MinReplicaCount intentionally omitted (nil) — KEDA defaults to 0
				MaxReplicaCount: new(int32(10)),
			},
		}

		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
		fc := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(so).
			Build()

		min, hasAutoscaler, err := ResolveMinReplicas(ctx, fc, defaultNamespace, "my-deploy", ResourceTypeDeployment, 3)
		Expect(err).ToNot(HaveOccurred())
		Expect(hasAutoscaler).To(BeTrue())
		Expect(min).To(Equal(int32(0)))
	})

	It("should return the set minReplicaCount when KEDA ScaledObject has it", func() {
		ctx := context.Background()

		so := &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "normal-so",
				Namespace: defaultNamespace,
			},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{
					Name: "my-deploy",
					Kind: ResourceTypeDeployment,
				},
				MinReplicaCount: new(int32(2)),
				MaxReplicaCount: new(int32(10)),
			},
		}

		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
		fc := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(so).
			Build()

		min, hasAutoscaler, err := ResolveMinReplicas(ctx, fc, defaultNamespace, "my-deploy", ResourceTypeDeployment, 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(hasAutoscaler).To(BeTrue())
		Expect(min).To(Equal(int32(2)))
	})
})
