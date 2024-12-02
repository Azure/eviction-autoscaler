package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1" // Import corev1 package
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
)

var _ = Describe("PDBWatcher Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const namespace = "default"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: namespace}

		BeforeEach(func() {
			By("creating the custom resource for the Kind PDBWatcher")
			pdbwatcher := &v1.PDBWatcher{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.PDBWatcherSpec{
					TargetName: "example-deployment",
					TargetKind: "deployment",
				},
			}
			err := k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, pdbwatcher)).To(Succeed())
			}

			By("creating a Deployment resource")
			surge := intstr.FromInt(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example-deployment",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "example",
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxSurge: &surge,
						},
					},
					Template: corev1.PodTemplateSpec{ // Use corev1.PodTemplateSpec
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "example",
							},
						},
						Spec: corev1.PodSpec{ // Use corev1.PodSpec
							Containers: []corev1.Container{ // Use corev1.Container
								{
									Name:  "nginx",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("creating a PDB resource")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{
						IntVal: 1,
					},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "example",
						},
					},
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 0,
				},
			}
			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		})

		AfterEach(func() {
			By("cleaning up resources")
			deleteResource := func(obj client.Object) {
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
				Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
					return errors.IsNotFound(err)
				}, time.Second*10, time.Millisecond*250).Should(BeTrue())
			}

			deleteResource(&v1.PDBWatcher{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace}})
			deleteResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "example-deployment", Namespace: namespace}})
			deleteResource(&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace}})
		})

		It("should successfully reconcile the resource", func() {
			By("reconciling the created resource")
			controllerReconciler := &PDBWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify PDBWatcher resource
			pdbwatcher := &v1.PDBWatcher{}
			err = k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			Expect(err).NotTo(HaveOccurred())
			Expect(pdbwatcher.Status.MinReplicas).To(Equal(int32(1)))
			Expect(pdbwatcher.Status.TargetGeneration).ToNot(BeZero())

			// run it twice so we hit unhandled eviction == false
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			//Should not have scaled.
			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "example-deployment", Namespace: namespace}, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(1))) // Change as needed to verify scaling
		})

		It("should deal with an eviction", func() {
			By("scaling up on reconcile")
			controllerReconciler := &PDBWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// run it once to populate target genration
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify PDBWatcher resource
			pdbwatcher := &v1.PDBWatcher{}
			err = k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			Expect(err).NotTo(HaveOccurred())
			pdbwatcher.Spec.LastEviction = v1.Eviction{
				PodName:      "somepod", //
				EvictionTime: time.Now().Format(time.RFC3339),
			}
			Expect(k8sClient.Update(ctx, pdbwatcher)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify PDBWatcher resource
			err = k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			Expect(err).NotTo(HaveOccurred())
			Expect(pdbwatcher.Spec.LastEviction.PodName).To(Equal("somepod"))
			Expect(pdbwatcher.Spec.LastEviction.EvictionTime).To(Equal(pdbwatcher.Spec.LastEviction.EvictionTime))

			// Verify Deployment scaling if necessary
			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "example-deployment", Namespace: namespace}, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2))) // Change as needed to verify scaling
		})

		//TODO scaledown
		//TODO reset on deployment change
		//TODO do noting on old eviction
	})
})

func int32Ptr(i int32) *int32 {
	return &i
}
