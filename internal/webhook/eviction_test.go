package webhook

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1" // Import corev1 package
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
)

var _ = Describe("Evictions webhook", func() {
	const resourceName = "test-resource"
	const namespace = "default"
	const podName = "example-pod"

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: namespace}
	podNamespacedName := types.NamespacedName{Name: podName, Namespace: namespace}

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
		})

		AfterEach(func() {
			By("cleaning up resources")
			deleteResource := func(obj client.Object) {
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
				Eventually(func() bool {
					err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
					return errors.IsNotFound(err)
				}, time.Second*10, time.Millisecond*250).Should(BeTrue())
			}

			deleteResource(&v1.PDBWatcher{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace}})
			deleteResource(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace}})
			deleteResource(&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace}})
		})

		It("should handle an eviction", func() {

			By("checking pod  start ")
			pod := &corev1.Pod{}
			err := k8sClient.Get(ctx, podNamespacedName, pod)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.Status.Conditions).To(HaveLen(1))
			Expect(pod.Status.Conditions[0].Type).To(Equal(corev1.PodReady))

			clientset, err := kubernetes.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())

			err = clientset.PolicyV1().Evictions(namespace).Evict(ctx, &policyv1.Eviction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: namespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsTooManyRequests(err)).To(BeTrue())
			By("updating pdb watcher ")

			pdbwatcher := &v1.PDBWatcher{}
			err = k8sClient.Get(ctx, typeNamespacedName, pdbwatcher)
			Expect(err).NotTo(HaveOccurred())
			Expect(pdbwatcher.Spec.LastEviction.EvictionTime).ToNot(BeZero())
			Expect(pdbwatcher.Spec.LastEviction.PodName).To(Equal(podName))

			By("checking pod condition ")

			err = k8sClient.Get(ctx, podNamespacedName, pod)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.Status.Conditions).To(HaveLen(2))
			//First is still ready ignore it
			Expect(pod.Status.Conditions[1].Type).To(Equal(corev1.DisruptionTarget))

		})
	})
})
