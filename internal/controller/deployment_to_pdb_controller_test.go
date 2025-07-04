package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("DeploymentToPDBReconciler", func() {
	var namespace string
	const deploymentName = "example-deployment"

	var (
		r          *DeploymentToPDBReconciler
		deployment *appsv1.Deployment
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()

		namespaceObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test",
			},
		}

		// create the namespace using the controller-runtime client
		Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
		namespace = namespaceObj.Name

		// Create a fake clientset and add required schemas
		s := scheme.Scheme
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		Expect(policyv1.AddToScheme(s)).To(Succeed())

		surge := intstr.FromInt(1)
		maxUnavailable := intstr.FromInt(0) // Explicitly set to 0 to ensure PDB is created
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
						MaxSurge:       &surge,
						MaxUnavailable: &maxUnavailable,
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
		Expect(r.Client.Create(ctx, deployment)).To(Succeed())
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
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Check if PDB is created
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Client.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deploymentName,
			}, pdb)
			Expect(err).ToNot(HaveOccurred())

			Expect(pdb.Name).To(Equal(deploymentName))
			Expect((*pdb.Spec.MinAvailable).IntVal).To(Equal(int32(3)))
		})

		It("should not create a PodDisruptionBudget if one already matches", func() {
			minavailable := intstr.FromInt(1)
			existingpdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rando",
					Namespace: namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app": "example",
					},
					},
					MinAvailable: &minavailable,
				},
			}
			Expect(r.Client.Create(ctx, existingpdb)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      deploymentName,
				},
			}

			// Call the reconciler
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Check if PDB is created
			newpdb := &policyv1.PodDisruptionBudget{}
			err = r.Client.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deploymentName,
			}, newpdb)
			Expect(errors.IsNotFound(err)).To(BeTrue())
			//should we list it?
		})

		It("should not create a PodDisruptionBudget if maxUnavailable is not 0", func() {
			// Create a deployment with maxUnavailable set to 25%
			maxUnavailablePercent := intstr.FromString("25%")
			surge := intstr.FromInt(1)
			deploymentWithMaxUnavailable := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-with-max-unavailable",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "example-max-unavailable",
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxSurge:       &surge,
							MaxUnavailable: &maxUnavailablePercent,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "example-max-unavailable",
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

			// Create the deployment
			Expect(r.Client.Create(ctx, deploymentWithMaxUnavailable)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      "deployment-with-max-unavailable",
				},
			}

			// Call the reconciler
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Check that PDB was NOT created
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Client.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      "deployment-with-max-unavailable",
			}, pdb)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should not create a PodDisruptionBudget if maxUnavailable is an integer > 0", func() {
			// Create a deployment with maxUnavailable set to 1 (integer)
			maxUnavailableInt := intstr.FromInt(1)
			surge := intstr.FromInt(1)
			deploymentWithMaxUnavailableInt := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-with-max-unavailable-int",
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "example-max-unavailable-int",
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxSurge:       &surge,
							MaxUnavailable: &maxUnavailableInt,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "example-max-unavailable-int",
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

			// Create the deployment
			Expect(r.Client.Create(ctx, deploymentWithMaxUnavailableInt)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      "deployment-with-max-unavailable-int",
				},
			}

			// Call the reconciler
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Check that PDB was NOT created
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Client.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      "deployment-with-max-unavailable-int",
			}, pdb)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Describe("when the PDB already exists", func() {
		It("should not take any further action", func() {
			// Create the PDB first
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "someothername",
					Namespace: namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app": "example",
					},
					},
				},
			}
			err := r.Client.Create(ctx, pdb)
			Expect(err).ToNot(HaveOccurred())

			// Reconcile should not take any further action since the PDB already exists
			_, err = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      deploymentName,
				},
			})

			Expect(err).ToNot(HaveOccurred())
		})
	})
})
