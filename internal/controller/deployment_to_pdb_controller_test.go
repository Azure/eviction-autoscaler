package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("DeploymentToPDBReconciler", func() {
	const namespace = "test"
	const deploymentName = "example-deployment"

	var (
		r          *DeploymentToPDBReconciler
		deployment *appsv1.Deployment
	)

	BeforeEach(func() {
		// Create the Namespace object (from corev1)
		namespaceObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}

		// create the namespace using the controller-runtime client
		_ = k8sClient.Create(context.Background(), namespaceObj)

		// Create a fake clientset and add required schemas
		s := scheme.Scheme
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		Expect(policyv1.AddToScheme(s)).To(Succeed())

		surge := intstr.FromInt(1)
		// Create the reconciler instance
		r = &DeploymentToPDBReconciler{
			Client: k8sClient, // Use the fake client
			Scheme: s,
		}

		// Define a Deployment to test
		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(3),
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

		// Create the deployment
		_ = r.Client.Create(context.Background(), deployment)
		//Expect(err).To(BeNil())
	})

	AfterEach(func() {
		// Create the PDB with a deletion timestamp set
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
			},
		}
		_ = r.Client.Delete(context.Background(), pdb)
		//Expect(err).To(BeNil())

	})

	Describe("when a deployment is created", func() {
		It("should create a PodDisruptionBudget", func() {
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      deploymentName,
				},
			}

			// Call the reconciler
			_, err := r.Reconcile(context.Background(), req)
			Expect(err).To(BeNil())

			// Check if PDB is created
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Client.Get(context.Background(), client.ObjectKey{
				Namespace: namespace,
				Name:      deploymentName,
			}, pdb)
			Expect(err).To(BeNil())

			Expect(pdb.Name).To(Equal(deploymentName))
			Expect((*pdb.Spec.MinAvailable).IntVal).To(Equal(int32(3)))
		})
	})

	Describe("when a deployment is deleted", func() {
		It("should delete the corresponding PodDisruptionBudget", func() {

			// Now delete the deployment
			err := r.Client.Delete(context.Background(), deployment)
			Expect(err).To(BeNil())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      deploymentName,
				},
			}

			// Reconcile should delete the PDB
			_, err = r.Reconcile(context.Background(), req)
			Expect(err).To(BeNil())

			// Check if PDB was deleted
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Client.Get(context.Background(), client.ObjectKey{
				Namespace: namespace,
				Name:      deploymentName,
			}, pdb)
			Expect(err).To(HaveOccurred()) // Should return an error (not found)
		})
	})

	Describe("when the PDB already exists", func() {
		It("should not take any further action", func() {
			// Create the PDB first
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app": "example",
					},
					},
				},
			}
			_ = r.Client.Create(context.Background(), pdb)
			//Expect(err).To(BeNil())

			// Reconcile should not take any further action since the PDB already exists
			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      deploymentName,
				},
			})

			Expect(err).To(BeNil())
		})
	})
})
