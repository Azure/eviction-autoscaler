package controllers

import (
	"context"
	"testing"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// reconcileSurgeTeardown selects the applier that actually owns the surge and reverts it on
// deletion. These cases exercise the two ownsActiveSurge arms the reconcile-level teardown
// relies on: the HPA/KEDA arm ("active marker is enough") and the deployment-marker fingerprint
// winning over a late-added HPA (so detectSurgeApplier can't mis-select and strand a surge).
// Kept as fast fake-client unit tests (no envtest): the Ginkgo suite covers the end-to-end
// pin/restore path, while these isolate the applier-selection branches.

const stdTeardownName, stdTeardownNS = "td-app", "td-ns"

func surgeTeardownReconciler(g *WithT, objs ...client.Object) *EvictionAutoScalerReconciler {
	scheme := runtime.NewScheme()
	g.Expect(v1.AddToScheme(scheme)).To(Succeed())
	g.Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(autoscalingv2.AddToScheme(scheme)).To(Succeed())
	g.Expect(kedav1alpha1.AddToScheme(scheme)).To(Succeed())
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &EvictionAutoScalerReconciler{Client: fc, Scheme: scheme}
}

func teardownEA() *v1.EvictionAutoScaler {
	return &v1.EvictionAutoScaler{
		ObjectMeta: metav1.ObjectMeta{Name: stdTeardownName, Namespace: stdTeardownNS, Finalizers: []string{EASSurgeFinalizer}},
		Spec:       v1.EvictionAutoScalerSpec{TargetName: stdTeardownName, TargetKind: "deployment"},
		// MinReplicas is deliberately distinct from the recorded original-min-replicas ("1") so
		// that a revert to 1 proves the durable-annotation baseline was used, not this fallback.
		Status: v1.EvictionAutoScalerStatus{MinReplicas: 9},
	}
}

// TestReconcileSurgeTeardownHPAOwnedSurge: the surge marker lives on the HPA (HPA/KEDA appliers
// mark their own object, not the deployment), so ownsActiveSurge takes the "active marker is
// enough" arm and RevertSurge resets the HPA floor.
func TestReconcileSurgeTeardownHPAOwnedSurge(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	key := client.ObjectKey{Name: stdTeardownName, Namespace: stdTeardownNS}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: stdTeardownName, Namespace: stdTeardownNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: stdTeardownName, Namespace: stdTeardownNS,
			Annotations: map[string]string{
				EvictionSurgeReplicasAnnotationKey: "3",
				OriginalMinReplicasAnnotationKey:   "1",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas:    ptr.To(int32(3)),
			MaxReplicas:    5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: stdTeardownName, APIVersion: "apps/v1"},
		},
	}
	ea := teardownEA()
	r := surgeTeardownReconciler(g, dep, hpa, ea)

	g.Expect(r.reconcileSurgeTeardown(ctx, ea)).To(Succeed())

	// HPA floor reverted to the recorded baseline (1); surge annotations cleared.
	var gotHPA autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(ctx, key, &gotHPA)).To(Succeed())
	g.Expect(gotHPA.Spec.MinReplicas).ToNot(BeNil())
	g.Expect(*gotHPA.Spec.MinReplicas).To(Equal(int32(1)))
	g.Expect(gotHPA.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	// Surge finalizer released.
	var gotEA v1.EvictionAutoScaler
	g.Expect(r.Get(ctx, key, &gotEA)).To(Succeed())
	g.Expect(gotEA.Finalizers).ToNot(ContainElement(EASSurgeFinalizer))
}

