package controllers

import (
	"context"
	"testing"

	kruiseappsv1alpha1 "github.com/openkruise/kruise-api/apps/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func udScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kruiseappsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruise scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := policyv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add policy scheme: %v", err)
	}
	return scheme
}

func TestUDSurgeApplier_ApplyThenRevertRoundTrip(t *testing.T) {
	ctx := context.Background()
	ud := makeTestUD()
	fc := fake.NewClientBuilder().WithScheme(udScheme(t)).WithObjects(ud).Build()

	// Apply surge.
	applier := &UnitedDeploymentSurgeApplier{client: fc, target: &UnitedDeploymentWrapper{obj: ud.DeepCopy()}}
	if err := applier.ApplySurge(ctx, 118); err != nil {
		t.Fatalf("ApplySurge: %v", err)
	}
	key := k8stypes.NamespacedName{Name: ud.Name, Namespace: ud.Namespace}
	got := &kruiseappsv1alpha1.UnitedDeployment{}
	if err := fc.Get(ctx, key, got); err != nil {
		t.Fatalf("get after surge: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 118 {
		t.Fatalf("surged spec.replicas = %v, want 118", got.Spec.Replicas)
	}
	if _, ok := got.Annotations[udSurgeSnapshotAnnotationKey]; !ok {
		t.Fatalf("snapshot annotation missing after surge")
	}
	if _, ok := got.Annotations[EvictionSurgeReplicasAnnotationKey]; !ok {
		t.Fatalf("surge marker annotation missing after surge")
	}

	// Revert from the surged state read back from the client.
	revApplier := &UnitedDeploymentSurgeApplier{client: fc, target: &UnitedDeploymentWrapper{obj: got.DeepCopy()}}
	if err := revApplier.RevertSurge(ctx, 100); err != nil {
		t.Fatalf("RevertSurge: %v", err)
	}
	reverted := &kruiseappsv1alpha1.UnitedDeployment{}
	if err := fc.Get(ctx, key, reverted); err != nil {
		t.Fatalf("get after revert: %v", err)
	}
	if reverted.Spec.Replicas == nil || *reverted.Spec.Replicas != 112 {
		t.Fatalf("reverted spec.replicas = %v, want 112", reverted.Spec.Replicas)
	}
	if _, ok := reverted.Annotations[udSurgeSnapshotAnnotationKey]; ok {
		t.Fatalf("snapshot annotation should be cleared after revert")
	}
	if _, ok := reverted.Annotations[EvictionSurgeReplicasAnnotationKey]; ok {
		t.Fatalf("surge marker should be cleared after revert")
	}
}

// When the snapshot is lost while a surge is active, RevertSurge must still scale
// the UD back down to the floor (best-effort) rather than leaving it surged.
func TestUDSurgeApplier_RevertWithoutSnapshotForcesScaleDown(t *testing.T) {
	ctx := context.Background()
	ud := makeTestUD()
	surged := int32(118)
	ud.Spec.Replicas = &surged
	ud.Annotations = map[string]string{EvictionSurgeReplicasAnnotationKey: "118"} // marker, no snapshot
	fc := fake.NewClientBuilder().WithScheme(udScheme(t)).WithObjects(ud).Build()

	applier := &UnitedDeploymentSurgeApplier{client: fc, target: &UnitedDeploymentWrapper{obj: ud.DeepCopy()}}
	if err := applier.RevertSurge(ctx, 100); err != nil {
		t.Fatalf("RevertSurge: %v", err)
	}
	got := &kruiseappsv1alpha1.UnitedDeployment{}
	if err := fc.Get(ctx, k8stypes.NamespacedName{Name: ud.Name, Namespace: ud.Namespace}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 100 {
		t.Fatalf("spec.replicas = %v, want 100 (forced scale-down)", got.Spec.Replicas)
	}
	if _, ok := got.Annotations[EvictionSurgeReplicasAnnotationKey]; ok {
		t.Fatalf("surge marker should be cleared after revert")
	}
}

// discoverDeployment must redirect a Deployment that is a UnitedDeployment subset
// child to the owning UnitedDeployment when the feature is enabled, and NOT
// redirect when it is disabled.
func TestDiscoverDeployment_UnitedDeploymentRedirect(t *testing.T) {
	ctx := context.Background()
	ns := "ns"
	labels := map[string]string{"app": "x"}

	ud := &kruiseappsv1alpha1.UnitedDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "myud", Namespace: ns, UID: k8stypes.UID("ud-uid")},
	}
	ctrl := true
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myud-regular", Namespace: ns, UID: k8stypes.UID("dep-uid"),
			OwnerReferences: []metav1.OwnerReference{{Kind: "UnitedDeployment", Name: "myud", UID: "ud-uid", Controller: &ctrl}},
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myud-regular-abc", Namespace: ns, UID: k8stypes.UID("rs-uid"),
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "myud-regular", UID: "dep-uid", Controller: &ctrl}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1", Namespace: ns, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "myud-regular-abc", UID: "rs-uid", Controller: &ctrl}},
		},
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "mypdb", Namespace: ns},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}

	build := func() client.Client {
		return fake.NewClientBuilder().WithScheme(udScheme(t)).WithObjects(ud, deploy, rs, pod, pdb).Build()
	}

	// Enabled -> redirect to the UnitedDeployment.
	rOn := &PDBToEvictionAutoScalerReconciler{Client: build(), EnableUnitedDeploymentSurge: true}
	name, kind, uid, err := rOn.discoverDeployment(ctx, pdb)
	if err != nil {
		t.Fatalf("discoverDeployment (enabled): %v", err)
	}
	if name != "myud" || kind != unitedDeploymentKind || uid != k8stypes.UID("ud-uid") {
		t.Fatalf("enabled: got (%q,%q,%q), want (myud,%s,ud-uid)", name, kind, uid, unitedDeploymentKind)
	}

	// Disabled -> stay on the child Deployment.
	rOff := &PDBToEvictionAutoScalerReconciler{Client: build(), EnableUnitedDeploymentSurge: false}
	name, kind, uid, err = rOff.discoverDeployment(ctx, pdb)
	if err != nil {
		t.Fatalf("discoverDeployment (disabled): %v", err)
	}
	if name != "myud-regular" || kind != deploymentKind || uid != k8stypes.UID("dep-uid") {
		t.Fatalf("disabled: got (%q,%q,%q), want (myud-regular,%s,dep-uid)", name, kind, uid, deploymentKind)
	}
}
