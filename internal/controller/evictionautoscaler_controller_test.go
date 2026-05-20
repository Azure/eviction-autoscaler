package controllers

import (
	"context"
	"time"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// evictionTestFilter for EvictionAutoScaler tests
// Uses opt-out mode with kube-system enabled by default
// - kube-system: enabled by default (in hardcoded list, returns !optin = !false = true)
// - Other namespaces: disabled by default (not in hardcoded, returns optin = false)
// - Any namespace can override via annotation
type evictionTestFilter struct {
	filter filter
}

func (f *evictionTestFilter) Filter(ctx context.Context, c namespacefilter.Reader, ns string) (bool, error) {
	if f.filter == nil {
		f.filter = namespacefilter.New([]string{"kube-system"}, false) // opt-out: kube-system enabled, others disabled
	}
	return f.filter.Filter(ctx, c, ns)
}

var _ = Describe("EvictionAutoScaler Controller", func() {
	const (
		resourceName    = "test-resource"
		deploymentName  = "example-deployment"
		statefulSetName = "example-statefulset"
	)

	var namespace string

	ctx := context.Background()
	var typeNamespacedName, deploymentNamespacedName types.NamespacedName
	Context("When reconciling a resource", func() {

		BeforeEach(func() {

			namespaceObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: testGenerateName,
					Annotations: map[string]string{
						namespacefilter.EnableEvictionAutoscalerAnnotationKey: annotationTrue,
					},
				},
			}

			// create the namespace using the controller-runtime client
			Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
			namespace = namespaceObj.Name
			typeNamespacedName = types.NamespacedName{Name: resourceName, Namespace: namespace}
			deploymentNamespacedName = types.NamespacedName{Name: deploymentName, Namespace: namespace}

			By("creating the custom resource for the Kind EvictionAutoScaler")
			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: deploymentName,
					TargetKind: deploymentKind,
				},
			}
			err := k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())
			}

			By("creating a Deployment resource")
			surge := intstr.FromInt(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							appLabelKey: exampleLabelValue,
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
								appLabelKey: exampleLabelValue,
							},
						},
						Spec: corev1.PodSpec{ // Use corev1.PodSpec
							Containers: []corev1.Container{ // Use corev1.Container
								{
									Name:  nginxContainerName,
									Image: nginxImage,
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
							appLabelKey: exampleLabelValue,
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
		})

		It("should successfully reconcile the resource", func() {
			By("reconciling the created resource")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify EvictionAutoScaler resource
			EvictionAutoScaler := &v1.EvictionAutoScaler{}
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Status.MinReplicas).To(Equal(int32(1)))
			Expect(EvictionAutoScaler.Status.TargetGeneration).ToNot(BeZero())
			Expect(EvictionAutoScaler.Status.Conditions).To(HaveLen(1))
			Expect(EvictionAutoScaler.Status.Conditions[0].Type).To(Equal("Ready"))
			Expect(EvictionAutoScaler.Status.Conditions[0].Reason).To(Equal("TargetSpecChange"))

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

			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Status.MinReplicas).To(Equal(int32(1)))
			Expect(EvictionAutoScaler.Status.TargetGeneration).ToNot(BeZero())

			Expect(EvictionAutoScaler.Status.Conditions).To(HaveLen(1))
			Expect(EvictionAutoScaler.Status.Conditions[0].Type).To(Equal("Ready"))
			Expect(EvictionAutoScaler.Status.Conditions[0].Reason).To(Equal("Reconciled"))
		})

		It("should deal with an eviction when allowedDisruptions == 0", func() {
			By("scaling up on reconcile")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			// run it once to populate target genration
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Log an eviction
			EvictionAutoScaler := &v1.EvictionAutoScaler{}
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.LastEviction = v1.Eviction{
				PodName:      somePodName, //
				EvictionTime: metav1.Now(),
			}
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify EvictionAutoScaler resource
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Spec.LastEviction.PodName).To(Equal(somePodName))
			//we don't update status of last eviction till
			Expect(EvictionAutoScaler.Spec.LastEviction.EvictionTime).ToNot(Equal(EvictionAutoScaler.Status.LastEviction.EvictionTime))

			// Verify Deployment scaling if necessary
			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2))) // Change as needed to verify scaling
		})

		It("should skip StatefulSet targets without surging", func() {

			By("creating a StatefulSet resource")
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      statefulSetName,
					Namespace: namespace,
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: new(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							appLabelKey: exampleLabelValue,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								appLabelKey: exampleLabelValue,
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  nginxContainerName,
									Image: nginxImage,
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, statefulSet)).To(Succeed())

			EvictionAutoScaler := &v1.EvictionAutoScaler{}
			err := k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.TargetName = statefulSetName
			EvictionAutoScaler.Spec.TargetKind = "statefulset"
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())

			By("reconciling and verifying StatefulSet is skipped")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Log an eviction
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.LastEviction = v1.Eviction{
				PodName:      somePodName,
				EvictionTime: metav1.Now(),
			}
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify StatefulSet replicas are unchanged (skip, no surge)
			err = k8sClient.Get(ctx, types.NamespacedName{Name: statefulSetName, Namespace: namespace}, statefulSet)
			Expect(err).NotTo(HaveOccurred())
			Expect(*statefulSet.Spec.Replicas).To(Equal(int32(1)))
		})

		//should this be merged with above?
		It("should deal with an eviction when allowedDisruptions > 0 ", func() {
			By("waiting on first on reconcile")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			} // simulate previously scaled up on an eviction
			deployment := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			deployment.Spec.Replicas = new(int32(2))
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			// Log an eviction
			EvictionAutoScaler := &v1.EvictionAutoScaler{}
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.LastEviction = v1.Eviction{
				PodName:      somePodName, //
				EvictionTime: metav1.Now(),
			}
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())
			EvictionAutoScaler.Status.MinReplicas = 1
			EvictionAutoScaler.Status.TargetGeneration = deployment.Generation
			Expect(k8sClient.Status().Update(ctx, EvictionAutoScaler)).To(Succeed())

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
			Expect(result.RequeueAfter).To(Equal(cooldown))

			// Deployment is not changed yet
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2))) // Change as needed to verify scaling

			// Verify EvictionAutoScaler resource
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Spec.LastEviction.PodName).To(Equal(somePodName))
			Expect(EvictionAutoScaler.Spec.LastEviction.EvictionTime).ToNot(Equal(EvictionAutoScaler.Status.LastEviction.EvictionTime))

			By("scaling down after cooldown")
			//okay lets say the eviction is older though
			//TODO make cooldown const/configurable
			EvictionAutoScaler.Spec.LastEviction.EvictionTime = metav1.NewTime(time.Now().Add(-2 * cooldown))
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())
			Expect(EvictionAutoScaler.Spec.LastEviction.EvictionTime).ToNot(Equal(EvictionAutoScaler.Status.LastEviction.EvictionTime))

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

			// EvictionAutoScaler should be ready and
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Spec.LastEviction.PodName).To(Equal(somePodName))
			Expect(EvictionAutoScaler.Spec.LastEviction.EvictionTime).To(Equal(EvictionAutoScaler.Status.LastEviction.EvictionTime))
			Expect(EvictionAutoScaler.Status.Conditions[0].Type).To(Equal("Ready"))
			Expect(EvictionAutoScaler.Status.Conditions[0].Reason).To(Equal("Reconciled"))

		})

		//TODO reset on deployment change
		It("should deal with deployment spec change", func() {
			By("reseting min replicas and target generation")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			// run it once to populate target genration
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify EvictionAutoScaler resource
			EvictionAutoScaler := &v1.EvictionAutoScaler{}
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Status.MinReplicas).To(Equal(int32(1)))
			Expect(EvictionAutoScaler.Status.TargetGeneration).ToNot(BeZero())
			firstGeneration := EvictionAutoScaler.Status.TargetGeneration

			// outside user changes deployment
			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			deployment.Spec.Replicas = new(int32(5))
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify EvictionAutoScaler resource reset min replicas and target genration
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Status.MinReplicas).To(Equal(int32(5)))
			Expect(EvictionAutoScaler.Status.TargetGeneration).ToNot(BeZero())
			Expect(EvictionAutoScaler.Status.TargetGeneration).ToNot(Equal(firstGeneration))

			// Verify Deployment left alone?
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(5))) // Change as needed to verify scaling
		})

		//TODO do noting on old eviction
		//TODO test a statefulset.

	})

	Context("when reconciling bad crds", func() {
		BeforeEach(func() {

			namespaceObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: testGenerateName,
					Annotations: map[string]string{
						namespacefilter.EnableEvictionAutoscalerAnnotationKey: annotationTrue,
					},
				},
			}

			// create the namespace using the controller-runtime client
			Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
			namespace = namespaceObj.Name
			typeNamespacedName = types.NamespacedName{Name: resourceName, Namespace: namespace}
		})

		It("should deal with no pdb", func() {
			By("by updating condition to degraded")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: deploymentName,
					TargetKind: deploymentKind,
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify EvictionAutoScaler resource
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Status.Conditions).To(HaveLen(1))
			Expect(EvictionAutoScaler.Status.Conditions[0].Type).To(Equal("Degraded"))
			Expect(EvictionAutoScaler.Status.Conditions[0].Reason).To(Equal("NoPdb"))
		})

		It("should deal with no target ", func() {
			By("by updating condition to degraded")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: "", //intentionally empty
					TargetKind: deploymentKind,
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
			}
			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify EvictionAutoScaler resource
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Status.Conditions).To(HaveLen(1))
			Expect(EvictionAutoScaler.Status.Conditions[0].Type).To(Equal("Degraded"))
			Expect(EvictionAutoScaler.Status.Conditions[0].Reason).To(Equal("EmptyTarget"))
		})

		It("should deal with bad target kind", func() {
			By("by updating condition to degraded")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: "something",
					TargetKind: "notavalidtarget",
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
			}
			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify EvictionAutoScaler resource
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Status.Conditions).To(HaveLen(1))
			Expect(EvictionAutoScaler.Status.Conditions[0].Type).To(Equal("Degraded"))
			Expect(EvictionAutoScaler.Status.Conditions[0].Reason).To(Equal("InvalidTarget"))
		})

		It("should deal with missing target", func() {
			By("by updating condition to degraded")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: "somethingmissing", //not found
					TargetKind: deploymentKind,
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
			}

			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify EvictionAutoScaler resource
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Status.Conditions).To(HaveLen(1))
			Expect(EvictionAutoScaler.Status.Conditions[0].Type).To(Equal("Degraded"))
			Expect(EvictionAutoScaler.Status.Conditions[0].Reason).To(Equal("MissingTarget"))
		})
	})

	Context("When checking namespace annotations", func() {
		var controllerReconciler *EvictionAutoScalerReconciler

		BeforeEach(func() {
			controllerReconciler = &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}
		})

		It("should NOT process EvictionAutoScaler in namespace without annotation", func() {
			// Create a namespace without annotation
			namespaceObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-no-anno-",
				},
			}
			Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
			testNamespace := namespaceObj.Name

			// Create deployment
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testDeployName,
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{appLabelKey: testGenerateName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{appLabelKey: testGenerateName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: nginxContainerName, Image: nginxImage},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Create PDB
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPDBName,
					Namespace: testNamespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{IntVal: 2},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{appLabelKey: testGenerateName},
					},
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 0,
				},
			}
			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

			// Create EvictionAutoScaler
			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPDBName,
					Namespace: testNamespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: testDeployName,
					TargetKind: deploymentKind,
					LastEviction: v1.Eviction{
						PodName:      testPodName,
						EvictionTime: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			// Reconcile should skip processing
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testPDBName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify deployment was NOT scaled (should still be 2)
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testDeployName, Namespace: testNamespace}, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))
		})

		It("should process EvictionAutoScaler in namespace with annotation", func() {
			// Create a namespace with annotation
			namespaceObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-with-anno-",
					Annotations: map[string]string{
						namespacefilter.EnableEvictionAutoscalerAnnotationKey: annotationTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
			testNamespace := namespaceObj.Name

			// Create deployment
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testDeployName,
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{appLabelKey: testGenerateName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{appLabelKey: testGenerateName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: nginxContainerName, Image: nginxImage},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Create PDB
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPDBName,
					Namespace: testNamespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{IntVal: 2},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{appLabelKey: testGenerateName},
					},
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 0,
				},
			}
			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

			// Create EvictionAutoScaler
			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPDBName,
					Namespace: testNamespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: testDeployName,
					TargetKind: deploymentKind,
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			// First reconcile to set up
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testPDBName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Add eviction
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testPDBName, Namespace: testNamespace}, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.LastEviction = v1.Eviction{
				PodName:      testPodName,
				EvictionTime: metav1.Now(),
			}
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())

			// Second reconcile should scale up
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testPDBName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify deployment WAS scaled
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testDeployName, Namespace: testNamespace}, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(3))) // Should be scaled up
		})

		It("should process EvictionAutoScaler in kube-system by default", func() {
			// Use kube-system namespace (no annotation needed)
			testNamespace := metav1.NamespaceSystem

			// Create deployment
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testKubeDeployName,
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{appLabelKey: testKubeLabelValue},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{appLabelKey: testKubeLabelValue},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: nginxContainerName, Image: nginxImage},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Create PDB
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testKubePDBName,
					Namespace: testNamespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{IntVal: 2},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{appLabelKey: testKubeLabelValue},
					},
				},
				Status: policyv1.PodDisruptionBudgetStatus{
					DisruptionsAllowed: 0,
				},
			}
			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

			// Create EvictionAutoScaler
			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testKubePDBName,
					Namespace: testNamespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: testKubeDeployName,
					TargetKind: deploymentKind,
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			// First reconcile to set up
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testKubePDBName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Add eviction
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testKubePDBName, Namespace: testNamespace}, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.LastEviction = v1.Eviction{
				PodName:      "test-kube-pod",
				EvictionTime: metav1.Now(),
			}
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())

			// Second reconcile should scale up
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testKubePDBName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify deployment WAS scaled (kube-system is enabled by default)
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testKubeDeployName, Namespace: testNamespace}, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(3))) // Should be scaled up

			// Cleanup
			Expect(k8sClient.Delete(ctx, EvictionAutoScaler)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pdb)).To(Succeed())
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})
})

