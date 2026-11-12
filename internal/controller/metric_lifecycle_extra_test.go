package controllers

import (
	"context"
	"testing"

	v1 "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// disabledFilter forces the "eviction autoscaler not enabled for namespace" path so a reconcile can
// be driven down a no-status-write early return.
type disabledFilter struct{}

func (disabledFilter) Filter(_ context.Context, _ namespacefilter.Reader, _ string) (bool, error) {
	return false, nil
}

// TestSyncDegradedMetricFromPersistedCondition verifies the Degraded gauge is derived from the
// persisted condition: a reason transition drops the previous reason's series (no stale label), and
// recovery clears the gauge. Because syncDegradedMetric runs only after a successful status write,
// this is also what keeps a failed Status().Update from diverging the gauge from the stored state.
func TestSyncDegradedMetricFromPersistedCondition(t *testing.T) {
	g := NewWithT(t)
	const ns, name = "deg-ns", "deg-ea"
	metrics.Degraded.Reset()

	eas := &v1.EvictionAutoScaler{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	meta.SetStatusCondition(&eas.Status.Conditions, metav1.Condition{
		Type: "Degraded", Status: metav1.ConditionTrue, Reason: "MissingTarget", LastTransitionTime: metav1.Now(),
	})
	syncDegradedMetric(eas)
	g.Expect(testutil.CollectAndCount(metrics.Degraded)).To(Equal(1), "one reason series after first degrade")

	// Reason change: only the new reason series may remain (a lingering old series would make it 2).
	meta.SetStatusCondition(&eas.Status.Conditions, metav1.Condition{
		Type: "Degraded", Status: metav1.ConditionTrue, Reason: "UnsupportedAutoscalerConfiguration", LastTransitionTime: metav1.Now(),
	})
	syncDegradedMetric(eas)
	g.Expect(testutil.CollectAndCount(metrics.Degraded)).To(Equal(1), "reason change must drop the previous reason's series")

	// Recovery: condition removed -> gauge cleared entirely.
	meta.RemoveStatusCondition(&eas.Status.Conditions, "Degraded")
	syncDegradedMetric(eas)
	g.Expect(testutil.CollectAndCount(metrics.Degraded)).To(Equal(0), "gauge cleared once no longer Degraded")
}

// TestLivelyMutatedTracksLiveSpec verifies pdb_mutated's underlying predicate reflects the LIVE spec,
// not annotation residue: a pinned PDB reads true, but a partner/GitOps spec revert that leaves our
// annotations behind reads false, so the pinned-but-not-mutated drift can be alerted on.
func TestLivelyMutatedTracksLiveSpec(t *testing.T) {
	g := NewWithT(t)
	mu := intstr.FromInt32(1)

	clean := &policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu}}
	g.Expect(livelyMutated(clean)).To(BeFalse(), "an unmutated PDB is not lively-mutated")

	pinned := &policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu}}
	g.Expect(snapshotPDBSpec(pinned)).To(Succeed())
	pinPDBFloor(pinned, 4)
	g.Expect(isMutated(pinned)).To(BeTrue())
	g.Expect(livelyMutated(pinned)).To(BeTrue(), "a freshly pinned PDB carries the floor on its live spec")

	// Partner reverts the live spec back to the original disruption policy but leaves our annotations.
	reverted := pinned.DeepCopy()
	reverted.Spec.MinAvailable = nil
	reverted.Spec.MaxUnavailable = &mu
	g.Expect(isMutated(reverted)).To(BeTrue(), "annotation residue remains")
	g.Expect(livelyMutated(reverted)).To(BeFalse(), "live spec no longer carries the floor, so pdb_mutated must read 0")
}

// TestDegradedGaugeSelfHealsWithoutStatusWrite proves the Degraded gauge is re-asserted from durable
// status at reconcile entry — so it self-heals after a controller restart (fresh registry) even on a
// path that writes no status. The interceptor asserts zero status writes, so the restored series can
// only come from entry-time synchronization, not a post-write sync.
func TestDegradedGaugeSelfHealsWithoutStatusWrite(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	const ns, name = "restart-ns", "restart-ea"

	scheme := runtime.NewScheme()
	g.Expect(v1.AddToScheme(scheme)).To(Succeed())

	// A persisted-Degraded EAS with no pin held → the namespace-disabled path returns without a
	// status write.
	eas := &v1.EvictionAutoScaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1.EvictionAutoScalerSpec{TargetName: "app", TargetKind: "deployment"},
		Status:     v1.EvictionAutoScalerStatus{PDBFloorPinned: false},
	}
	meta.SetStatusCondition(&eas.Status.Conditions, metav1.Condition{
		Type: "Degraded", Status: metav1.ConditionTrue, Reason: "MissingTarget", LastTransitionTime: metav1.Now(),
	})
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(eas).WithStatusSubresource(eas).Build()

	statusWrites := 0
	ic := interceptor.NewClient(fc, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sr string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			statusWrites++
			return c.SubResource(sr).Update(ctx, obj, opts...)
		},
	})
	r := &EvictionAutoScalerReconciler{Client: ic, Scheme: scheme, Filter: disabledFilter{}}

	// Simulate a fresh registry after a controller restart.
	metrics.Degraded.Reset()

	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: name}})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(statusWrites).To(Equal(0), "the disabled path must not write status — proves the gauge came from entry-time self-heal")
	g.Expect(testutil.CollectAndCount(metrics.Degraded)).
		To(Equal(1), "Degraded gauge must self-heal from durable status even with no status write (restart scenario)")
}
