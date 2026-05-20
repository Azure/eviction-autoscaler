package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("countPodsOnCordoned", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(policyv1.AddToScheme(scheme)).To(Succeed())
	})

	makePDB := func(selector map[string]string) *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pdb", Namespace: "default"},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: selector},
			},
		}
	}

	makeNode := func(name string, cordoned bool) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		}
	}

	makePod := func(name, nodeName string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
		}
	}

	It("returns 0 when no pods match the PDB selector", func() {
		pdb := makePDB(map[string]string{"app": "myapp"})
		node := makeNode("node1", true) // cordoned, but no matching pods
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

		count, err := countPodsOnCordoned(ctx, fc, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int32(0)))
	})

	It("returns 0 when matching pods are on uncordoned nodes", func() {
		pdb := makePDB(map[string]string{"app": "myapp"})
		node := makeNode("node1", false) // not cordoned
		pod := makePod("pod1", "node1", map[string]string{"app": "myapp"})
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod).Build()

		count, err := countPodsOnCordoned(ctx, fc, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int32(0)))
	})

	It("counts all pods on a single cordoned node", func() {
		pdb := makePDB(map[string]string{"app": "myapp"})
		node := makeNode("node1", true)
		pod1 := makePod("pod1", "node1", map[string]string{"app": "myapp"})
		pod2 := makePod("pod2", "node1", map[string]string{"app": "myapp"})
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod1, pod2).Build()

		count, err := countPodsOnCordoned(ctx, fc, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int32(2)))
	})

	It("aggregates pods across multiple cordoned nodes", func() {
		pdb := makePDB(map[string]string{"app": "myapp"})
		node1 := makeNode("node1", true)
		node2 := makeNode("node2", true)
		node3 := makeNode("node3", false) // not cordoned
		pod1 := makePod("pod1", "node1", map[string]string{"app": "myapp"})
		pod2 := makePod("pod2", "node2", map[string]string{"app": "myapp"})
		pod3 := makePod("pod3", "node3", map[string]string{"app": "myapp"})
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node1, node2, node3, pod1, pod2, pod3).Build()

		count, err := countPodsOnCordoned(ctx, fc, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int32(2))) // pod3 on node3 (uncordoned) excluded
	})

	It("skips pods with no NodeName (pending/unscheduled)", func() {
		pdb := makePDB(map[string]string{"app": "myapp"})
		pod := makePod("pod1", "", map[string]string{"app": "myapp"}) // no node yet
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

		count, err := countPodsOnCordoned(ctx, fc, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int32(0)))
	})

	It("does not count pods that do not match the PDB selector", func() {
		pdb := makePDB(map[string]string{"app": "myapp"})
		node := makeNode("node1", true)
		pod := makePod("pod1", "node1", map[string]string{"app": "other"}) // different labels
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod).Build()

		count, err := countPodsOnCordoned(ctx, fc, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int32(0)))
	})

	It("counts pods on cordoned nodes while ignoring pods on uncordoned nodes (mixed cluster)", func() {
		pdb := makePDB(map[string]string{"app": "myapp"})
		cordoned := makeNode("cordoned-node", true)
		healthy := makeNode("healthy-node", false)
		// 3 pods on cordoned, 5 on healthy
		objects := []client.Object{cordoned, healthy}
		for i := 0; i < 3; i++ {
			objects = append(objects, makePod(
				"displaced-"+string(rune('a'+i)), "cordoned-node",
				map[string]string{"app": "myapp"},
			))
		}
		for i := 0; i < 5; i++ {
			objects = append(objects, makePod(
				"healthy-"+string(rune('a'+i)), "healthy-node",
				map[string]string{"app": "myapp"},
			))
		}
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

		count, err := countPodsOnCordoned(ctx, fc, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int32(3)))
	})
})
