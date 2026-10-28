package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var _ = Describe("Node Pod cache configuration", func() {
	DescribeTable("registers the node-name index only in cached mode", func(enabled bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		DeferCleanup(cancel)

		skipNameValidation := true
		options := ctrl.Options{
			Scheme: k8sClient.Scheme(),
			Cache: cache.Options{
				ReaderFailOnMissingInformer: true,
				ByObject: map[client.Object]cache.ByObject{
					&corev1.Pod{}: {Transform: TransformPodCache},
				},
			},
			Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		}
		if !enabled {
			options.Client.Cache = &client.CacheOptions{DisableFor: []client.Object{&corev1.Pod{}}}
		}
		mgr, err := ctrl.NewManager(cfg, options)
		Expect(err).NotTo(HaveOccurred())
		reconciler := &NodeReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), PodCacheEnabled: enabled,
		}
		Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

		if !enabled {
			var pods corev1.PodList
			Expect(mgr.GetClient().List(ctx, &pods, client.MatchingFields{
				NodeNameIndex: "uncached-test-node",
			})).To(Succeed())
			Expect(mgr.GetClient().List(ctx, &pods, client.InNamespace("default"),
				client.MatchingLabels{"app": "uncached-test"})).To(Succeed())
			// A successful client read must not have created a Pod informer.
			err := mgr.GetCache().List(ctx, &corev1.PodList{})
			Expect(err).To(BeAssignableToTypeOf(&cache.ErrResourceNotCached{}))
			return
		}

		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "node-cache-"}}
		otherNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "node-cache-other-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		Expect(k8sClient.Create(ctx, otherNamespace)).To(Succeed())
		targetNode := namespace.Name
		var sourcePod *corev1.Pod
		for _, pod := range []*corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "target", Namespace: namespace.Name,
					Labels:      map[string]string{"app": "cache-test"},
					Annotations: map[string]string{"cache-test": "preserve-on-server"},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "cache-owner", UID: "cache-owner",
					}},
				},
				Spec: corev1.PodSpec{
					NodeName: targetNode, Containers: []corev1.Container{{
						Name: "app", Image: "test",
						Env: []corev1.EnvVar{{Name: "EXAMPLE", Value: "preserve-on-server"}},
					}},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "other-namespace", Namespace: otherNamespace.Name},
				Spec: corev1.PodSpec{
					NodeName: targetNode, Containers: []corev1.Container{{Name: "app", Image: "test"}},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "other-node", Namespace: namespace.Name},
				Spec: corev1.PodSpec{
					NodeName: "other-node", Containers: []corev1.Container{{Name: "app", Image: "test"}},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "unscheduled", Namespace: namespace.Name},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "test"}}},
			},
		} {
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			if pod.Name == "target" {
				pod.Status = corev1.PodStatus{
					Phase: corev1.PodRunning, PodIP: "10.0.0.1", HostIP: "192.168.0.1",
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
					ContainerStatuses: []corev1.ContainerStatus{{
						Name: "app", Image: "test", ImageID: "test-image", Ready: true, RestartCount: 3,
						State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					}},
				}
				Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
				sourcePod = pod
			}
		}

		cacheCtx, stopCache := context.WithCancel(ctx)
		cacheDone := make(chan error, 1)
		go func() {
			cacheDone <- mgr.GetCache().Start(cacheCtx)
		}()
		DeferCleanup(func() {
			stopCache()
			Eventually(cacheDone).Should(Receive(BeNil()))
		})
		Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())

		var pods corev1.PodList
		Expect(mgr.GetClient().List(ctx, &pods, client.MatchingFields{NodeNameIndex: targetNode})).To(Succeed())
		Expect(pods.Items).To(HaveLen(2))
		names := make([]string, 0, len(pods.Items))
		for _, pod := range pods.Items {
			names = append(names, pod.Name)
			Expect(pod.Spec.NodeName).To(Equal(targetNode))
			Expect(pod.Spec.Containers).To(BeEmpty())
		}
		Expect(names).To(ConsistOf("target", "other-namespace"))

		var cachedPod corev1.Pod
		Expect(mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(sourcePod), &cachedPod)).To(Succeed())
		Expect(cachedPod.Status).To(Equal(sourcePod.Status))
		Expect(cachedPod.Labels).To(Equal(sourcePod.Labels))
		Expect(cachedPod.OwnerReferences).To(Equal(sourcePod.OwnerReferences))
		Expect(cachedPod.Annotations).To(BeEmpty())

		var matchingPods corev1.PodList
		Expect(mgr.GetClient().List(ctx, &matchingPods, client.InNamespace(namespace.Name),
			client.MatchingLabels{"app": "cache-test"})).To(Succeed())
		Expect(matchingPods.Items).To(HaveLen(1))

		Expect(reconciler.markPodDisruptionTarget(ctx, &cachedPod)).To(Succeed())

		var storedPod corev1.Pod
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sourcePod), &storedPod)).To(Succeed())
		Expect(storedPod.Spec).To(Equal(sourcePod.Spec))
		Expect(storedPod.Annotations).To(Equal(sourcePod.Annotations))
		Expect(storedPod.Status.Conditions).To(ContainElement(HaveField("Type", corev1.DisruptionTarget)))
		conditions := make([]corev1.PodCondition, 0, len(storedPod.Status.Conditions))
		for _, condition := range storedPod.Status.Conditions {
			if condition.Type != corev1.DisruptionTarget {
				conditions = append(conditions, condition)
			}
		}
		storedPod.Status.Conditions = conditions
		Expect(storedPod.Status).To(Equal(sourcePod.Status))
		// The original snapshot is now stale: a second patch must conflict.
		Expect(apierrors.IsConflict(reconciler.markPodDisruptionTarget(ctx, &cachedPod))).To(BeTrue())
	},
		Entry("direct API reads", false),
		Entry("cached reads", true),
	)
})