// TestReconcileSurgeTeardownDeploymentMarkerWins: a plain-Deployment surge marks the deployment;
// if an HPA is added after the surge, detectSurgeApplier would now pick the HPA — but
// hasTargetAnnotation must win so we revert the actually-surged deployment, not reset the HPA.
func TestReconcileSurgeTeardownDeploymentMarkerWins(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	key := client.ObjectKey{Name: stdTeardownName, Namespace: stdTeardownNS}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: stdTeardownName, Namespace: stdTeardownNS,
			Annotations: map[string]string{
				EvictionSurgeReplicasAnnotationKey: "3",
				OriginalMinReplicasAnnotationKey:   "1",
			},
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: stdTeardownName, Namespace: stdTeardownNS},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas:    ptr.To(int32(2)),
			MaxReplicas:    5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: stdTeardownName, APIVersion: "apps/v1"},
		},
	}
	ea := teardownEA()
	r := surgeTeardownReconciler(g, dep, hpa, ea)

	g.Expect(r.reconcileSurgeTeardown(ctx, ea)).To(Succeed())

	// Deployment reverted to the recorded baseline (1) via its own marker; annotations cleared.
	var gotDep appsv1.Deployment
	g.Expect(r.Get(ctx, key, &gotDep)).To(Succeed())
	g.Expect(gotDep.Spec.Replicas).ToNot(BeNil())
	g.Expect(*gotDep.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(gotDep.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	// The late HPA was left untouched (not selected as the applier).
	var gotHPA autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(ctx, key, &gotHPA)).To(Succeed())
	g.Expect(gotHPA.Spec.MinReplicas).ToNot(BeNil())
	g.Expect(*gotHPA.Spec.MinReplicas).To(Equal(int32(2)))
	// Surge finalizer released.
	var gotEA v1.EvictionAutoScaler
	g.Expect(r.Get(ctx, key, &gotEA)).To(Succeed())
	g.Expect(gotEA.Finalizers).ToNot(ContainElement(EASSurgeFinalizer))
}

// TestReconcileSurgeTeardownHPAMarkedDespiteLateKEDA: an HPA-surged deployment (marker on the
// HPA) that later gains a KEDA ScaledObject. Apply-time detection would trip the
// unsupported-config path (KEDA + standalone HPA) and drop the finalizer while the HPA stays
// pinned; marker-based resolveSurgeOwner must still pick and revert the marked HPA.
func TestReconcileSurgeTeardownHPAMarkedDespiteLateKEDA(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	key := client.ObjectKey{Name: stdTeardownName, Namespace: stdTeardownNS}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: stdTeardownName, Namespace: stdTeardownNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: stdTeardownName, Namespace: stdTeardownNS,
			Annotations: map[string]string{
				EvictionSurgeReplicasAnnotationKey: "3",
				OriginalMinReplicasAnnotationKey:   "1",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas:    ptr.To(int32(3)),
			MaxReplicas:    5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: stdTeardownName, APIVersion: "apps/v1"},
		},
	}
	// A KEDA ScaledObject targeting the same deployment, added after the surge (no marker).
	so := createScaledObject("td-so", stdTeardownNS, stdTeardownName, 0, 10)
	ea := teardownEA()
	r := surgeTeardownReconciler(g, dep, hpa, so, ea)

	g.Expect(r.reconcileSurgeTeardown(ctx, ea)).To(Succeed())

	// The marker-owning HPA is reverted despite the late ScaledObject.
	var gotHPA autoscalingv2.HorizontalPodAutoscaler
	g.Expect(r.Get(ctx, key, &gotHPA)).To(Succeed())
	g.Expect(gotHPA.Spec.MinReplicas).ToNot(BeNil())
	g.Expect(*gotHPA.Spec.MinReplicas).To(Equal(int32(1)))
	g.Expect(gotHPA.Annotations).ToNot(HaveKey(EvictionSurgeReplicasAnnotationKey))
	// Surge finalizer released.
	var gotEA2 v1.EvictionAutoScaler
	g.Expect(r.Get(ctx, key, &gotEA2)).To(Succeed())
	g.Expect(gotEA2.Finalizers).ToNot(ContainElement(EASSurgeFinalizer))
}

