package controllers

import (
	"context"
	"fmt"
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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
					GenerateName: "test",
					Annotations: map[string]string{
						namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
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
					TargetKind: "deployment",
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
				PodName:      "somepod", //
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
			Expect(EvictionAutoScaler.Spec.LastEviction.PodName).To(Equal("somepod"))
			//we don't update status of last eviction till
			Expect(EvictionAutoScaler.Spec.LastEviction.EvictionTime).ToNot(Equal(EvictionAutoScaler.Status.LastEviction.EvictionTime))

			// No real pods exist on a cordoned node, so displaced=0 and no surge fires.
			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))
		})

		It("should surge by exactly displaced pod count when pods are on a cordoned node", func() {
			By("setting up a cordoned node and pods on it")
			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			// Create a node and cordon it.
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-cordoned-node-" + namespace},
				Spec:       corev1.NodeSpec{Unschedulable: true},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			// Create 2 pods on the cordoned node matching the PDB selector.
			for i := 0; i < 2; i++ {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "displaced-pod-",
						Namespace:    namespace,
						Labels:       map[string]string{"app": "example"},
					},
					Spec: corev1.PodSpec{
						NodeName: node.Name,
						Containers: []corev1.Container{
							{Name: "nginx", Image: "nginx:latest"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			}

			// Run once to populate TargetGeneration.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Log an eviction so the reconciler enters the surge path.
			ea := &v1.EvictionAutoScaler{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction = v1.Eviction{PodName: "displaced-pod", EvictionTime: metav1.Now()}
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Deployment should have surged to minReplicas(1) + displaced(2) = 3.
			// MaxSurge=1 on the deployment caps it at minReplicas(1) + maxSurge(1) = 2.
			// Since displaced(2) > maxSurge(1), surgeTarget is capped at 2.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
		})

		It("should surge by exactly displaced pod count when displaced is less than maxSurge", func() {
			By("setting up a cordoned node with fewer pods than maxSurge")
			// Increase maxSurge on the deployment to 5 so displaced(1) < maxSurge(5).
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			bigSurge := intstr.FromInt32(5)
			dep.Spec.Strategy.RollingUpdate.MaxSurge = &bigSurge
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())

			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			// Create a cordoned node with exactly 1 matching pod.
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-small-drain-node-" + namespace},
				Spec:       corev1.NodeSpec{Unschedulable: true},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "small-drain-pod-" + namespace,
					Namespace: namespace,
					Labels:    map[string]string{"app": "example"},
				},
				Spec: corev1.PodSpec{
					NodeName:   node.Name,
					Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			// Reconcile once to update TargetGeneration after the deployment change.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Log an eviction.
			ea := &v1.EvictionAutoScaler{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction = v1.Eviction{PodName: "small-drain-pod", EvictionTime: metav1.Now()}
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// displaced(1) < maxSurge(5), so surgeTarget = minReplicas(1) + displaced(1) = 2.
			// Not minReplicas(1) + maxSurge(5) = 6.
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
		})

		It("should top up surge incrementally as more nodes are cordoned", func() {
			// Verifies the "scale TO target" formula: surgeTarget = minReplicas + totalDisplaced.
			// Cordoning a second node should top up to the new total without double-counting.
			By("giving the deployment a generous maxSurge so it doesn't cap us")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			bigSurge := intstr.FromInt32(10)
			dep.Spec.Strategy.RollingUpdate.MaxSurge = &bigSurge
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())

			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			// Create two nodes, both cordoned; each has 1 pod.
			nodeA := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "incremental-node-a-" + namespace},
				Spec:       corev1.NodeSpec{Unschedulable: true},
			}
			nodeB := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "incremental-node-b-" + namespace},
				Spec:       corev1.NodeSpec{Unschedulable: false}, // start uncordoned
			}
			Expect(k8sClient.Create(ctx, nodeA)).To(Succeed())
			Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())

			podA := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "inc-pod-a-" + namespace,
					Namespace: namespace,
					Labels:    map[string]string{"app": "example"},
				},
				Spec: corev1.PodSpec{
					NodeName:   nodeA.Name,
					Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
				},
			}
			podB := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "inc-pod-b-" + namespace,
					Namespace: namespace,
					Labels:    map[string]string{"app": "example"},
				},
				Spec: corev1.PodSpec{
					NodeName:   nodeB.Name,
					Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
				},
			}
			Expect(k8sClient.Create(ctx, podA)).To(Succeed())
			Expect(k8sClient.Create(ctx, podB)).To(Succeed())

			// Reconcile once to populate TargetGeneration after the deployment maxSurge change.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// --- Wave 1: only node A is cordoned (1 displaced pod) ---
			ea := &v1.EvictionAutoScaler{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction = v1.Eviction{PodName: "inc-pod-a", EvictionTime: metav1.Now()}
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// surgeTarget = minReplicas(1) + displaced(1) = 2.
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)), "after first cordon: should surge to 2")

			// --- Wave 2: cordon node B as well (now 2 displaced pods total) ---
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeB.Name}, nodeB)).To(Succeed())
			nodeB.Spec.Unschedulable = true
			Expect(k8sClient.Update(ctx, nodeB)).To(Succeed())

			// New eviction triggers reconcile.
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction = v1.Eviction{PodName: "inc-pod-b", EvictionTime: metav1.Now()}
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// surgeTarget = minReplicas(1) + displaced(2) = 3. Tops up without double-counting.
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)), "after second cordon: should top up to 3")
		})

		It("should not scale down when some nodes are uncordoned but drain is still blocked", func() {
			// When DisruptionsAllowed is still 0 (drain ongoing), surgeTarget drops if displaced
			// drops, but we do NOT scale the deployment down — only the cooldown path does that.
			// This tests that we don't inadvertently evict pods we just brought up.
			By("giving the deployment a generous maxSurge")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			bigSurge := intstr.FromInt32(10)
			dep.Spec.Strategy.RollingUpdate.MaxSurge = &bigSurge
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())

			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			// Create two cordoned nodes each with 1 pod.
			nodeC := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "partial-uncordon-c-" + namespace},
				Spec:       corev1.NodeSpec{Unschedulable: true},
			}
			nodeD := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "partial-uncordon-d-" + namespace},
				Spec:       corev1.NodeSpec{Unschedulable: true},
			}
			Expect(k8sClient.Create(ctx, nodeC)).To(Succeed())
			Expect(k8sClient.Create(ctx, nodeD)).To(Succeed())

			for i, nodeName := range []string{nodeC.Name, nodeD.Name} {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "partial-pod-" + string(rune('c'+i)) + "-" + namespace,
						Namespace: namespace,
						Labels:    map[string]string{"app": "example"},
					},
					Spec: corev1.PodSpec{
						NodeName:   nodeName,
						Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
					},
				}
				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			}

			// Reconcile once to update TargetGeneration.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Trigger surge with both nodes cordoned: displaced=2, surgeTarget=3.
			ea := &v1.EvictionAutoScaler{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction = v1.Eviction{PodName: "partial-pod-c", EvictionTime: metav1.Now()}
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)), "surged to 3 with 2 displaced pods")

			// Now uncordon node D (only node C remains cordoned, displaced=1).
			// DisruptionsAllowed is still 0 (PDB still blocking — drain not done).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeD.Name}, nodeD)).To(Succeed())
			nodeD.Spec.Unschedulable = false
			Expect(k8sClient.Update(ctx, nodeD)).To(Succeed())

			// Reconcile again with the same unhandled eviction.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// surgeTarget is now 2, but GetReplicas()=3 >= 2, so no scale-up fires.
			// Scale-down does NOT happen here — it only fires via the cooldown path
			// once DisruptionsAllowed > 0. Replicas remain at 3.
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)), "no premature scale-down while drain is still blocked")
		})

		It("should surge incrementally across multiple nodes then scale back down when all drains complete", func() {
			// Full end-to-end multi-node drain scenario:
			//   1. Cordon node A (2 pods) → surge to minReplicas+2
			//   2. Cordon node B (2 more pods) → top up to minReplicas+4
			//   3. Both nodes drain; PDB unblocks; cooldown expires → scale down to minReplicas
			By("giving the deployment enough maxSurge for the scenario")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			bigSurge := intstr.FromInt32(10)
			dep.Spec.Strategy.RollingUpdate.MaxSurge = &bigSurge
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())

			controllerReconciler := &EvictionAutoScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Filter: &evictionTestFilter{},
			}

			// Two nodes, initially uncordoned; 2 pods each.
			nodeE := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-drain-e-" + namespace},
				Spec:       corev1.NodeSpec{Unschedulable: false},
			}
			nodeF := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-drain-f-" + namespace},
				Spec:       corev1.NodeSpec{Unschedulable: false},
			}
			Expect(k8sClient.Create(ctx, nodeE)).To(Succeed())
			Expect(k8sClient.Create(ctx, nodeF)).To(Succeed())

			for i := 0; i < 2; i++ {
				podE := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("multi-pod-e%d-%s", i, namespace),
						Namespace: namespace,
						Labels:    map[string]string{"app": "example"},
					},
					Spec: corev1.PodSpec{
						NodeName:   nodeE.Name,
						Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
					},
				}
				podF := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("multi-pod-f%d-%s", i, namespace),
						Namespace: namespace,
						Labels:    map[string]string{"app": "example"},
					},
					Spec: corev1.PodSpec{
						NodeName:   nodeF.Name,
						Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
					},
				}
				Expect(k8sClient.Create(ctx, podE)).To(Succeed())
				Expect(k8sClient.Create(ctx, podF)).To(Succeed())
			}

			// Seed TargetGeneration so future reconciles don't see a "deployment changed" reset.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// ── Wave 1: cordon node E ──────────────────────────────────────────────────────
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeE.Name}, nodeE)).To(Succeed())
			nodeE.Spec.Unschedulable = true
			Expect(k8sClient.Update(ctx, nodeE)).To(Succeed())

			ea := &v1.EvictionAutoScaler{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction = v1.Eviction{PodName: "multi-pod-e0", EvictionTime: metav1.Now()}
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// minReplicas(1) + displaced(2) = 3
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)), "wave 1: surged to 3")

			// ── Wave 2: cordon node F ──────────────────────────────────────────────────────
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeF.Name}, nodeF)).To(Succeed())
			nodeF.Spec.Unschedulable = true
			Expect(k8sClient.Update(ctx, nodeF)).To(Succeed())

			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction = v1.Eviction{PodName: "multi-pod-f0", EvictionTime: metav1.Now()}
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// minReplicas(1) + displaced(4) = 5
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(5)), "wave 2: topped up to 5")

			// ── Drain completes: remove pods from both cordoned nodes ─────────────────────
			// In a real cluster the scheduler rescheduled them; here we just delete them.
			podList := &corev1.PodList{}
			Expect(k8sClient.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{"app": "example"})).To(Succeed())
			for i := range podList.Items {
				pod := &podList.Items[i]
				if pod.Spec.NodeName == nodeE.Name || pod.Spec.NodeName == nodeF.Name {
					Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
				}
			}

			// PDB now unblocked (drain finished).
			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, pdb)).To(Succeed())
			pdb.Status.DisruptionsAllowed = 1
			Expect(k8sClient.Status().Update(ctx, pdb)).To(Succeed())

			// Advance TargetGeneration so the reconciler treats the current deployment as known.
			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Status.TargetGeneration = dep.Generation
			Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

			// First reconcile after drain: still within cooldown — should requeue but not scale.
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction.EvictionTime = metav1.Now()
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(cooldown), "should requeue during cooldown")

			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(5)), "no scale-down yet: still within cooldown")

			// ── Cooldown expires ──────────────────────────────────────────────────────────
			Expect(k8sClient.Get(ctx, typeNamespacedName, ea)).To(Succeed())
			ea.Spec.LastEviction.EvictionTime = metav1.NewTime(time.Now().Add(-2 * cooldown))
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, deploymentNamespacedName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)), "post-drain: scaled back down to minReplicas")
		})

		It("should skip StatefulSet targets without surging", func() {

			By("creating a StatefulSet resource")
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      statefulSetName,
					Namespace: namespace,
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "example",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "example",
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
				PodName:      "somepod",
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
			deployment.Spec.Replicas = int32Ptr(2)
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			// Log an eviction
			EvictionAutoScaler := &v1.EvictionAutoScaler{}
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.LastEviction = v1.Eviction{
				PodName:      "somepod", //
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
			Expect(EvictionAutoScaler.Spec.LastEviction.PodName).To(Equal("somepod"))
			Expect(EvictionAutoScaler.Spec.LastEviction.EvictionTime).ToNot(Equal(EvictionAutoScaler.Status.LastEviction.EvictionTime))

			By("scaling down after cooldown")
			//okay lets say the eviction is older though
			//TODO make cooldown const/configurable
			EvictionAutoScaler.Spec.LastEviction.EvictionTime = metav1.NewTime(time.Now().Add(-2 * cooldown))
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())
			Expect(EvictionAutoScaler.Spec.LastEviction.EvictionTime).ToNot(Equal(EvictionAutoScaler.Status.LastEviction.EvictionTime))

			// All surged pods are now ready — the new retry logic only reverts when
			// readyReplicas >= desiredReplicas (or max attempts exhausted).
			err = k8sClient.Get(ctx, deploymentNamespacedName, deployment)
			Expect(err).NotTo(HaveOccurred())
			deployment.Status.Replicas = 2
			deployment.Status.ReadyReplicas = 2
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

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
			Expect(EvictionAutoScaler.Spec.LastEviction.PodName).To(Equal("somepod"))
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
			deployment.Spec.Replicas = int32Ptr(5)
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
					GenerateName: "test",
					Annotations: map[string]string{
						namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
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
					TargetKind: "deployment",
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
					TargetKind: "deployment",
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
					TargetKind: "deployment",
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
					Name:      "test-deploy",
					Namespace: testNamespace,
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
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Create PDB
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pdb",
					Namespace: testNamespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{IntVal: 2},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
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
					Name:      "test-pdb",
					Namespace: testNamespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: "test-deploy",
					TargetKind: "deployment",
					LastEviction: v1.Eviction{
						PodName:      "test-pod",
						EvictionTime: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			// Reconcile should skip processing
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-pdb", Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify deployment was NOT scaled (should still be 2)
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-deploy", Namespace: testNamespace}, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))
		})

		It("should process EvictionAutoScaler in namespace with annotation", func() {
			// Create a namespace with annotation
			namespaceObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-with-anno-",
					Annotations: map[string]string{
						namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
			testNamespace := namespaceObj.Name

			// Create deployment
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deploy",
					Namespace: testNamespace,
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
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Create PDB
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pdb",
					Namespace: testNamespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{IntVal: 2},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
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
					Name:      "test-pdb",
					Namespace: testNamespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: "test-deploy",
					TargetKind: "deployment",
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			// First reconcile to set up
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-pdb", Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Add eviction
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-pdb", Namespace: testNamespace}, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.LastEviction = v1.Eviction{
				PodName:      "test-pod",
				EvictionTime: metav1.Now(),
			}
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())

			// Second reconcile should scale up
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-pdb", Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// No real pods exist on a cordoned node, so displaced=0 and no surge fires.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-deploy", Namespace: testNamespace}, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))
		})

		It("should process EvictionAutoScaler in kube-system by default", func() {
			// Use kube-system namespace (no annotation needed)
			testNamespace := metav1.NamespaceSystem

			// Create deployment
			kubeSurge := intstr.FromInt(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-kube-deploy",
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(2),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test-kube"},
					},
					Strategy: appsv1.DeploymentStrategy{
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxSurge: &kubeSurge,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test-kube"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "nginx", Image: "nginx:latest"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Create PDB
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-kube-pdb",
					Namespace: testNamespace,
				},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{IntVal: 2},
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test-kube"},
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
					Name:      "test-kube-pdb",
					Namespace: testNamespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: "test-kube-deploy",
					TargetKind: "deployment",
				},
			}
			Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())

			// First reconcile to set up
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-kube-pdb", Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Add eviction
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-kube-pdb", Namespace: testNamespace}, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			EvictionAutoScaler.Spec.LastEviction = v1.Eviction{
				PodName:      "test-kube-pod",
				EvictionTime: metav1.Now(),
			}
			Expect(k8sClient.Update(ctx, EvictionAutoScaler)).To(Succeed())

			// Second reconcile should scale up
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-kube-pdb", Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// No real pods exist on a cordoned node, so displaced=0 and no surge fires.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-kube-deploy", Namespace: testNamespace}, deployment)
			Expect(err).NotTo(HaveOccurred())
			Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))

			// Cleanup
			Expect(k8sClient.Delete(ctx, EvictionAutoScaler)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pdb)).To(Succeed())
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})
})

