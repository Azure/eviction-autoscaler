package controllers

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// forbiddenOnUpdateClient wraps a fake client so every Update returns a Forbidden error.
// This simulates Deployment Safeguards (DS) rejecting a surge write in an AKS-owned
// namespace on an Automatic cluster.
func forbiddenOnUpdateClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	Expect(autoscalingv2.AddToScheme(scheme)).To(Succeed())
	Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "apps", Resource: "deployments"},
					obj.GetName(),
					nil,
				)
			}
			return c.Update(ctx, obj, opts...)
		},
	})
}

// When Deployment Safeguards blocks the surge write, each surge strategy must return the
// error (so the reconcile loop requeues) and must not panic. A panic here would fail the
// spec because Ginkgo treats panics as failures.
var _ = Describe("Surge appliers when Deployment Safeguards blocks the write", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("DeploymentSurgeApplier returns a forbidden error and does not panic", func() {
		dep := createDeployment("ds-dep", "kube-system", "ds-dep", 1, nil)
		applier := &DeploymentSurgeApplier{
			client: forbiddenOnUpdateClient(dep),
			target: &DeploymentWrapper{obj: dep},
		}
		err := applier.ApplySurge(ctx, 3)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue())
	})

	It("HPASurgeApplier returns a forbidden error and does not panic", func() {
		dep := createDeployment("ds-hpa-dep", "kube-system", "ds-hpa-dep", 1, nil)
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "ds-hpa", Namespace: "kube-system"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				MinReplicas: ptr.To(int32(1)),
				MaxReplicas: 5,
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: dep.Name,
				},
			},
		}
		applier := &HPASurgeApplier{
			client: forbiddenOnUpdateClient(dep, hpa),
			hpa:    hpa,
			target: &DeploymentWrapper{obj: dep},
		}
		err := applier.ApplySurge(ctx, 3)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue())
	})

	It("KEDASurgeApplier returns a forbidden error and does not panic", func() {
		dep := createDeployment("ds-keda-dep", "kube-system", "ds-keda-dep", 1, nil)
		so := createScaledObject("ds-so", "kube-system", "ds-keda-dep", 1, 5)
		applier := &KEDASurgeApplier{
			client:       forbiddenOnUpdateClient(dep, so),
			scaledObject: so,
			target:       &DeploymentWrapper{obj: dep},
		}
		err := applier.ApplySurge(ctx, 3)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue())
	})
})
