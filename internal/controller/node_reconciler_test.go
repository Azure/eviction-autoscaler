package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1" // Import corev1 package
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
)

var _ = Describe("Node Controller", func() {
	const resourceName = "test-resource"
	const podName = "example-pod"
	ctx := context.Background()
	var namespace, nodeName string
	var typeNamespacedName, podNamespacedName, nodeNamespacedName types.NamespacedName

	Context("When reconciling a node resource", func() {

		BeforeEach(func() {
			namespaceObj := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test",
				},
			}

			// create the namespace using the controller-runtime client
			Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())
			namespace = namespaceObj.Name
			typeNamespacedName = types.NamespacedName{Name: resourceName, Namespace: namespace}
			podNamespacedName = types.NamespacedName{Name: podName, Namespace: namespace}

			nodeName = rand.String(8)
			nodeNamespacedName = types.NamespacedName{Name: nodeName}

			By("creating the custom resource for the Kind EvictionAutoScaler")
			EvictionAutoScaler := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.EvictionAutoScalerSpec{
					TargetName: "exmple-whatever",
					TargetKind: "deployment",
				},
			}
			err := k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, EvictionAutoScaler)).To(Succeed())
			}

			By("creating a pod resource")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: namespace,
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
					NodeName: nodeName,
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
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			By("creating a PDB resource")
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{
					Name:       resourceName,
					Namespace:  namespace,
					Generation: 1,
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
			}
			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
			pdb.Status = policyv1.PodDisruptionBudgetStatus{
				DisruptionsAllowed: 0,
				CurrentHealthy:     1,
				DesiredHealthy:     1,
				ExpectedPods:       1,
				ObservedGeneration: 1,
			}
			Expect(k8sClient.Status().Update(ctx, pdb)).To(Succeed())

			By("creating a Node resource")
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: corev1.NodeSpec{},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

		})

		It("should handle cordon by updating pod and EvictionAutoScaler", func() {
			nodeReconciler := &NodeReconciler{
				Client: k8sClient,
				Scheme: scheme.Scheme,
			}
			_, err := nodeReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: nodeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			node := &corev1.Node{}
			err = k8sClient.Get(ctx, nodeNamespacedName, node)
			Expect(err).NotTo(HaveOccurred())
			node.Spec.Unschedulable = true

			err = k8sClient.Update(ctx, node)
			Expect(err).NotTo(HaveOccurred())
			result, err := nodeReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: nodeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(cooldown))

			By("checking pod condition ")
			pod := &corev1.Pod{}
			err = k8sClient.Get(ctx, podNamespacedName, pod)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.Status.Conditions).To(HaveLen(2))
			//First is still ready ignore it
			Expect(pod.Status.Conditions[1].Type).To(Equal(corev1.DisruptionTarget))

			By("updating pdb watcher ")
			EvictionAutoScaler := &v1.EvictionAutoScaler{}
			err = k8sClient.Get(ctx, typeNamespacedName, EvictionAutoScaler)
			Expect(err).NotTo(HaveOccurred())
			Expect(EvictionAutoScaler.Spec.LastEviction.EvictionTime).ToNot(BeZero())
			Expect(EvictionAutoScaler.Spec.LastEviction.PodName).To(Equal(podName))

		})

		It("should handle cordon with no targetable pod", func() {
			nodeReconciler := &NodeReconciler{
				Client: k8sClient,
				Scheme: scheme.Scheme,
			}
			_, err := nodeReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: nodeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			err = k8sClient.Get(ctx, podNamespacedName, pod)
			Expect(err).NotTo(HaveOccurred())
			pod.Labels = map[string]string{
				"app": "notexample",
			}
			err = k8sClient.Update(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			node := &corev1.Node{}
			err = k8sClient.Get(ctx, nodeNamespacedName, node)
			Expect(err).NotTo(HaveOccurred())
			node.Spec.Unschedulable = true

			err = k8sClient.Update(ctx, node)
			Expect(err).NotTo(HaveOccurred())
			result, err := nodeReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: nodeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("checking pod condition ")
			err = k8sClient.Get(ctx, podNamespacedName, pod)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.Status.Conditions).To(HaveLen(1))
			//First is still ready ignore it
			Expect(pod.Status.Conditions[0].Type).To(Equal(corev1.PodReady))

		})
	})
})
