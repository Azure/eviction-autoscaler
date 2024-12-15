package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1" // Import corev1 package
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
)

var _ = Describe("Node Controller", func() {
	const resourceName = "test-resource"
	const namespace = "default"
	const podName = "example-pod"
	const nodeName = "mynode"

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: namespace}
	podNamespacedName := types.NamespacedName{Name: podName, Namespace: namespace}
	nodeNamespacedName := types.NamespacedName{Name: nodeName}

	Context("When reconciling a resource", func() {

		BeforeEach(func() {
			By("creating the custom resource for the Kind PDBWatcher")
			pdbwatcher := &v1.PDBWatcher{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: v1.PDBWatcherSpec{
					TargetName: "exmple-whatever",
					TargetKind: "deployment",
				},
			}
			err := k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, pdbwatcher)).To(Succeed())
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

		AfterEach(func() {
			By("cleaning up resources")
			deleteResource := func(obj client.Object) {
				//no clue why we need to set GracePeriodSeconds 0 here but not in eviction_test.go
				//if we don't set it we get a
				Expect(k8sClient.Delete(ctx, obj, &client.DeleteOptions{GracePeriodSeconds: int64Ptr(0)})).To(Succeed())
				Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
					return errors.IsNotFound(err)
				}, time.Second*10, time.Millisecond*250).Should(BeTrue(), "Failed to delete resource "+obj.GetName())
			}

			deleteResource(&v1.PDBWatcher{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace}})
			deleteResource(&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace}})
			deleteResource(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace}})
			deleteResource(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})
		})

		It("should handle cordon by updating pod and pdbwatcher", func() {
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

			By("updating pdb watcher ")
			pdbwatcher := &v1.PDBWatcher{}
			err = k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			Expect(err).NotTo(HaveOccurred())
			Expect(pdbwatcher.Spec.LastEviction.EvictionTime).ToNot(BeZero())
			Expect(pdbwatcher.Spec.LastEviction.PodName).To(Equal(podName))

			By("checking pod condition ")

			pod := &corev1.Pod{}
			err = k8sClient.Get(ctx, podNamespacedName, pod)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.Status.Conditions).To(HaveLen(2))
			//First is still ready ignore it
			Expect(pod.Status.Conditions[1].Type).To(Equal(corev1.DisruptionTarget))

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

func int64Ptr(i int64) *int64 {
	return &i
}
