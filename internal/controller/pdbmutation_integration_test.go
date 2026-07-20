package controllers

import (
	"context"
	"encoding/json"
	"time"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// These tests exercise the reconcile-level PDB-floor mutation wiring. envtest
// does not run the disruption controller, so pdb.Status is populated manually
// (DesiredHealthy/DisruptionsAllowed) to drive the state machine.
var _ = Describe("PDB floor mutation", func() {
	ctx := context.Background()
	const name = "floor-ea"

	var (
		ns            string
		nsName        types.NamespacedName
		reconciler    *EvictionAutoScalerReconciler
		prevEnabled   bool
		selectorMatch = map[string]string{"app": "floor-test"}
	)

	// makeSnapshot builds the JSON snapshot of a partner spec (as stored in the
	// original-pdb-spec annotation).
	makeSnapshot := func(spec policyv1.PodDisruptionBudgetSpec) string {
		b, err := json.Marshal(spec)
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}

	BeforeEach(func() {
		prevEnabled = pdbFloorMutationEnabled
		pdbFloorMutationEnabled = true

		nsObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "floor",
				Annotations: map[string]string{
					namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
					AnnotationNamespacePDBFloorOptIn:                      "true",
				},
			},
		}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
		nsName = types.NamespacedName{Name: name, Namespace: ns}

		reconciler = &EvictionAutoScalerReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Filter: &evictionTestFilter{},
		}
	})

	AfterEach(func() {
		pdbFloorMutationEnabled = prevEnabled
	})

	// createDeployment creates a deployment with the given replicas + maxSurge.
	createDeployment := func(replicas int32, maxSurge intstr.IntOrString) *appsv1.Deployment {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(replicas),
				Selector: &metav1.LabelSelector{MatchLabels: selectorMatch},
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &maxSurge},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: selectorMatch},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		return dep
	}

	// setPDBStatus updates the PDB status subresource.
	setPDBStatus := func(pdb *policyv1.PodDisruptionBudget, disruptionsAllowed, currentHealthy, desiredHealthy, expected int32) {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, pdb)).To(Succeed())
		pdb.Status = policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: disruptionsAllowed,
			CurrentHealthy:     currentHealthy,
			DesiredHealthy:     desiredHealthy,
			ExpectedPods:       expected,
			ObservedGeneration: pdb.Generation,
		}
		Expect(k8sClient.Status().Update(ctx, pdb)).To(Succeed())
	}

	// cordonWithPods creates a cordoned node with n pods matching the selector.
	cordonWithPods := func(n int) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "floor-node-" + ns},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		for i := 0; i < n; i++ {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "floor-pod-", Namespace: ns, Labels: selectorMatch},
				Spec:       corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		}
	}

	It("pins the PDB floor and adds a finalizer on surge (enabled)", func() {
		createDeployment(5, intstr.FromInt32(2))

		mu := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &mu,
				Selector:       &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 0, 4, 4, 5) // DA==0, DesiredHealthy==4 (the floor)

		cordonWithPods(2)

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: name, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		// First reconcile populates TargetGeneration / MinReplicas.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// Log a new eviction to enter the surge path.
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		ea.Spec.LastEviction = v1.Eviction{PodName: "p", EvictionTime: metav1.Now()}
		Expect(k8sClient.Update(ctx, ea)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// PDB is pinned to minAvailable=4, maxUnavailable cleared, snapshot present.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).To(BeNil())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(4)))
		Expect(pdb.Annotations).To(HaveKey(AnnotationOriginalPDBSpec))
		Expect(pdb.Annotations).To(HaveKey(AnnotationMutatedAt))

		// CR carries the finalizer and the persisted floor.
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(ea, PDBFloorFinalizer)).To(BeTrue())
		Expect(ea.Status.PinnedPDBFloor).NotTo(BeNil())
		Expect(*ea.Status.PinnedPDBFloor).To(Equal(int32(4)))
	})

	It("does not touch the PDB when the feature is disabled", func() {
		pdbFloorMutationEnabled = false

		createDeployment(5, intstr.FromInt32(2))
		mu := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &mu,
				Selector:       &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 0, 4, 4, 5)
		cordonWithPods(2)

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: name, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		ea.Spec.LastEviction = v1.Eviction{PodName: "p", EvictionTime: metav1.Now()}
		Expect(k8sClient.Update(ctx, ea)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// PDB untouched, no finalizer, no floor.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(ea, PDBFloorFinalizer)).To(BeFalse())
		Expect(ea.Status.PinnedPDBFloor).To(BeNil())
	})

	It("restores the partner PDB and drops the finalizer on scale-down", func() {
		// Deployment already surged to 7; drain is done (DA>0).
		createDeployment(7, intstr.FromInt32(2))

		origSpec := policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
			Selector:       &metav1.LabelSelector{MatchLabels: selectorMatch},
		}
		pinned := intstr.FromInt32(4)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Annotations: map[string]string{
					AnnotationOriginalPDBSpec: makeSnapshot(origSpec),
					AnnotationMutatedAt:       time.Now().UTC().Format(time.RFC3339),
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &pinned,
				Selector:     &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 1, 7, 4, 7) // DA>0 -> drain done

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{PDBFloorFinalizer}},
			Spec: v1.EvictionAutoScalerSpec{
				TargetName:   name,
				TargetKind:   "deployment",
				LastEviction: v1.Eviction{PodName: "p", EvictionTime: metav1.NewTime(time.Now().Add(-2 * cooldown))},
			},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		ea.Status.MinReplicas = 5
		ea.Status.TargetGeneration = dep.Generation
		ea.Status.PinnedPDBFloor = ptr.To(int32(4))
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// PDB restored to the original partner spec.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))

		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(ea, PDBFloorFinalizer)).To(BeFalse())
		Expect(ea.Status.PinnedPDBFloor).To(BeNil())
	})

	It("restores the PDB and removes the finalizer when the CR is deleted", func() {
		createDeployment(5, intstr.FromInt32(2))

		origSpec := policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
			Selector:       &metav1.LabelSelector{MatchLabels: selectorMatch},
		}
		pinned := intstr.FromInt32(4)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Annotations: map[string]string{
					AnnotationOriginalPDBSpec: makeSnapshot(origSpec),
					AnnotationMutatedAt:       time.Now().UTC().Format(time.RFC3339),
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &pinned,
				Selector:     &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{PDBFloorFinalizer}},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: name, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		// Delete the CR — the finalizer keeps it around with a deletionTimestamp.
		Expect(k8sClient.Delete(ctx, ea)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// PDB restored, and the CR is fully gone (finalizer removed).
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))

		got := &v1.EvictionAutoScaler{}
		err = k8sClient.Get(ctx, nsName, got)
		Expect(err).To(HaveOccurred()) // NotFound
	})

	It("restores a stale mutated PDB via the backstop", func() {
		createDeployment(5, intstr.FromInt32(2))

		origSpec := policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
			Selector:       &metav1.LabelSelector{MatchLabels: selectorMatch},
		}
		pinned := intstr.FromInt32(4)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Annotations: map[string]string{
					AnnotationOriginalPDBSpec: makeSnapshot(origSpec),
					// Older than the default 2h stale window.
					AnnotationMutatedAt: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &pinned,
				Selector:     &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: name, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
	})

	It("does not pin when the namespace has not opted in", func() {
		// Remove the per-namespace opt-in annotation (master flag stays on).
		nsObj := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ns}, nsObj)).To(Succeed())
		delete(nsObj.Annotations, AnnotationNamespacePDBFloorOptIn)
		Expect(k8sClient.Update(ctx, nsObj)).To(Succeed())

		createDeployment(5, intstr.FromInt32(2))
		mu := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &mu,
				Selector:       &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 0, 4, 4, 5)
		cordonWithPods(2)

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: name, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		ea.Spec.LastEviction = v1.Eviction{PodName: "p", EvictionTime: metav1.Now()}
		Expect(k8sClient.Update(ctx, ea)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// PDB untouched, no finalizer, no floor — namespace did not opt in.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(ea, PDBFloorFinalizer)).To(BeFalse())
		Expect(ea.Status.PinnedPDBFloor).To(BeNil())
	})

	It("re-pins the floor after a partner overwrites the PDB mid-drain", func() {
		createDeployment(7, intstr.FromInt32(2)) // already surged

		// Partner has just overwritten the PDB: maxUnavailable back, our annotations
		// stripped. DA still 0 (drain ongoing). We only know the floor from the CR
		// status fallback.
		mu := intstr.FromInt32(5)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &mu,
				Selector:       &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 0, 7, 2, 7) // DA==0, drain ongoing

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{PDBFloorFinalizer}},
			Spec: v1.EvictionAutoScalerSpec{
				TargetName:   name,
				TargetKind:   "deployment",
				LastEviction: v1.Eviction{PodName: "p", EvictionTime: metav1.Now()},
			},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		ea.Status.MinReplicas = 5
		ea.Status.TargetGeneration = dep.Generation
		ea.Status.PinnedPDBFloor = ptr.To(int32(4))
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// Re-pinned to F=4, and the partner's NEW intent (maxUnavailable:5) is now
		// the snapshot that will be restored later.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).To(BeNil())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(4)))
		Expect(pdb.Annotations).To(HaveKey(AnnotationOriginalPDBSpec))
		var snap policyv1.PodDisruptionBudgetSpec
		Expect(json.Unmarshal([]byte(pdb.Annotations[AnnotationOriginalPDBSpec]), &snap)).To(Succeed())
		Expect(snap.MaxUnavailable).NotTo(BeNil())
		Expect(snap.MaxUnavailable.IntVal).To(Equal(int32(5)))
	})

	It("cleans up an existing pin on scale-down even when the feature is disabled", func() {
		pdbFloorMutationEnabled = false // operator turned the master flag off mid-drain

		createDeployment(7, intstr.FromInt32(2))
		origSpec := policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
			Selector:       &metav1.LabelSelector{MatchLabels: selectorMatch},
		}
		pinned := intstr.FromInt32(4)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Annotations: map[string]string{
					AnnotationOriginalPDBSpec: makeSnapshot(origSpec),
					AnnotationMutatedAt:       time.Now().UTC().Format(time.RFC3339),
					AnnotationPinnedFloor:     "4",
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &pinned,
				Selector:     &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 1, 7, 4, 7) // DA>0 -> drain done

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{PDBFloorFinalizer}},
			Spec: v1.EvictionAutoScalerSpec{
				TargetName:   name,
				TargetKind:   "deployment",
				LastEviction: v1.Eviction{PodName: "p", EvictionTime: metav1.NewTime(time.Now().Add(-2 * cooldown))},
			},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		ea.Status.MinReplicas = 5
		ea.Status.TargetGeneration = dep.Generation
		ea.Status.PinnedPDBFloor = ptr.To(int32(4))
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// Restore still happens despite the flag being off.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(ea, PDBFloorFinalizer)).To(BeFalse())
		Expect(ea.Status.PinnedPDBFloor).To(BeNil())
	})

	It("stops claiming the pin (removes marker, drops finalizer) when the snapshot annotation is gone on scale-down", func() {
		createDeployment(7, intstr.FromInt32(2))

		// Partner stripped the original-pdb-spec snapshot but the spec is still
		// pinned at F and our pinned-floor marker remains. Drain is done (DA>0).
		pinned := intstr.FromInt32(4)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Annotations: map[string]string{
					AnnotationPinnedFloor: "4",
					// original-pdb-spec intentionally absent (stripped by partner).
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &pinned,
				Selector:     &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 1, 7, 4, 7) // DA>0 -> drain done

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{PDBFloorFinalizer}},
			Spec: v1.EvictionAutoScalerSpec{
				TargetName:   name,
				TargetKind:   "deployment",
				LastEviction: v1.Eviction{PodName: "p", EvictionTime: metav1.NewTime(time.Now().Add(-2 * cooldown))},
			},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		ea.Status.MinReplicas = 5
		ea.Status.TargetGeneration = dep.Generation
		ea.Status.PinnedPDBFloor = ptr.To(int32(4))
		Expect(k8sClient.Status().Update(ctx, ea)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// We cannot restore (snapshot gone), so the stricter floor stays, but we
		// stop claiming it: our marker annotation is removed, the finalizer is
		// dropped, and the CR floor is cleared.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(4)))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(ea, PDBFloorFinalizer)).To(BeFalse())
		Expect(ea.Status.PinnedPDBFloor).To(BeNil())
	})
})
