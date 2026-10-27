package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
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
		})
		Expect(err).NotTo(HaveOccurred())
		reconciler := &NodeReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), PodCacheEnabled: enabled,
		}
		Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

		if !enabled {
			err := mgr.GetCache().List(ctx, &corev1.PodList{})
			Expect(err).To(BeAssignableToTypeOf(&cache.ErrResourceNotCached{}))
			return
		}

		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "node-cache-"}}
		otherNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "node-cache-other-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		Expect(k8sClient.Create(ctx, otherNamespace)).To(Succeed())
		targetNode := namespace.Name
		for _, pod := range []*corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: namespace.Name},
				Spec: corev1.PodSpec{
					NodeName: targetNode, Containers: []corev1.Container{{Name: "app", Image: "test"}},
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
	},
		Entry("direct API reads", false),
		Entry("cached reads", true),
	)
})
