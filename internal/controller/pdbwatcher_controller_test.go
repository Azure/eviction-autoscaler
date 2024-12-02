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
		const deploymentName = "example-deployment"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: namespace}
		deploymentNamespacedName := types.NamespacedName{Name: deploymentName, Namespace: namespace}

		BeforeEach(func() {
			By("creating the custom resource for the Kind PDBWatcher")
			pdbwatcher := &v1.PDBWatcher{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.PDBWatcherSpec{
					TargetName: deploymentName,
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
					Name:      deploymentName,
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
			deleteResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: namespace}})
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
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(1))) // Change as needed to verify scaling
		})

		It("should deal with an eviction when allowedDisruptions == 0", func() {
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

			// Log an eviction (webhook would do this in e2e)
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
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2))) // Change as needed to verify scaling
		})

		//should this be merged with above?
		It("should deal with an eviction when allowedDisruptions > 0 ", func() {
			By("waiting on first on reconcile")
			controllerReconciler := &PDBWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// simulate previously scaled up on an eviction
			deployment := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			deployment.Spec.Replicas = int32Ptr(2)
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			// Log an eviction (webhook would do this in e2e)
			pdbwatcher := &v1.PDBWatcher{}
			err = k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			Expect(err).NotTo(HaveOccurred())
			pdbwatcher.Spec.LastEviction = v1.Eviction{
				PodName:      "somepod", //
				EvictionTime: time.Now().Format(time.RFC3339),
			}
			Expect(k8sClient.Update(ctx, pdbwatcher)).To(Succeed())
			pdbwatcher.Status.MinReplicas = 1
			pdbwatcher.Status.TargetGeneration = deployment.Generation
			Expect(k8sClient.Status().Update(ctx, pdbwatcher)).To(Succeed())

			//have the pdb show it
			pdb := &policyv1.PodDisruptionBudget{}
			err = k8sClient.Get(ctx, typeNamespacedName, pdb)
			Expect(err).NotTo(HaveOccurred())
			pdb.Status.DisruptionsAllowed = 1
			Expect(k8sClient.Status().Update(ctx, pdb)).To(Succeed())

			//first reconcile should do demure.
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))

			// Deployment is not changed yet
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2))) // Change as needed to verify scaling

			// Verify PDBWatcher resource
			err = k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			Expect(err).NotTo(HaveOccurred())
			Expect(pdbwatcher.Spec.LastEviction.PodName).To(Equal("somepod"))
			Expect(pdbwatcher.Spec.LastEviction.EvictionTime).To(Equal(pdbwatcher.Spec.LastEviction.EvictionTime))

			By("scaling down after cooldown")
			//okay lets say the eviction is older though
			//TODO make cooldown const/configurable
			pdbwatcher.Spec.LastEviction.EvictionTime = time.Now().Add(-15 * time.Second).Format(time.RFC3339)
			Expect(k8sClient.Update(ctx, pdbwatcher)).To(Succeed())

			//second reconcile should scaledown.
			result, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Deployment scaled down to 1
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(1))) // Change as needed to verify scaling
		})

		//TODO reset on deployment change
		It("should deal with deployment spec change", func() {
			By("reseting min replicas and target generation")
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
			Expect(pdbwatcher.Status.MinReplicas).To(Equal(int32(1)))
			Expect(pdbwatcher.Status.TargetGeneration).ToNot(BeZero())
			firstGeneration := pdbwatcher.Status.TargetGeneration

			// outside user changes deployment
			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			deployment.Spec.Replicas = int32Ptr(5)
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify PDBWatcher resource reset min replicas and target genration
			err = k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			Expect(err).NotTo(HaveOccurred())
			Expect(pdbwatcher.Status.MinReplicas).To(Equal(int32(5)))
			Expect(pdbwatcher.Status.TargetGeneration).ToNot(BeZero())
			Expect(pdbwatcher.Status.TargetGeneration).ToNot(Equal(firstGeneration))

			// Verify Deployment left alone?
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(5))) // Change as needed to verify scaling
		})

		//TODO do noting on old eviction
		//TODO test a statefulset.
	})
})

func int32Ptr(i int32) *int32 {
	return &i
}