// TestReconcileSurgeTeardownClearsSurgeActiveWhenTargetGone: the workload and EAS are deleted
// together while a surge is active, so the target Get returns NotFound and none of the
// revert/ownership arms run. The terminal teardown must still drop the surge series (surge_active
// and the replica gauges), so a GC'd object cannot leak a stuck surge_active==1 series.
func TestReconcileSurgeTeardownClearsSurgeActiveWhenTargetGone(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	key := client.ObjectKey{Name: stdTeardownName, Namespace: stdTeardownNS}

	// Simulate a live surge: gauges set, but no target object.
	metrics.SurgeActive.WithLabelValues(stdTeardownNS, stdTeardownName).Set(1)
	metrics.SurgeReplicas.WithLabelValues(stdTeardownNS, stdTeardownName).Set(5)
	ea := teardownEA()
	r := surgeTeardownReconciler(g, ea)

	g.Expect(r.reconcileSurgeTeardown(ctx, ea)).To(Succeed())

	// Both surge series are dropped for the gone object, so nothing leaks against a GC'd EAS.
	g.Expect(testutil.ToFloat64(metrics.SurgeActive.WithLabelValues(stdTeardownNS, stdTeardownName))).
		To(Equal(0.0), "surge_active must be cleared on teardown even when the target is gone")
	g.Expect(testutil.ToFloat64(metrics.SurgeReplicas.WithLabelValues(stdTeardownNS, stdTeardownName))).
		To(Equal(0.0), "surge_replicas must be cleared on teardown for the deleted object")
	// Surge finalizer released.
	var gotEA v1.EvictionAutoScaler
	g.Expect(r.Get(ctx, key, &gotEA)).To(Succeed())
	g.Expect(gotEA.Finalizers).ToNot(ContainElement(EASSurgeFinalizer))
}

// TestReconcileSurgeActiveMetricTracksMarker: surge_active must track the durable surge marker
// exactly and topology-independently. A target Deployment carrying the marker → surge_active=1;
// the same target without the marker → the series is deleted. This is the single source of truth
// the degrade/idle/restart paths all rely on.
func TestReconcileSurgeActiveMetricTracksMarker(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const ns, tgt = "sa-ns", "sa-target"

	build := func(withMarker bool) (*EvictionAutoScalerReconciler, Surger, *v1.EvictionAutoScaler) {
		ann := map[string]string{}
		if withMarker {
			ann[EvictionSurgeReplicasAnnotationKey] = "3"
		}
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: tgt, Namespace: ns, Annotations: ann},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
		}
		r := surgeTeardownReconciler(g, dep)
		target, err := GetSurger("deployment")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(r.Get(ctx, client.ObjectKey{Name: tgt, Namespace: ns}, target.Obj())).To(Succeed())
		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: "sa-ea", Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: tgt, TargetKind: "deployment"},
		}
		return r, target, ea
	}

	// Marker present → surge_active reflects an active surge.
	r, target, ea := build(true)
	r.reconcileSurgeActiveMetric(ctx, ea, target)
	g.Expect(testutil.ToFloat64(metrics.SurgeActive.WithLabelValues(ns, tgt))).
		To(Equal(1.0), "surge_active must be 1 while the target carries the surge marker")

	// Marker absent → surge_active SERIES IS DELETED (not just set to 0). Proven with
	// CollectAndCount, which reflects actual series presence — unlike ToFloat64(WithLabelValues),
	// which would recreate the series and mask a missing-cleanup bug.
	metrics.SurgeActive.Reset()
	r2, target2, ea2 := build(false)
	metrics.SurgeActive.WithLabelValues(ns, tgt).Set(1)
	g.Expect(testutil.CollectAndCount(metrics.SurgeActive)).To(Equal(1), "precondition: one surge_active series present")
	r2.reconcileSurgeActiveMetric(ctx, ea2, target2)
	g.Expect(testutil.CollectAndCount(metrics.SurgeActive)).To(Equal(0), "surge_active series must be deleted (absent), not merely 0, when the marker is gone")
}