var _ = Describe("EvictionAutoScaler Controller - unsupported autoscaler config", func() {
	ctx := context.Background()

	It("should set Degraded status and not requeue when KEDA + standalone HPA target the same deployment", func() {
		namespace := "test-unsupported"

		// Create all resources in envtest
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
				Annotations: map[string]string{
					namespacefilter.EnableEvictionAutoscalerAnnotationKey: annotationTrue,
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		// Create deployment
		surge := intstr.FromInt(1)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: dualTargetName, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: new(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: dualLabelValue}},
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabelKey: dualLabelValue}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: nginxContainerName, Image: nginxImage}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deploy)).To(Succeed())

		// Create PDB (same name as EA)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: dualEAName, Namespace: namespace},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &intstr.IntOrString{IntVal: 1},
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: dualLabelValue}},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

		// Create standalone HPA targeting the same deployment.
		// Note: the HPA must be created in envtest (which supports autoscaling/v2)
		// before the KEDA ScaledObject so that findHPAForTarget can find it.
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "dual-hpa", Namespace: namespace},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: ResourceTypeDeployment,
					Name: dualTargetName,
				},
				MinReplicas: new(int32(1)),
				MaxReplicas: 5,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		// Create KEDA ScaledObject targeting the deployment.
		// The envtest environment includes the KEDA CRD (registered in suite_test.go scheme).
		so := &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Name: "dual-so", Namespace: namespace},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef:  &kedav1alpha1.ScaleTarget{Name: dualTargetName, Kind: ResourceTypeDeployment},
				MinReplicaCount: new(int32(1)),
				MaxReplicaCount: new(int32(5)),
				Triggers: []kedav1alpha1.ScaleTriggers{{
					Type:     "cpu",
					Metadata: map[string]string{"type": "Utilization", testAnnotationValue: "50"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, so)).To(Succeed())

		// Create EvictionAutoScaler
		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: dualEAName, Namespace: namespace},
			Spec: v1.EvictionAutoScalerSpec{
				TargetName: dualTargetName,
				TargetKind: deploymentKind,
			},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		// Reconcile
		reconciler := &EvictionAutoScalerReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Filter: &evictionTestFilter{},
		}

		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: dualEAName, Namespace: namespace},
		})

		// Should NOT return an error (no requeue via error path)
		Expect(err).ToNot(HaveOccurred())
		// Should NOT request explicit requeue
		Expect(result.RequeueAfter).To(BeZero())

		// Should set Degraded condition
		var updated v1.EvictionAutoScaler
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: dualEAName, Namespace: namespace}, &updated)).To(Succeed())

		var degradedCondition *metav1.Condition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == "Degraded" {
				degradedCondition = &updated.Status.Conditions[i]
				break
			}
		}
		Expect(degradedCondition).ToNot(BeNil(), "expected Degraded condition to be set")
		Expect(degradedCondition.Status).To(Equal(metav1.ConditionTrue))
		Expect(degradedCondition.Reason).To(Equal("UnsupportedAutoscalerConfiguration"))
		Expect(degradedCondition.Message).To(ContainSubstring("KEDA ScaledObject"))
		Expect(degradedCondition.Message).To(ContainSubstring("standalone HPA"))
	})
})
