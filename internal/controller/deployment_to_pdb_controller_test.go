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

// createDeployment is a helper function to create a deployment for testing
func createDeployment(name, namespace, appLabel string, replicas int32, maxUnavailable *intstr.IntOrString) *appsv1.Deployment {
	surge := intstr.FromInt(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": appLabel,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": appLabel,
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

	// Only set strategy if maxUnavailable is provided
	if maxUnavailable != nil {
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxSurge:       &surge,
				MaxUnavailable: maxUnavailable,
			},
		}
	}

	return deployment
}

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
				Annotations: map[string]string{
					EnableEvictionAutoscalerAnnotationKey: "true",
				},
			},
		}

		// create the namespace using the controller-runtime client
		Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
		namespace = namespaceObj.Name

		// Create a fake clientset and add required schemas
		s := scheme.Scheme
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		Expect(policyv1.AddToScheme(s)).To(Succeed())

		maxUnavailable := intstr.FromInt(0) // Explicitly set to 0 to ensure PDB is created
		// Create the reconciler instance
		r = &DeploymentToPDBReconciler{
			Client: k8sClient, // Use the fake client
			Scheme: s,
		}

		// Define a Deployment to test using helper
		deployment = createDeployment(deploymentName, namespace, "example", 3, &maxUnavailable)

		// Create the deployment
		Expect(r.Client.Create(ctx, deployment)).To(Succeed())
	})

	Describe("when a deployment is created", func() {
		It("should create a PodDisruptionBudget", func() {
			var err error
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      deploymentName,
				},
			}

			// Call the reconciler
			_, err = r.Reconcile(ctx, req)
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
			deploymentWithMaxUnavailable := createDeployment(
				"deployment-with-max-unavailable",
				namespace,
				"example-max-unavailable",
				3,
				&maxUnavailablePercent,
			)

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
			deploymentWithMaxUnavailableInt := createDeployment(
				"deployment-with-max-unavailable-int",
				namespace,
				"example-max-unavailable-int",
				3,
				&maxUnavailableInt,
			)

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

		It("should create a PodDisruptionBudget if maxUnavailable is string '0%'", func() {
			// Create a deployment with maxUnavailable set to "0%" (percentage string)
			maxUnavailableZeroPercent := intstr.FromString("0%") // Explicitly set to 0% to ensure PDB is created
			deploymentWithMaxUnavailableZeroPercent := createDeployment(
				"deployment-with-max-unavailable-zero-percent",
				namespace,
				"example-max-unavailable-zero-percent",
				3,
				&maxUnavailableZeroPercent,
			)

			// Create the deployment
			Expect(r.Client.Create(ctx, deploymentWithMaxUnavailableZeroPercent)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      "deployment-with-max-unavailable-zero-percent",
				},
			}

			// Call the reconciler
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Check that PDB WAS created
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Client.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      "deployment-with-max-unavailable-zero-percent",
			}, pdb)
			Expect(err).ToNot(HaveOccurred())
			Expect(pdb.Name).To(Equal("deployment-with-max-unavailable-zero-percent"))
		})

		It("should create a PodDisruptionBudget if maxUnavailable is nil (no RollingUpdate strategy)", func() {
			// Create a deployment without RollingUpdate strategy set (nil)
			deploymentWithNilStrategy := createDeployment(
				"deployment-with-nil-strategy",
				namespace,
				"example-nil-strategy",
				3,
				nil, // No maxUnavailable
			)
			// Override strategy to Recreate
			deploymentWithNilStrategy.Spec.Strategy = appsv1.DeploymentStrategy{
				Type:          appsv1.RecreateDeploymentStrategyType,
				RollingUpdate: nil, // No rolling update strategy
			}

			// Create the deployment
			Expect(r.Client.Create(ctx, deploymentWithNilStrategy)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: namespace,
					Name:      "deployment-with-nil-strategy",
				},
			}

			// Call the reconciler
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Check that PDB WAS created (since there's no maxUnavailable check when RollingUpdate is nil)
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Client.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      "deployment-with-nil-strategy",
			}, pdb)
			Expect(err).ToNot(HaveOccurred())
			Expect(pdb.Name).To(Equal("deployment-with-nil-strategy"))
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

var _ = Describe("DeploymentToPDBReconciler PDB creation control", func() {
	var (
		namespace  string
		deployment *appsv1.Deployment
		r          *DeploymentToPDBReconciler
		ctx        context.Context
	)

	const deploymentName = "skip-pdb-deployment"

	BeforeEach(func() {
		ctx = context.Background()

		// Create namespace
		namespaceObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-skip-",
				Annotations: map[string]string{
					EnableEvictionAutoscalerAnnotationKey: "true",
				},
			},
		}
		Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
		namespace = namespaceObj.Name

		s := scheme.Scheme
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		Expect(policyv1.AddToScheme(s)).To(Succeed())

		r = &DeploymentToPDBReconciler{
			Client: k8sClient,
			Scheme: s,
		}

		// Use helper to create deployment
		maxUnavailable := intstr.FromInt(0)
		deployment = createDeployment(deploymentName, namespace, "skip", 2, &maxUnavailable)
		deployment.Labels = map[string]string{"app": "skip"}
	})

	It("should skip PDB creation if deployment annotation disables it", func() {
		var err error
		deployment.Annotations = map[string]string{PDBCreateAnnotationKey: "false"}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: namespace, Name: deploymentName},
		}
		_, err = r.Reconcile(ctx, req)
		Expect(err).ToNot(HaveOccurred())

		pdb := &policyv1.PodDisruptionBudget{}
		err = k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deploymentName}, pdb)
		Expect(err).To(HaveOccurred())
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})
})
