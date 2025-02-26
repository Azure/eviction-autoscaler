package controllers

import (
	"context"

	types "github.com/azure/eviction-autoscaler/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	machinery_types "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("PDBToEvictionAutoScalerReconciler", func() {
	var (
		reconciler *PDBToEvictionAutoScalerReconciler
		// Set the namespace to "test" instead of "default"
		namespace      string
		deploymentName = "example-deployment"
		ctx            context.Context
	)
	const podName = "example-pod"
	BeforeEach(func() {
		ctx = context.Background()

		// Create the Namespace object (from corev1)
		namespaceObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test",
			},
		}

		// create the namespace using the controller-runtime client
		Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
		namespace = namespaceObj.Name

		s := scheme.Scheme
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		Expect(policyv1.AddToScheme(s)).To(Succeed())
		// Initialize the reconciler with the fake client
		reconciler = &PDBToEvictionAutoScalerReconciler{
			Client: k8sClient,
			Scheme: s,
		}

		surge := intstr.FromInt(1)
		// Define a Deployment to test
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(3),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": deploymentName,
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
							"app": deploymentName,
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

		// Create the deployment
		err := reconciler.Client.Create(ctx, deployment)
		Expect(err).ToNot(HaveOccurred())

		// Define the ReplicaSet
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName, // ReplicaSet name should match the deployment name or whatever identifier you'd like
				Namespace: namespace,
				Labels: map[string]string{
					"app": deploymentName,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",    // API version of the owner (e.g., Deployment)
						Kind:       "Deployment", // The kind of the owner (usually Deployment for replicas)
						Name:       deploymentName,
						UID:        machinery_types.UID("some-uid"),
					},
				},
			},
			Spec: appsv1.ReplicaSetSpec{
				Replicas: int32Ptr(3), // Define the number of replicas you want
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": deploymentName,
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": deploymentName,
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "nginx",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: namespace,
				Labels: map[string]string{
					"app": deploymentName,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",      // API version of the owner (ReplicaSet)
						Kind:       "ReplicaSet",   // The kind of the owner (ReplicaSet)
						Name:       deploymentName, // Indicating that this Pod is controlled by ReplicaSet
						UID:        machinery_types.UID("some-uid"),
					},
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
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		pod.Status = corev1.PodStatus{ // Use corev1.PodStatus
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{ // Use corev1.PodCondition
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		}
		err = k8sClient.Status().Update(ctx, pod)
		Expect(err).ToNot(HaveOccurred())
	})

	Context("When the PDB exists", func() {
		It("should create a EvictionAutoScaler if it doesn't already exist", func() {
			// Prepare a PodDisruptionBudget in the "test" namespace
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app": deploymentName,
					},
					},
				},
			}

			// Add PDB to fake client
			Expect(k8sClient.Create(ctx, pdb)).Should(Succeed())

			// Prepare the EvictionAutoScaler object that will be checked if it exists
			EvictionAutoScaler := &types.EvictionAutoScaler{}
			err := k8sClient.Get(ctx, client.ObjectKey{Name: deploymentName, Namespace: namespace}, EvictionAutoScaler)
			Expect(err).Should(HaveOccurred()) // EvictionAutoScaler does not exist initially

			// Simulate EvictionAutoScaler creation
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}

			// Reconcile the request
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).ShouldNot(HaveOccurred())

			// Verify that the EvictionAutoScaler was created
			err = k8sClient.Get(ctx, client.ObjectKey{Name: deploymentName, Namespace: namespace}, EvictionAutoScaler)
			Expect(err).Should(Succeed()) // EvictionAutoScaler should now exist
		})
	})

	Context("When the EvictionAutoScaler already exists", func() {
		It("should not create a new EvictionAutoScaler", func() {
			// Prepare a PodDisruptionBudget in the "test" namespace
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}

			Expect(k8sClient.Create(ctx, pdb)).Should(Succeed())

			// Prepare the EvictionAutoScaler object that will be created if it doesn't exist
			EvictionAutoScaler := &types.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).Should(Succeed())

			// Simulate EvictionAutoScaler already exists scenario
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}

			// Reconcile the request
			_, err := reconciler.Reconcile(ctx, req)

			Expect(err).ShouldNot(HaveOccurred())

			// Verify that the EvictionAutoScaler was not created again
			err = k8sClient.Get(ctx, client.ObjectKey{Name: deploymentName, Namespace: namespace}, EvictionAutoScaler)
			Expect(err).Should(Succeed()) // EvictionAutoScaler should already exist, not re-created
		})
	})
})