// TestReconcileSurgeReplicaMetricsMalformedAnnotations: when the surge marker is present but the
// durable baseline annotation is missing/unparseable, surgeReplicaCounts returns ok=false. In that
// case surge_active must stay 1 (the surge is real), but the replica gauges must be DELETED so a
// stale value from an earlier valid observation cannot linger.
func TestReconcileSurgeReplicaMetricsMalformedAnnotations(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const ns, tgt = "mal-ns", "mal-target"

	metrics.SurgeActive.Reset()
	metrics.SurgeReplicas.Reset()
	metrics.SurgeReplicasReady.Reset()

	// Marker present, but NO original-min-replicas annotation → RecordedBaseline() is (0,false).
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: tgt, Namespace: ns, Annotations: map[string]string{
			EvictionSurgeReplicasAnnotationKey: "10",
		}},
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(10))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 8},
	}
	r := surgeTeardownReconciler(g, dep)
	target, err := GetSurger("deployment")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, client.ObjectKey{Name: tgt, Namespace: ns}, target.Obj())).To(Succeed())
	ea := &v1.EvictionAutoScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "mal-ea", Namespace: ns},
		Spec:       v1.EvictionAutoScalerSpec{TargetName: tgt, TargetKind: "deployment"},
	}

	// Pre-seed stale replica values from a hypothetical earlier valid observation.
	metrics.SurgeReplicas.WithLabelValues(ns, tgt).Set(5)
	metrics.SurgeReplicasReady.WithLabelValues(ns, tgt).Set(3)

	r.reconcileSurgeActiveMetric(ctx, ea, target)

	g.Expect(testutil.ToFloat64(metrics.SurgeActive.WithLabelValues(ns, tgt))).
		To(Equal(1.0), "surge_active must remain 1 while the surge marker is present")
	g.Expect(testutil.CollectAndCount(metrics.SurgeReplicas)).
		To(Equal(0), "stale surge_replicas series must be deleted when the counts can't be parsed")
	g.Expect(testutil.CollectAndCount(metrics.SurgeReplicasReady)).
		To(Equal(0), "stale surge_replicas_ready series must be deleted when the counts can't be parsed")
}

// TestReconcileSurgeReplicaMetrics: surge_replicas is the extra replicas EAS requested (surged
// count minus recorded baseline), and surge_replicas_ready is the extra Ready replicas above
// baseline (clamped to the requested amount) — so realized lags while surged pods are still
// Pending on new nodes. Both are cleared when the surge marker is gone.
func TestReconcileSurgeReplicaMetrics(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const ns, tgt = "sr-ns", "sr-target"

	build := func(withMarker bool, ready int32) (*EvictionAutoScalerReconciler, Surger, *v1.EvictionAutoScaler) {
		ann := map[string]string{}
		if withMarker {
			// Surged to 10 from a baseline of 5 → 5 extra replicas requested.
			ann[EvictionSurgeReplicasAnnotationKey] = "10"
			ann[OriginalMinReplicasAnnotationKey] = "5"
		}
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: tgt, Namespace: ns, Annotations: ann},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(10))},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
		}
		r := surgeTeardownReconciler(g, dep)
		target, err := GetSurger("deployment")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(r.Get(ctx, client.ObjectKey{Name: tgt, Namespace: ns}, target.Obj())).To(Succeed())
		ea := &v1.EvictionAutoScaler{
			ObjectMeta: metav1.ObjectMeta{Name: "sr-ea", Namespace: ns},
			Spec:       v1.EvictionAutoScalerSpec{TargetName: tgt, TargetKind: "deployment"},
		}
		return r, target, ea
	}

	// Surged (10) from baseline (5) with only 8 Ready → 5 requested, 3 realized (2 still Pending
	// on nodes coming up).
	r, target, ea := build(true, 8)
	r.reconcileSurgeActiveMetric(ctx, ea, target)
	g.Expect(testutil.ToFloat64(metrics.SurgeReplicas.WithLabelValues(ns, tgt))).
		To(Equal(5.0), "surge_replicas must be surged(10) - baseline(5)")
	g.Expect(testutil.ToFloat64(metrics.SurgeReplicasReady.WithLabelValues(ns, tgt))).
		To(Equal(3.0), "surge_replicas_ready must be ready(8) - baseline(5), bounded by requested")

	// Marker gone → both gauges DELETED (proven via CollectAndCount, which won't recreate them).
	metrics.SurgeReplicas.Reset()
	metrics.SurgeReplicasReady.Reset()
	r2, target2, ea2 := build(false, 10)
	metrics.SurgeReplicas.WithLabelValues(ns, tgt).Set(5)
	metrics.SurgeReplicasReady.WithLabelValues(ns, tgt).Set(3)
	g.Expect(testutil.CollectAndCount(metrics.SurgeReplicas)).To(Equal(1), "precondition: one surge_replicas series present")
	r2.reconcileSurgeActiveMetric(ctx, ea2, target2)
	g.Expect(testutil.CollectAndCount(metrics.SurgeReplicas)).
		To(Equal(0), "surge_replicas series must be deleted (absent) when the surge marker is gone")
	g.Expect(testutil.CollectAndCount(metrics.SurgeReplicasReady)).
		To(Equal(0), "surge_replicas_ready series must be deleted (absent) when the surge marker is gone")
}
