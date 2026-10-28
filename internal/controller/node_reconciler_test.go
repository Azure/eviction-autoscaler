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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1" // Import corev1 package
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
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
					Annotations: map[string]string{
						namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
					},
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

		reconcileCordonedNode := func(intercepts interceptor.Funcs) (reconcile.Result, error) {
			node := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeNamespacedName, node)).To(Succeed())
			node.Spec.Unschedulable = true
			Expect(k8sClient.Update(ctx, node)).To(Succeed())
			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			reconciler := &NodeReconciler{
				Client: interceptor.NewClient(watchClient, intercepts),
				Scheme: k8sClient.Scheme(),
			}
			return reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nodeNamespacedName})
		}

		It("lists EAS once per namespace and reuses updated resource versions for matching Pods", func() {
			otherNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "node-other-"}}
			Expect(k8sClient.Create(ctx, otherNamespace)).To(Succeed())
			Expect(k8sClient.Create(ctx, &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: otherNamespace.Name},
				Spec:       v1.EvictionAutoScalerSpec{TargetName: "other-target", TargetKind: "deployment"},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: otherNamespace.Name},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &intstr.IntOrString{IntVal: 1},
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "example"}},
				},
			})).To(Succeed())
			for _, podNamespace := range []string{namespace, otherNamespace.Name} {
				Expect(k8sClient.Create(ctx, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "extra-pod", Namespace: podNamespace, Labels: map[string]string{"app": "example"},
					},
					Spec: corev1.PodSpec{
						NodeName: nodeName, Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
					},
				})).To(Succeed())
			}

			listCalls := make(map[string]int)
			updateVersions := make(map[string][]string)
			lastPodByNamespace := make(map[string]string)
			podListCalls := 0
			firstNamespace := ""
			result, err := reconcileCordonedNode(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					switch list := list.(type) {
					case *v1.EvictionAutoScalerList:
						options := &client.ListOptions{}
						for _, opt := range opts {
							opt.ApplyToList(options)
						}
						if options.Namespace != firstNamespace {
							Expect(updateVersions[firstNamespace]).NotTo(BeEmpty(),
								"process the first namespace before loading the next one")
						}
						listCalls[options.Namespace]++
					case *corev1.PodList:
						podListCalls++
						firstNamespace = list.Items[0].Namespace
						for _, pod := range list.Items {
							lastPodByNamespace[pod.Namespace] = pod.Name
						}
					}
					return nil
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*v1.EvictionAutoScaler); ok {
						updateVersions[obj.GetNamespace()] = append(updateVersions[obj.GetNamespace()], obj.GetResourceVersion())
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(cooldown))
			Expect(podListCalls).To(Equal(1))
			Expect(listCalls).To(Equal(map[string]int{namespace: 1, otherNamespace.Name: 1}))
			Expect(updateVersions[namespace]).To(HaveLen(2))
			Expect(updateVersions[namespace][0]).NotTo(Equal(updateVersions[namespace][1]))
			Expect(updateVersions[otherNamespace.Name]).To(HaveLen(1))
			for _, podNamespace := range []string{namespace, otherNamespace.Name} {
				var autoscaler v1.EvictionAutoScaler
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: resourceName, Namespace: podNamespace,
				}, &autoscaler)).To(Succeed())
				Expect(autoscaler.Spec.LastEviction.PodName).To(Equal(lastPodByNamespace[podNamespace]))
			}
		})

		It("lists an empty EAS namespace only once for multiple Pods", func() {
			Expect(k8sClient.Delete(ctx, &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "extra-pod", Namespace: namespace},
				Spec: corev1.PodSpec{
					NodeName: nodeName, Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
				},
			})).To(Succeed())
			listCalls := 0
			result, err := reconcileCordonedNode(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*v1.EvictionAutoScalerList); ok {
						listCalls++
					}
					return c.List(ctx, list, opts...)
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(listCalls).To(Equal(1))
		})

		It("returns EAS list failures without updating Pod status", func() {
			listErr := errors.NewServiceUnavailable("test EAS list failure")
			_, err := reconcileCordonedNode(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*v1.EvictionAutoScalerList); ok {
						return listErr
					}
					return c.List(ctx, list, opts...)
				},
			})
			Expect(err).To(MatchError(listErr))
			var pod corev1.Pod
			Expect(k8sClient.Get(ctx, podNamespacedName, &pod)).To(Succeed())
			Expect(pod.Status.Conditions).To(HaveLen(1))
			Expect(pod.Status.Conditions[0].Type).To(Equal(corev1.PodReady))
		})

		It("returns a later namespace list failure after preserving earlier Pod processing", func() {
			otherNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "node-list-error-"}}
			Expect(k8sClient.Create(ctx, otherNamespace)).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "other-pod", Namespace: otherNamespace.Name},
				Spec: corev1.PodSpec{
					NodeName: nodeName, Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
				},
			})).To(Succeed())
			listErr := errors.NewServiceUnavailable("test later namespace list failure")
			_, err := reconcileCordonedNode(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*v1.EvictionAutoScalerList); ok {
						options := &client.ListOptions{}
						for _, opt := range opts {
							opt.ApplyToList(options)
						}
						if options.Namespace == otherNamespace.Name {
							return listErr
						}
					}
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if pods, ok := list.(*corev1.PodList); ok {
						Expect(pods.Items).To(HaveLen(2))
						if pods.Items[0].Namespace != namespace {
							pods.Items[0], pods.Items[1] = pods.Items[1], pods.Items[0]
						}
					}
					return nil
				},
			})
			Expect(err).To(MatchError(listErr))
			var pod corev1.Pod
			Expect(k8sClient.Get(ctx, podNamespacedName, &pod)).To(Succeed())
			Expect(pod.Status.Conditions).To(ContainElement(HaveField("Type", corev1.DisruptionTarget)))
			var autoscaler v1.EvictionAutoScaler
			Expect(k8sClient.Get(ctx, typeNamespacedName, &autoscaler)).To(Succeed())
			Expect(autoscaler.Spec.LastEviction.PodName).To(Equal(podName))
			Expect(autoscaler.Spec.LastEviction.EvictionTime.IsZero()).To(BeFalse())
		})
	})
})