func int32Ptr(i int32) *int32 {
	return &i
}

var _ = Describe("EvictionAutoScaler Controller - surge retry behavior", func() {
	ctx := context.Background()

	var (
		surgeNs         string
		eaNsName        types.NamespacedName
		deployNsName    types.NamespacedName
		surgeReconciler *EvictionAutoScalerReconciler
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-surge-",
				Annotations: map[string]string{
					namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		surgeNs = ns.Name
		eaNsName = types.NamespacedName{Name: "surge-ea", Namespace: surgeNs}
		deployNsName = types.NamespacedName{Name: "surge-deploy", Namespace: surgeNs}

		// Create a deployment that is already in the surged state (spec.replicas=2, surge annotation present).
		surge := intstr.FromInt(1)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "surge-deploy",
				Namespace: surgeNs,
				Annotations: map[string]string{
					EvictionSurgeReplicasAnnotationKey: "2",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(2),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "surge-test"}},
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "surge-test"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "surge-ea", Namespace: surgeNs},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &intstr.IntOrString{IntVal: 1},
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "surge-test"}},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		// DisruptionsAllowed > 0 so the reconciler skips the scale-up path and goes straight to revert logic.
		pdb.Status.DisruptionsAllowed = 1
		Expect(k8sClient.Status().Update(ctx, pdb)).To(Succeed())

		// Create EA with a past eviction (beyond cooldown) so the cooldown gate is already open.
		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: "surge-ea", Namespace: surgeNs},
			Spec: v1.EvictionAutoScalerSpec{
				TargetName: "surge-deploy",
				TargetKind: "deployment",
				LastEviction: v1.Eviction{
					PodName:      "evicted-pod",
					EvictionTime: metav1.NewTime(time.Now().Add(-2 * cooldown)),
				},
			},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		// Set EA status to reflect the surged state: minReplicas=1, attempt 1 already recorded.
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		ea.Status.MinReplicas = 1
		ea.Status.TargetGeneration = dep.Generation
		ea.Status.SurgeAttempts = 1
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		surgeReconciler = &EvictionAutoScalerReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Filter: &evictionTestFilter{},
		}
	})

	It("requeues and increments SurgeAttempts when pods are not ready and under max attempts", func() {
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		dep.Status.Replicas = 2
		dep.Status.ReadyReplicas = 1 // 1 of 2 desired pods ready
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		result, err := surgeReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaNsName})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(cooldown))

		// Deployment must NOT be reverted.
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(2)))

		// SurgeAttempts must be incremented.
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaNsName, ea)).To(Succeed())
		Expect(ea.Status.SurgeAttempts).To(Equal(int32(2)))
	})

	It("reverts immediately when all surged pods are ready before max attempts", func() {
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		dep.Status.Replicas = 2
		dep.Status.ReadyReplicas = 2 // all desired pods ready
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		result, err := surgeReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaNsName})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

		// Deployment must be reverted to the original minReplicas.
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))

		// SurgeAttempts must be reset.
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaNsName, ea)).To(Succeed())
		Expect(ea.Status.SurgeAttempts).To(Equal(int32(0)))
	})

	It("gives up and reverts after max surge attempts when pods remain not ready", func() {
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		dep.Status.ReadyReplicas = 0 // pods still not ready
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		// Advance SurgeAttempts to the default maximum.
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaNsName, ea)).To(Succeed())
		ea.Status.SurgeAttempts = defaultTimeToReadyMinutes
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		result, err := surgeReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaNsName})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

		// Deployment must be reverted despite pods not being ready.
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))

		Expect(k8sClient.Get(ctx, eaNsName, ea)).To(Succeed())
		Expect(ea.Status.SurgeAttempts).To(Equal(int32(0)))
	})

	It("uses the time-to-ready annotation on the deployment to determine max attempts", func() {
		// Annotate the deployment with time-to-ready=2 → max 2 attempts.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		dep.Annotations[TimeToReadyAnnotationKey] = "2"
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed()) // refresh RV
		dep.Status.Replicas = 2
		dep.Status.ReadyReplicas = 0
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		// SurgeAttempts is at the annotation-based max (2).
		// Re-sync TargetGeneration in case annotating the deployment bumped the generation.
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaNsName, ea)).To(Succeed())
		ea.Status.SurgeAttempts = 2
		ea.Status.TargetGeneration = dep.Generation
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		result, err := surgeReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaNsName})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
	})

	It("keeps waiting when time-to-ready annotation allows more attempts", func() {
		// Annotate the deployment with time-to-ready=10 → max 10 attempts.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		dep.Annotations[TimeToReadyAnnotationKey] = "10"
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		dep.Status.Replicas = 2
		dep.Status.ReadyReplicas = 1 // still not fully ready
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		// SurgeAttempts is well under the annotation-based max.
		// Re-sync TargetGeneration in case annotating the deployment bumped the generation.
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, eaNsName, ea)).To(Succeed())
		ea.Status.SurgeAttempts = 3
		ea.Status.TargetGeneration = dep.Generation
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		result, err := surgeReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaNsName})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(cooldown))

		// Deployment must NOT be reverted.
		Expect(k8sClient.Get(ctx, deployNsName, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(2)))

		Expect(k8sClient.Get(ctx, eaNsName, ea)).To(Succeed())
		Expect(ea.Status.SurgeAttempts).To(Equal(int32(4)))
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
					namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		// Create deployment
		surge := intstr.FromInt(1)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "dual-target", Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dual"}},
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "dual"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deploy)).To(Succeed())

		// Create PDB (same name as EA)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "dual-ea", Namespace: namespace},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &intstr.IntOrString{IntVal: 1},
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dual"}},
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
					Kind: "Deployment",
					Name: "dual-target",
				},
				MinReplicas: ptr.To(int32(1)),
				MaxReplicas: 5,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		// Create KEDA ScaledObject targeting the deployment.
		// The envtest environment includes the KEDA CRD (registered in suite_test.go scheme).
		so := &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Name: "dual-so", Namespace: namespace},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef:  &kedav1alpha1.ScaleTarget{Name: "dual-target", Kind: "Deployment"},
				MinReplicaCount: ptr.To(int32(1)),
				MaxReplicaCount: ptr.To(int32(5)),
				Triggers: []kedav1alpha1.ScaleTriggers{{
					Type:     "cpu",
					Metadata: map[string]string{"type": "Utilization", "value": "50"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, so)).To(Succeed())

		// Create EvictionAutoScaler
		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: "dual-ea", Namespace: namespace},
			Spec: v1.EvictionAutoScalerSpec{
				TargetName: "dual-target",
				TargetKind: "deployment",
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
			NamespacedName: types.NamespacedName{Name: "dual-ea", Namespace: namespace},
		})

		// Should NOT return an error (no requeue via error path)
		Expect(err).ToNot(HaveOccurred())
		// Should NOT request explicit requeue
		Expect(result.RequeueAfter).To(BeZero())

		// Should set Degraded condition
		var updated v1.EvictionAutoScaler
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "dual-ea", Namespace: namespace}, &updated)).To(Succeed())

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

