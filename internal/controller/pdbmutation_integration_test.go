package controllers

import (
	"context"
	"time"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	metrics "github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// These tests exercise the reconcile-level PDB-floor pin/restore/bail wiring.
// envtest does not run the disruption controller, so pdb.Status is populated
// manually to drive the state machine.
var _ = Describe("PDB floor pin/restore/bail", func() {
	ctx := context.Background()
	const name = "floor-ea"

	var (
		ns            string
		nsName        types.NamespacedName
		reconciler    *EvictionAutoScalerReconciler
		pdbReconciler *PDBToEvictionAutoScalerReconciler
		selectorMatch = map[string]string{"app": "floor-test"}
	)

	BeforeEach(func() {
		nsObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "floor",
				Annotations: map[string]string{
					namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true",
				},
			},
		}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
		nsName = types.NamespacedName{Name: name, Namespace: ns}

		// The master switch defaults to off (feature ships dormant); enable it per
		// instance for these specs (other specs leave it off).
		reconciler = &EvictionAutoScalerReconciler{
			Client:                  k8sClient,
			Scheme:                  k8sClient.Scheme(),
			Filter:                  &evictionTestFilter{},
			PDBFloorMutationEnabled: true,
		}
		pdbReconciler = &PDBToEvictionAutoScalerReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Filter: &evictionTestFilter{},
		}
	})

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

	addCordonedPods := func(n int) {
		for i := 0; i < n; i++ {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "floor-pod-", Namespace: ns, Labels: selectorMatch},
				Spec:       corev1.PodSpec{NodeName: "floor-node-" + ns, Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		}
	}

	cordonWithPods := func(n int) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "floor-node-" + ns},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		addCordonedPods(n)
	}

	// makeMaxUnavailablePDB creates a maxUnavailable:1 PDB (DA==0) whose floor at
	// 5 baseline replicas is 4.
	makeBlockedPDB := func() *policyv1.PodDisruptionBudget {
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
		return pdb
	}

	actuatePDB := func() {
		_, err := pdbReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
	}

	// surgeAndPin drives the initial reconcile (baseline) then the surge reconcile,
	// leaving the PDB pinned to minAvailable=4 and the deployment surged to 7.
	surgeAndPin := func(pdb *policyv1.PodDisruptionBudget) *v1.EvictionAutoScaler {
		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: name, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		// Eviction time in the past so the later scale-down cooldown check passes.
		ea.Spec.LastEviction = v1.Eviction{PodName: "p", EvictionTime: metav1.NewTime(time.Now().Add(-2 * cooldown))}
		Expect(k8sClient.Update(ctx, ea)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeTrue())
		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).To(BeNil())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(4)))
		Expect(pdb.Annotations).To(HaveKey(AnnotationOriginalPDBSpec))
		return ea
	}

	It("pins on surge, widens DisruptionsAllowed, and restores the partner PDB exactly on scale-down", func() {
		createDeployment(5, intstr.FromInt32(2))
		pdb := makeBlockedPDB()
		cordonWithPods(2)

		surgeAndPin(pdb)

		// Deployment surged to 7 so the absolute floor of 4 now yields headroom
		// (7-4=3 disruptions worth) instead of tracking the surged count.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(7)))

		// Drain finishes: disruptions allowed again.
		setPDBStatus(pdb, 1, 7, 4, 7)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		actuatePDB()

		// PDB restored to the partner's exact original (maxUnavailable:1), annotations gone.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))

		// Deployment scaled back to baseline, pin cleared.
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(5)))
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeFalse())
	})

	It("bails and restores the partner PDB on an external replica change mid-pin", func() {
		createDeployment(5, intstr.FromInt32(2))
		pdb := makeBlockedPDB()
		cordonWithPods(2)

		surgeAndPin(pdb)

		// An external actor changes the deployment replicas out from under us
		// (not via ApplySurge, so the recorded surge annotation still reads 7).
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		dep.Spec.Replicas = ptr.To(int32(10))
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		actuatePDB()

		// The bail restored the partner PDB and cleared the pin.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeFalse())
	})

	It("does not oscillate when replicas are parked above the recorded surge without a generation bump", func() {
		createDeployment(5, intstr.FromInt32(2))
		pdb := makeBlockedPDB()
		cordonWithPods(2)

		surgeAndPin(pdb) // pinned minAvailable:4, surged to 7, recorded surge annotation=7

		// Park replicas above the recorded surge via the /scale subresource, which does
		// NOT bump the Deployment generation — reproducing the HPA/KEDA-style scale-up
		// where the generation-reset path does not fire and RecordedSurge stays < live.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		scale := &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       autoscalingv1.ScaleSpec{Replicas: 10},
		}
		Expect(k8sClient.SubResource("scale").Update(ctx, dep, client.WithSubResourceBody(scale))).To(Succeed())

		// The first reconcile bails (restores the partner PDB + un-pins); subsequent
		// reconciles must NOT re-pin. Before the fix this flip-flopped every cycle.
		for i := 0; i < 3; i++ {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())
		}
		actuatePDB()

		// The PDB stays restored to the partner's original (maxUnavailable:1), pin cleared.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeFalse())
	})

	It("honors a mid-drain PDB edit and preserves it on completion", func() {
		createDeployment(5, intstr.FromInt32(5)) // maxSurge:5 so the surge is not capped at 7
		pdb := makeBlockedPDB()
		cordonWithPods(2)

		surgeAndPin(pdb) // pins minAvailable:4, surges to 7

		// Partner overwrites our pinned spec with their own edit (maxUnavailable:2),
		// leaving our tracking annotations in place; the drain is still blocking.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		pdb.Spec.MinAvailable = nil
		pdb.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(2))
		Expect(k8sClient.Update(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 0, 7, 4, 7)

		// The PDB actuator honors the edit by re-deriving at the frozen baseline:
		// maxUnavailable:2 at 5 replicas => minAvailable floor 3.
		actuatePDB()
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).To(BeNil())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(3)))

		// Grow the displaced count so surgeTarget (5+4=9) exceeds current replicas (7).
		addCordonedPods(2)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).To(BeNil())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(3)))
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(9)))

		// Drain finishes: restore must preserve the partner's latest edit.
		setPDBStatus(pdb, 1, 9, 4, 9)
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(2)))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeFalse())
	})

	It("re-pins after a partner reverts the PDB to its original spec mid-drain", func() {
		createDeployment(5, intstr.FromInt32(2))
		pdb := makeBlockedPDB()
		cordonWithPods(2)

		surgeAndPin(pdb) // pinned minAvailable:4, snapshot maxUnavailable:1

		// Partner reverts the live spec to the exact original (maxUnavailable:1) while the
		// pin is still active; our tracking annotations remain. The live spec now equals the
		// snapshot but is NOT our pin, so the actuator must re-assert the floor rather than
		// treat "spec == snapshot" as done (the old no-op left the drain unprotected).
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		pdb.Spec.MinAvailable = nil
		pdb.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(1))
		Expect(k8sClient.Update(ctx, pdb)).To(Succeed())

		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).To(BeNil())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(4)))
		Expect(pdb.Annotations).To(HaveKey(AnnotationOriginalPDBSpec))
		Expect(pdb.Annotations[AnnotationPinnedFloor]).To(Equal("4"))
	})

	It("re-baselines to a partner's absolute minAvailable set mid-drain and restores it", func() {
		createDeployment(5, intstr.FromInt32(2))
		pdb := makeBlockedPDB()
		cordonWithPods(2)

		surgeAndPin(pdb) // pinned minAvailable:4, snapshot maxUnavailable:1

		// Partner replaces our pin with their own absolute minAvailable:2, leaving our
		// tracking annotations; drain still blocking. This is the case the old guard-4
		// short-circuit adopted silently (pinned-floor stayed 4, snapshot stayed the
		// original). Now we honor it: re-derive the floor from minAvailable:2 at the frozen
		// baseline (=2), re-pin it, and re-snapshot it as the restore target.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		pdb.Spec.MaxUnavailable = nil
		pdb.Spec.MinAvailable = ptr.To(intstr.FromInt32(2))
		Expect(k8sClient.Update(ctx, pdb)).To(Succeed())

		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(2)))
		Expect(pdb.Annotations[AnnotationPinnedFloor]).To(Equal("2"))
		Expect(pdb.Annotations[AnnotationOriginalPDBSpec]).To(ContainSubstring("minAvailable"))

		// Drain finishes: restore must return the partner's latest policy (minAvailable:2),
		// not the pre-drain original (maxUnavailable:1).
		setPDBStatus(pdb, 1, 7, 2, 7)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).To(BeNil())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(2)))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
		ea := &v1.EvictionAutoScaler{}
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeFalse())
	})

	It("treats a partner minAvailable equal to our floor as our own pin (holding, no re-baseline)", func() {
		createDeployment(5, intstr.FromInt32(2))
		pdb := makeBlockedPDB()
		cordonWithPods(2)

		surgeAndPin(pdb) // pinned minAvailable:4, snapshot maxUnavailable:1

		// Partner sets minAvailable:4 — coincidentally equal to our recorded floor, so it is
		// indistinguishable from our pin. We hold: no re-snapshot, the true original is kept.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		pdb.Spec.MaxUnavailable = nil
		pdb.Spec.MinAvailable = ptr.To(intstr.FromInt32(4))
		Expect(k8sClient.Update(ctx, pdb)).To(Succeed())

		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(4)))
		// Snapshot is NOT re-baselined to the coincidence — it stays the true original.
		Expect(pdb.Annotations[AnnotationOriginalPDBSpec]).To(ContainSubstring("maxUnavailable"))
		Expect(pdb.Annotations[AnnotationPinnedFloor]).To(Equal("4"))

		// On completion, restore returns the true original (maxUnavailable:1), not minAvailable:4.
		setPDBStatus(pdb, 1, 7, 4, 7)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(pdb.Spec.MinAvailable).To(BeNil())
	})

	It("keeps re-pinning across repeated GitOps re-applies of the original spec", func() {
		createDeployment(5, intstr.FromInt32(2))
		pdb := makeBlockedPDB()
		cordonWithPods(2)

		surgeAndPin(pdb) // pinned minAvailable:4, snapshot maxUnavailable:1

		// Simulate a GitOps reconciler re-applying the declared original (maxUnavailable:1)
		// every sync; each actuate must re-assert our floor, and the snapshot must not drift.
		for i := 0; i < 3; i++ {
			Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
			pdb.Spec.MinAvailable = nil
			pdb.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(1))
			Expect(k8sClient.Update(ctx, pdb)).To(Succeed())

			actuatePDB()

			Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
			Expect(pdb.Spec.MaxUnavailable).To(BeNil())
			Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
			Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(4)), "cycle %d should re-pin", i)
		}
		// Snapshot never drifted from the true original across the tug-of-war.
		Expect(pdb.Annotations[AnnotationOriginalPDBSpec]).To(ContainSubstring("maxUnavailable"))
		Expect(pdb.Annotations[AnnotationPinnedFloor]).To(Equal("4"))

		// Completion restores the original exactly.
		setPDBStatus(pdb, 1, 7, 4, 7)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(pdb.Spec.MinAvailable).To(BeNil())
	})

	It("does not publish a pin policy when the workload is above its baseline (autoscaler above min)", func() {
		createDeployment(7, intstr.FromInt32(2))
		minReplicas := int32(5)
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: name, APIVersion: "apps/v1"},
				MinReplicas:    &minReplicas,
				MaxReplicas:    10,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())
		pdb := makeBlockedPDB()

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: name, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.MinReplicas).To(Equal(int32(5)))

		ea.Spec.LastEviction = v1.Eviction{PodName: "p", EvictionTime: metav1.NewTime(time.Now().Add(-2 * cooldown))}
		Expect(k8sClient.Update(ctx, ea)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		actuatePDB()

		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeFalse())

		dh, err := desiredHealthyAt(pdb.Spec, 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(dh).To(Equal(int32(4)))
	})

	It("restores a lingering pin on the no-scale completion path", func() {
		// Aftermath of a partial cleanup: a prior drain pinned the PDB (minAvailable:4
		// + our tracking annotations) but the restore never completed, and the
		// Deployment is back at baseline. With disruptions allowed again and a
		// past-cooldown eviction, the reconcile reaches the no-scale tail — which must
		// restore the held pin (before this hardening it would linger, since a
		// status-only update does not re-trigger a reconcile).
		createDeployment(5, intstr.FromInt32(2))

		ma := intstr.FromInt32(4)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Annotations: map[string]string{
					AnnotationOriginalPDBSpec: `{"maxUnavailable":1}`, // partner's original spec
					AnnotationPinnedFloor:     "4",
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &ma, // our pin
				Selector:     &metav1.LabelSelector{MatchLabels: selectorMatch},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
		setPDBStatus(pdb, 1, 5, 4, 5) // disruptions allowed again, at baseline

		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: name, TargetKind: "deployment"},
		}
		Expect(k8sClient.Create(ctx, ea)).To(Succeed())

		// First reconcile initializes MinReplicas/TargetGeneration (pin left intact).
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())

		// An unhandled, past-cooldown eviction so the reconcile falls through to the
		// no-scale tail (DA>0, cooldown elapsed, replicas == MinReplicas).
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		ea.Spec.LastEviction = v1.Eviction{PodName: "p", EvictionTime: metav1.NewTime(time.Now().Add(-2 * cooldown))}
		Expect(k8sClient.Update(ctx, ea)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
		actuatePDB()

		// The no-scale tail restored the partner's PDB and cleared our tracking.
		Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).To(BeNil())
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
		Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
		Expect(ea.Status.PDBFloorPinned).To(BeFalse())
	})

	// Reconcile-level teardown state machine: deleting an EAS mid-pin/mid-surge must
	// restore the partner PDB and revert the surge, and must not strand either even when a
	// finalizer or snapshot has been externally removed.
	Context("teardown on EAS deletion", func() {
		It("adds the PDB-floor finalizer to the EAS before pinning the partner PDB", func() {
			createDeployment(5, intstr.FromInt32(2))
			pdb := makeBlockedPDB()
			cordonWithPods(2)

			ea := surgeAndPin(pdb) // pins the PDB; finalizer must be persisted before the pin

			Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
			Expect(ea.Finalizers).To(ContainElement(PDBFloorFinalizer))
		})

		It("restores the partner PDB and releases the floor finalizer when the EAS is deleted mid-pin", func() {
			createDeployment(5, intstr.FromInt32(2))
			pdb := makeBlockedPDB()
			cordonWithPods(2)

			ea := surgeAndPin(pdb)

			// Finalizers hold the CR in Terminating; the teardown-first path must restore.
			Expect(k8sClient.Delete(ctx, ea)).To(Succeed())
			_, err := pdbReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
			Expect(pdb.Spec.MinAvailable).To(BeNil())
			Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
			Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
			Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
			Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))

			// The floor finalizer is released (the CR may still exist under EASSurgeFinalizer).
			if err := k8sClient.Get(ctx, nsName, ea); err == nil {
				Expect(ea.Finalizers).NotTo(ContainElement(PDBFloorFinalizer))
			}
		})

		It("still restores the partner PDB when the floor finalizer was externally removed", func() {
			createDeployment(5, intstr.FromInt32(2))
			pdb := makeBlockedPDB()
			cordonWithPods(2)

			ea := surgeAndPin(pdb)

			// Strip the floor finalizer but keep the surge finalizer, so the CR still goes
			// Terminating rather than being GC'd — reproducing a lost-finalizer pinned PDB.
			Expect(k8sClient.Get(ctx, nsName, ea)).To(Succeed())
			ea.Finalizers = []string{EASSurgeFinalizer}
			Expect(k8sClient.Update(ctx, ea)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ea)).To(Succeed())

			_, err := pdbReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())

			// Best-effort teardown restored the PDB despite the missing floor finalizer.
			Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
			Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
			Expect(pdb.Spec.MaxUnavailable.IntVal).To(Equal(int32(1)))
			Expect(pdb.Annotations).NotTo(HaveKey(AnnotationOriginalPDBSpec))
		})

		It("force-releases and records the unrestorable metric when the restore snapshot is missing", func() {
			createDeployment(5, intstr.FromInt32(2))
			pdb := makeBlockedPDB()
			cordonWithPods(2)

			ea := surgeAndPin(pdb)

			// Tamper: drop the snapshot annotation but leave the pinned-floor marker, so the
			// PDB is recognizably ours yet unrestorable (isMutated=false, raw-annotation=true).
			Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
			delete(pdb.Annotations, AnnotationOriginalPDBSpec)
			Expect(k8sClient.Update(ctx, pdb)).To(Succeed())

			before := testutil.ToFloat64(metrics.PDBFloorTeardownUnrestorableCounter.WithLabelValues(ns, name))

			Expect(k8sClient.Delete(ctx, ea)).To(Succeed())
			_, err := pdbReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())

			// Stale pin annotation cleared and the floor finalizer released — no wedged CR.
			Expect(k8sClient.Get(ctx, nsName, pdb)).To(Succeed())
			Expect(pdb.Annotations).NotTo(HaveKey(AnnotationPinnedFloor))
			if err := k8sClient.Get(ctx, nsName, ea); err == nil {
				Expect(ea.Finalizers).NotTo(ContainElement(PDBFloorFinalizer))
			}
			after := testutil.ToFloat64(metrics.PDBFloorTeardownUnrestorableCounter.WithLabelValues(ns, name))
			Expect(after).To(BeNumerically(">", before))
		})

		It("reverts the surge and releases the surge finalizer when the EAS is deleted mid-surge", func() {
			createDeployment(5, intstr.FromInt32(2))
			pdb := makeBlockedPDB()
			cordonWithPods(2)

			ea := surgeAndPin(pdb) // deployment surged to 7, EASSurgeFinalizer held

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(7)))

			Expect(k8sClient.Delete(ctx, ea)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
			Expect(err).NotTo(HaveOccurred())

			// Surge reverted to the recorded baseline and the surge finalizer released.
			Expect(k8sClient.Get(ctx, nsName, dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(5)))
			if err := k8sClient.Get(ctx, nsName, ea); err == nil {
				Expect(ea.Finalizers).NotTo(ContainElement(EASSurgeFinalizer))
			}
		})

		It("easOwnsPDB rejects a same-name replacement PDB with a different UID", func() {
			owned := &v1.EvictionAutoScaler{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{Kind: ResourceTypePDB, UID: types.UID("uid-A")}},
				},
			}
			livePDB := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid-B")}}
			Expect(easOwnsPDB(owned, livePDB)).To(BeFalse())

			livePDB.UID = types.UID("uid-A")
			Expect(easOwnsPDB(owned, livePDB)).To(BeTrue())

			// A legacy EAS with no PDB owner-ref falls back to name identity (owns).
			legacy := &v1.EvictionAutoScaler{}
			Expect(easOwnsPDB(legacy, livePDB)).To(BeTrue())
		})
	})
})
