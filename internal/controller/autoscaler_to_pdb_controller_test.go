package controllers

import (
	"context"

	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("AutoscalerToPDBReconciler adopted policy", func() {
	It("preserves maxUnavailable against an HPA minimum-replica change", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-autoscaler-pdb-",
			Annotations: map[string]string{
				namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
			},
		}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		Expect(appsv1.AddToScheme(scheme.Scheme)).To(Succeed())
		Expect(autoscalingv2.AddToScheme(scheme.Scheme)).To(Succeed())
		Expect(policyv1.AddToScheme(scheme.Scheme)).To(Succeed())

		deployment := createDeployment("app", ns.Name, "autoscaler-app", 8, nil)
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		minAvailable := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app",
				Namespace: ns.Name,
				Annotations: map[string]string{
					PDBOwnedByAnnotationKey:             ControllerName,
					OriginalMaxUnavailableAnnotationKey: `"25%"`,
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &minAvailable,
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "autoscaler-app"}},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

		minReplicas := int32(5)
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "app-hpa", Namespace: ns.Name},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       ResourceTypeDeployment,
					Name:       deployment.Name,
				},
				MinReplicas: &minReplicas,
				MaxReplicas: 10,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		r := &AutoscalerToPDBReconciler{
			Client: k8sClient,
			Scheme: scheme.Scheme,
			Filter: &deploymentTestFilter{},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(hpa)})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pdb), pdb)).To(Succeed())
		// ceil(25% of 5) is 2, so the maintained healthy floor is 3.
		Expect(pdb.Spec.MinAvailable).To(Equal(&intstr.IntOrString{Type: intstr.Int, IntVal: 3}))
	})
})