var _ = Describe("EvictionAutoScaler Controller - invalid time-to-ready annotation", func() {
	ctx := context.Background()

	// buildInvalidAnnotationScenario creates all necessary resources and returns a
	// ready-to-use reconciler plus the EA namespaced name.
	buildScenario := func(annotationValue string) (*EvictionAutoScalerReconciler, types.NamespacedName) {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ttr-invalid-",
				Annotations:  map[string]string{"eviction-autoscaler.azure.com/enable": "true"},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace := ns.Name

		surge := intstr.FromInt(1)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ttr-deploy",
				Namespace: namespace,
				Annotations: map[string]string{
					TimeToReadyAnnotationKey: annotationValue,
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ttr"}},
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ttr"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "ttr-ea", Namespace: namespace},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &intstr.IntOrString{IntVal: 1},
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ttr"}},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: "ttr-ea", Namespace: namespace},
			Spec: v1.EvictionAutoScalerSpec{
				TargetName: "ttr-deploy",
				TargetKind: "deployment",
			},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		reconciler := &EvictionAutoScalerReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Filter: &evictionTestFilter{},
		}
		eaNsName := types.NamespacedName{Name: "ttr-ea", Namespace: namespace}
		return reconciler, eaNsName
	}

	checkDegraded := func(eaNsName types.NamespacedName) {
		var updated v1.EvictionAutoScaler
		Expect(k8sClient.Get(ctx, eaNsName, &updated)).To(Succeed())
		var cond *metav1.Condition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == "Degraded" {
				cond = &updated.Status.Conditions[i]
				break
			}
		}
		Expect(cond).ToNot(BeNil(), "expected Degraded condition")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("InvalidTimeToReadyAnnotation"))
		Expect(cond.Message).To(ContainSubstring(TimeToReadyAnnotationKey))
	}

	It("marks EA degraded when time-to-ready is above the maximum (11)", func() {
		reconciler, eaNsName := buildScenario("11")
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaNsName})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		checkDegraded(eaNsName)
	})

	It("marks EA degraded when time-to-ready is 0", func() {
		reconciler, eaNsName := buildScenario("0")
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaNsName})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		checkDegraded(eaNsName)
	})

	It("marks EA degraded when time-to-ready is not a number", func() {
		reconciler, eaNsName := buildScenario("fast")
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: eaNsName})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		checkDegraded(eaNsName)
	})
})
