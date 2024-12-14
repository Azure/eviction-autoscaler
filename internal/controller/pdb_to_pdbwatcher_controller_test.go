package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	types "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("PDBToPDBWatcherReconciler", func() {
	var (
		reconciler *PDBToPDBWatcherReconciler
		namespace  string
	)

	BeforeEach(func() {

		// Set the namespace to "test" instead of "default"
		namespace = "test"

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
	})

	Context("When the PDB exists", func() {
		It("should create a PDBWatcher if it doesn't already exist", func() {
			// Prepare a PodDisruptionBudget in the "test" namespace
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example-pdb",
					Namespace: namespace,
				},
			}

			// Add PDB to fake client
			Expect(k8sClient.Create(context.Background(), pdb)).Should(Succeed())

			// Prepare the PDBWatcher object that will be checked if it exists
			pdbWatcher := &types.PDBWatcher{}
			err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "example-pdb", Namespace: namespace}, pdbWatcher)
			Expect(err).Should(HaveOccurred()) // PDBWatcher does not exist initially

			// Simulate PDBWatcher creation
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      "example-pdb",
					Namespace: namespace,
				},
			}

			// Reconcile the request
			_, err = reconciler.Reconcile(context.Background(), req)

			Expect(err).ShouldNot(HaveOccurred())

			// Verify that the PDBWatcher was created
			err = k8sClient.Get(context.Background(), client.ObjectKey{Name: "example-pdb", Namespace: namespace}, pdbWatcher)
			Expect(err).Should(Succeed()) // PDBWatcher should now exist
		})
	})

	Context("When the PDB is deleted", func() {
		It("should delete the PDBWatcher if it exists", func() {
			// Prepare a PodDisruptionBudget in the "test" namespace
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example-pdb",
					Namespace: namespace,
				},
			}

			// Add PDB to fake client
			_ = k8sClient.Create(context.Background(), pdb)

			// Prepare PDBWatcher and create it
			pdbWatcher := &types.PDBWatcher{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example-pdb",
					Namespace: namespace,
				},
			}
			_ = k8sClient.Create(context.Background(), pdbWatcher)

			// Now, delete the PDB
			Expect(k8sClient.Delete(context.Background(), pdb)).Should(Succeed())

			// Reconcile the request to check if PDBWatcher is deleted
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      "example-pdb",
					Namespace: namespace,
				},
			}
			_, err := reconciler.Reconcile(context.Background(), req)

			Expect(err).ShouldNot(HaveOccurred())

			// Verify that the PDBWatcher was deleted
			err = k8sClient.Get(context.Background(), client.ObjectKey{Name: "example-pdb", Namespace: namespace}, pdbWatcher)
			Expect(err).Should(HaveOccurred()) // PDBWatcher should no longer exist
		})
	})

	Context("When the PDBWatcher already exists", func() {
		It("should not create a new PDBWatcher", func() {
			// Prepare a PodDisruptionBudget in the "test" namespace
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example-pdb",
					Namespace: namespace,
				},
			}

			_ = k8sClient.Create(context.Background(), pdb)

			// Prepare the PDBWatcher object that will be created if it doesn't exist
			pdbWatcher := &types.PDBWatcher{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example-pdb",
					Namespace: namespace,
				},
			}
			Expect(k8sClient.Create(context.Background(), pdbWatcher)).Should(Succeed())

			// Simulate PDBWatcher already exists scenario
			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      "example-pdb",
					Namespace: namespace,
				},
			}

			// Reconcile the request
			_, err := reconciler.Reconcile(context.Background(), req)

			Expect(err).ShouldNot(HaveOccurred())

			// Verify that the PDBWatcher was not created again
			err = k8sClient.Get(context.Background(), client.ObjectKey{Name: "example-pdb", Namespace: namespace}, pdbWatcher)
			Expect(err).Should(Succeed()) // PDBWatcher should already exist, not re-created
		})
	})
})
