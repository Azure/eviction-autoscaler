package controllers

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	types "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("PDBToPDBWatcherReconciler", func() {
	var (
		reconciler *PDBToPDBWatcherReconciler
		// Set the namespace to "test" instead of "default"
		namespace      = "test"
		deploymentName = "example-deployment"
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

		s := scheme.Scheme
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		Expect(policyv1.AddToScheme(s)).To(Succeed())
		// Initialize the reconciler with the fake client
		reconciler = &PDBToPDBWatcherReconciler{
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
						"app": "example-deployment",
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
							"app": "example-deployment",
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
		_ = reconciler.Client.Create(context.Background(), deployment)

		podList := &corev1.PodList{}
		_ = reconciler.List(context.Background(), podList, &client.ListOptions{Namespace: namespace})
		fmt.Printf("number of pods %d", len(podList.Items))
		for _, pod := range podList.Items {
			fmt.Printf("looking at pod %s", pod.Name)
		}
	})

	AfterEach(func() {
		// Create the PDB with a deletion timestamp set
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
			},
		}
		_ = reconciler.Client.Delete(context.Background(), pdb)
		//Expect(err).To(BeNil())

	})

	Context("When the PDB exists", func() {
		It("should create a PDBWatcher if it doesn't already exist", func() {
			// Prepare a PodDisruptionBudget in the "test" namespace
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app": "example-deployment",
					},
					},
				},
			}

			// Add PDB to fake client
			Expect(k8sClient.Create(context.Background(), pdb)).Should(Succeed())

			// Prepare the PDBWatcher object that will be checked if it exists
			pdbWatcher := &types.PDBWatcher{}
			err := k8sClient.Get(context.Background(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, pdbWatcher)
			Expect(err).Should(HaveOccurred()) // PDBWatcher does not exist initially

			// Simulate PDBWatcher creation
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}

			// Reconcile the request
			_, err = reconciler.Reconcile(context.Background(), req)

			Expect(err).ShouldNot(HaveOccurred())

			// Verify that the PDBWatcher was created
			err = k8sClient.Get(context.Background(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, pdbWatcher)
			Expect(err).Should(Succeed()) // PDBWatcher should now exist
		})
	})

	Context("When the PDB is deleted", func() {
		It("should delete the PDBWatcher if it exists", func() {
			// Prepare a PodDisruptionBudget in the "test" namespace
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}

			// Add PDB to fake client
			_ = k8sClient.Create(context.Background(), pdb)

			// Prepare PDBWatcher and create it
			pdbWatcher := &types.PDBWatcher{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}
			_ = k8sClient.Create(context.Background(), pdbWatcher)

			// Now, delete the PDB
			Expect(k8sClient.Delete(context.Background(), pdb)).Should(Succeed())

			// Reconcile the request to check if PDBWatcher is deleted
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}
			_, err := reconciler.Reconcile(context.Background(), req)

			Expect(err).ShouldNot(HaveOccurred())

			// Verify that the PDBWatcher was deleted
			err = k8sClient.Get(context.Background(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, pdbWatcher)
			Expect(err).Should(HaveOccurred()) // PDBWatcher should no longer exist
		})
	})

	Context("When the PDBWatcher already exists", func() {
		It("should not create a new PDBWatcher", func() {
			// Prepare a PodDisruptionBudget in the "test" namespace
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}

			_ = k8sClient.Create(context.Background(), pdb)

			// Prepare the PDBWatcher object that will be created if it doesn't exist
			pdbWatcher := &types.PDBWatcher{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}
			Expect(k8sClient.Create(context.Background(), pdbWatcher)).Should(Succeed())

			// Simulate PDBWatcher already exists scenario
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      deploymentName,
					Namespace: namespace,
				},
			}

			// Reconcile the request
			_, err := reconciler.Reconcile(context.Background(), req)

			Expect(err).ShouldNot(HaveOccurred())

			// Verify that the PDBWatcher was not created again
			err = k8sClient.Get(context.Background(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, pdbWatcher)
			Expect(err).Should(Succeed()) // PDBWatcher should already exist, not re-created
		})
	})
})
