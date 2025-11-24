package controllers

import (
	"context"

	"github.com/go-logr/logr/testr"
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
	"sigs.k8s.io/controller-runtime/pkg/event"
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

		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
				Labels:    map[string]string{"app": "skip"},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(2),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "skip"},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": "skip"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "nginx", Image: "nginx:latest"},
						},
					},
				},
			},
		}
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

// We had a bug where we wou
var _ = Describe("DeploymentToPDBReconciler triggerOnReplicaChange", func() {

	type testCase struct {
		name           string
		oldObject      client.Object
		newObject      client.Object
		expectedResult bool
	}

	DescribeTable("should correctly determine when to trigger on deployment updates",
		func(tc testCase) {
			updateEvent := event.UpdateEvent{
				ObjectOld: tc.oldObject,
				ObjectNew: tc.newObject,
			}

			result := triggerOnReplicaChange(updateEvent, testr.NewWithInterface(GinkgoT(), testr.Options{}))
			Expect(result).To(Equal(tc.expectedResult))
		},
		Entry("should return true when replicas increase", testCase{
			name: "replicas increase",
			oldObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(2),
				},
			},
			newObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(5),
				},
			},
			expectedResult: true,
		}),
		Entry("should return true when replicas decrease", testCase{
			name: "replicas decrease",
			oldObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(5),
				},
			},
			newObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(2),
				},
			},
			expectedResult: true,
		}),
		Entry("should return false when replicas stay the same", testCase{
			name: "replicas stay the same",
			oldObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
			},
			newObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
			},
			expectedResult: false,
		}),
		Entry("should return false when old object is not a deployment", testCase{
			name:      "old object is not a deployment",
			oldObject: &corev1.Pod{},
			newObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
			},
			expectedResult: false,
		}),
		Entry("should handle nil replicas gracefully", testCase{
			name: "nil replicas in old deployment",
			oldObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: nil,
				},
			},
			newObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
			},
			expectedResult: true,
		}),
		Entry("should handle both nil replicas", testCase{
			name: "both deployments have nil replicas",
			oldObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: nil,
				},
			},
			newObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: nil,
				},
			},
			expectedResult: false,
		}),
		Entry("should handle new deployment with nil replicas", testCase{
			name: "new deployment has nil replicas",
			oldObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
			},
			newObject: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: nil,
				},
			},
			expectedResult: true,
		}),
	)
})

var _ = Describe("DeploymentToPDBReconciler with enable-eviction-autoscaler annotation", func() {
	var (
		namespace  string
		deployment *appsv1.Deployment
		r          *DeploymentToPDBReconciler
		ctx        context.Context
	)

	const deploymentName = "test-deployment"

	BeforeEach(func() {
		ctx = context.Background()

		s := scheme.Scheme
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		Expect(policyv1.AddToScheme(s)).To(Succeed())

		r = &DeploymentToPDBReconciler{
			Client: k8sClient,
			Scheme: s,
		}
	})

	Context("when deployment is in kube-system namespace", func() {
		BeforeEach(func() {
			// Use kube-system namespace
			namespace = "kube-system"

			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(2),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "nginx", Image: "nginx:latest"},
							},
						},
					},
				},
			}
		})

		It("should create PDB by default without annotation", func() {
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: namespace, Name: deploymentName},
			}
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deploymentName}, pdb)
			Expect(err).ToNot(HaveOccurred())
			Expect(pdb.Name).To(Equal(deploymentName))
		})
	})

	Context("when deployment is in non-kube-system namespace", func() {
		BeforeEach(func() {
			// Create a test namespace
			namespaceObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-enable-",
				},
			}
			Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
			namespace = namespaceObj.Name

			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(2),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "nginx", Image: "nginx:latest"},
							},
						},
					},
				},
			}
		})

		It("should NOT create PDB without annotation", func() {
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: namespace, Name: deploymentName},
			}
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deploymentName}, pdb)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should create PDB when annotation is set to true on namespace", func() {
			// Update namespace with annotation
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: namespace}, ns)).To(Succeed())
			ns.Annotations = map[string]string{EnableEvictionAutoscalerAnnotationKey: "true"}
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: namespace, Name: deploymentName},
			}
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deploymentName}, pdb)
			Expect(err).ToNot(HaveOccurred())
			Expect(pdb.Name).To(Equal(deploymentName))
		})

		It("should NOT create PDB when annotation is set to false on namespace", func() {
			// Update namespace with annotation
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: namespace}, ns)).To(Succeed())
			ns.Annotations = map[string]string{EnableEvictionAutoscalerAnnotationKey: "false"}
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: namespace, Name: deploymentName},
			}
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deploymentName}, pdb)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should NOT create PDB when annotation is set to empty string on namespace", func() {
			// Update namespace with annotation
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: namespace}, ns)).To(Succeed())
			ns.Annotations = map[string]string{EnableEvictionAutoscalerAnnotationKey: ""}
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: namespace, Name: deploymentName},
			}
			_, err := r.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			pdb := &policyv1.PodDisruptionBudget{}
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deploymentName}, pdb)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})
})
