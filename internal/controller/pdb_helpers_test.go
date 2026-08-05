package controllers

import (
	"context"

	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("ParseMaxUnavailableToMinAvailablePercentage", func() {
	It("returns nil for an empty value", func() {
		value, err := ParseMaxUnavailableToMinAvailablePercentage("")
		Expect(err).NotTo(HaveOccurred())
		Expect(value).To(BeNil())
	})

	DescribeTable("parses valid percentages",
		func(raw, expected string) {
			value, err := ParseMaxUnavailableToMinAvailablePercentage(raw)
			Expect(err).NotTo(HaveOccurred())
			Expect(value).NotTo(BeNil())
			Expect(*value).To(Equal(intstr.FromString(expected)))
		},
		Entry("lower boundary", "1", "1%"),
		Entry("typical value", "90", "90%"),
		Entry("upper boundary", "100", "100%"),
	)

	DescribeTable("rejects invalid percentages",
		func(raw string) {
			_, err := ParseMaxUnavailableToMinAvailablePercentage(raw)
			Expect(err).To(HaveOccurred())
		},
		Entry("zero", "0"),
		Entry("negative", "-1"),
		Entry("above 100", "101"),
		Entry("percentage syntax", "90%"),
		Entry("malformed", "abc"),
	)
})

var _ = Describe("maxUnavailable PDB conversion", func() {
	var (
		ctx        context.Context
		scheme     *runtime.Scheme
		percentage *intstr.IntOrString
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(policyv1.AddToScheme(scheme)).To(Succeed())
		value := intstr.FromString("90%")
		percentage = &value
	})

	makePDB := func(maxUnavailable intstr.IntOrString) *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pdb",
				Namespace:   "managed",
				Annotations: map[string]string{"example": "preserved"},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &maxUnavailable,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
			},
		}
	}

	DescribeTable("converts integer and percentage maxUnavailable values",
		func(maxUnavailable intstr.IntOrString) {
			pdb := makePDB(maxUnavailable)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pdb).Build()
			r := &PDBToEvictionAutoScalerReconciler{
				Client:                                 fakeClient,
				Scheme:                                 scheme,
				MaxUnavailableToMinAvailablePercentage: percentage,
			}

			converted, err := r.convertMaxUnavailablePDB(ctx, pdb)
			Expect(err).NotTo(HaveOccurred())
			Expect(converted).To(BeTrue())

			updated := &policyv1.PodDisruptionBudget{}
			Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(pdb), updated)).To(Succeed())
			Expect(updated.Spec.MaxUnavailable).To(BeNil())
			Expect(updated.Spec.MinAvailable).NotTo(BeNil())
			Expect(*updated.Spec.MinAvailable).To(Equal(intstr.FromString("90%")))
			Expect(updated.Spec.Selector.MatchLabels).To(Equal(map[string]string{"app": "test"}))
			Expect(updated.Annotations).To(HaveKeyWithValue("example", "preserved"))
		},
		Entry("integer", intstr.FromInt(1)),
		Entry("percentage", intstr.FromString("25%")),
	)

	It("does nothing when the option is disabled", func() {
		pdb := makePDB(intstr.FromInt(1))
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pdb).Build()
		r := &PDBToEvictionAutoScalerReconciler{Client: fakeClient, Scheme: scheme}

		converted, err := r.convertMaxUnavailablePDB(ctx, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(converted).To(BeFalse())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
	})

	It("does nothing to an existing minAvailable PDB", func() {
		minAvailable := intstr.FromString("75%")
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pdb", Namespace: "managed"},
			Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvailable},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pdb).Build()
		r := &PDBToEvictionAutoScalerReconciler{
			Client:                                 fakeClient,
			Scheme:                                 scheme,
			MaxUnavailableToMinAvailablePercentage: percentage,
		}

		converted, err := r.convertMaxUnavailablePDB(ctx, pdb)
		Expect(err).NotTo(HaveOccurred())
		Expect(converted).To(BeFalse())
		Expect(*pdb.Spec.MinAvailable).To(Equal(minAvailable))
	})

	It("leaves a PDB in an unmanaged namespace unchanged", func() {
		pdb := makePDB(intstr.FromInt(1))
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "managed"}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, pdb).Build()
		r := &PDBToEvictionAutoScalerReconciler{
			Client:                                 fakeClient,
			Scheme:                                 scheme,
			Filter:                                 namespacefilter.New([]string{}, true),
			MaxUnavailableToMinAvailablePercentage: percentage,
		}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(pdb)})
		Expect(err).NotTo(HaveOccurred())

		updated := &policyv1.PodDisruptionBudget{}
		Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(pdb), updated)).To(Succeed())
		Expect(updated.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(updated.Spec.MinAvailable).To(BeNil())
	})
})

var _ = Describe("triggerOnPDBRelevantChange", func() {
	makePDB := func() *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pdb", Namespace: "default"},
		}
	}

	It("triggers when maxUnavailable is added", func() {
		oldPDB := makePDB()
		newPDB := oldPDB.DeepCopy()
		value := intstr.FromInt(1)
		newPDB.Spec.MaxUnavailable = &value

		Expect(triggerOnPDBRelevantChange(event.UpdateEvent{ObjectOld: oldPDB, ObjectNew: newPDB}, logr.Discard())).To(BeTrue())
	})

	It("triggers when conversion clears maxUnavailable", func() {
		oldPDB := makePDB()
		value := intstr.FromString("25%")
		oldPDB.Spec.MaxUnavailable = &value
		newPDB := oldPDB.DeepCopy()
		newPDB.Spec.MaxUnavailable = nil
		minAvailable := intstr.FromString("90%")
		newPDB.Spec.MinAvailable = &minAvailable

		Expect(triggerOnPDBRelevantChange(event.UpdateEvent{ObjectOld: oldPDB, ObjectNew: newPDB}, logr.Discard())).To(BeTrue())
	})

	It("ignores unrelated PDB updates", func() {
		oldPDB := makePDB()
		newPDB := oldPDB.DeepCopy()
		newPDB.Labels = map[string]string{"changed": "true"}

		Expect(triggerOnPDBRelevantChange(event.UpdateEvent{ObjectOld: oldPDB, ObjectNew: newPDB}, logr.Discard())).To(BeFalse())
	})
})

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
