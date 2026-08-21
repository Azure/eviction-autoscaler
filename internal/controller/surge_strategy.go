package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SurgeApplier abstracts the mechanism for temporarily increasing minimum replicas.
// Exactly one implementation is used per deployment, determined by detectSurgeApplier:
//   - KEDASurgeApplier: when a KEDA ScaledObject targets the deployment
//   - HPASurgeApplier: when a standalone HPA targets the deployment (no KEDA)
//   - DeploymentSurgeApplier: when neither KEDA nor HPA is present
//
// KEDA + standalone HPA on the same target is unsupported and rejected by detectSurgeApplier.
//
// For autoscaler strategies (HPA, KEDA): the autoscaler floor is raised first, then
// deployment replicas are set directly for immediate effect. On failure, the reconcile
// loop retries ApplySurge idempotently until the deployment write succeeds.
type SurgeApplier interface {
	// ApplySurge sets the minimum replica count to surgeReplicas.
	// Callers may invoke this multiple times; implementations must be idempotent.
	ApplySurge(ctx context.Context, surgeReplicas int32) error
	// RevertSurge restores the original minimum replica count.
	RevertSurge(ctx context.Context, originalMinReplicas int32) error
	// IsSurgeActive returns true if a surge is currently in progress on the target.
	// Used during generation tracking to distinguish our own scaling from external changes.
	IsSurgeActive() bool
	// RecordedSurge returns the replica count recorded by the last ApplySurge (from the
	// evictionSurgeReplicas annotation) and whether it is present. Used by the
	// bail-on-replica-change guard to detect an external replica edit mid-surge.
	RecordedSurge() (int32, bool)
	// RecordedBaseline returns the pre-surge baseline recorded by ApplySurge (from the
	// original-min-replicas annotation) and whether it is present. Used to recover the
	// true baseline for an EvictionAutoScaler that lost its Status.MinReplicas (e.g. a
	// freshly recreated CR that started at 0 while a surge was already active).
	RecordedBaseline() (int32, bool)
	// Name returns a human-readable name for logging
	Name() string
}

// recordedBaselineFromAnnotations reads the original-min-replicas annotation set by
// ApplySurge and returns the recorded pre-surge baseline. Returns (0,false) when the
// annotation is absent or unparseable.
func recordedBaselineFromAnnotations(annotations map[string]string) (int32, bool) {
	if annotations == nil {
		return 0, false
	}
	v, ok := annotations[OriginalMinReplicasAnnotationKey]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// recordedSurgeFromAnnotations reads the evictionSurgeReplicas annotation set by
// ApplySurge and returns the recorded surge replica count. Returns (0,false) when
// the annotation is absent or unparseable.
func recordedSurgeFromAnnotations(annotations map[string]string) (int32, bool) {
	if annotations == nil {
		return 0, false
	}
	v, ok := annotations[EvictionSurgeReplicasAnnotationKey]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// errUnsupportedAutoscalerConfig is returned when KEDA + standalone HPA both target
// the same deployment. This is a permanent misconfiguration that cannot be resolved
// by retrying — the reconciler should surface it on status and stop requeueing.
var errUnsupportedAutoscalerConfig = errors.New("unsupported autoscaler configuration")

// detectSurgeApplier determines which surge strategy to use based on the
// autoscaler resources targeting this workload. The strategies are mutually
// exclusive — exactly one applier is returned:
//
//   - KEDA ScaledObject present → KEDASurgeApplier (raises minReplicaCount + sets deployment replicas)
//   - Standalone HPA present (no KEDA) → HPASurgeApplier (raises minReplicas + sets deployment replicas)
//   - Neither → DeploymentSurgeApplier (sets deployment replicas directly)
//
// KEDA + standalone HPA on the same target is treated as unsupported. KEDA already
// creates and owns its own HPA for the target, and validates against unmanaged HPAs
// on the same scale target. If we detect both, we return an error — the eviction
// autoscaler can't fix multiple-writer conflicts and shouldn't try. The reconciler
// logs the error and skips the deployment. KEDA-managed HPAs (identified by
// label/ownerRef) are always filtered out by findHPAForTarget and never reach this logic.
func detectSurgeApplier(ctx context.Context, c client.Client, namespace, targetName, targetKind string, target Surger) (SurgeApplier, error) {
	logger := log.FromContext(ctx)

	// HPA and KEDA only target Deployments; skip autoscaler detection for other kinds.
	if strings.EqualFold(targetKind, ResourceTypeDeployment) {
		// Check for KEDA ScaledObject targeting this workload
		scaledObj, err := findScaledObjectForTarget(ctx, c, namespace, targetName, targetKind)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("checking for KEDA ScaledObject: %w", err)
		}
		if scaledObj != nil {
			// Reject if a standalone HPA also targets this deployment. This is an
			// unsupported configuration — KEDA already owns an HPA for the target,
			// and having an additional standalone HPA creates multiple-writer conflicts
			// that the eviction autoscaler cannot resolve safely.
			standaloneHPA, hpaErr := findHPAForTarget(ctx, c, namespace, targetName, targetKind)
			if hpaErr == nil && standaloneHPA != nil {
				return nil, fmt.Errorf("%w: both KEDA ScaledObject %q and "+
					"standalone HPA %q target deployment %q in namespace %q — "+
					"eviction autoscaler cannot safely surge with multiple autoscaler writers",
					errUnsupportedAutoscalerConfig, scaledObj.GetName(), standaloneHPA.Name, targetName, namespace)
			}

			logger.Info("Found KEDA ScaledObject for target, using KEDA surge strategy",
				"scaledObject", scaledObj.GetName(), "target", targetName)
			return &KEDASurgeApplier{client: c, scaledObject: scaledObj, target: target}, nil
		}

		// No KEDA — check for standalone HPA
		hpa, err := findHPAForTarget(ctx, c, namespace, targetName, targetKind)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("checking for HPA: %w", err)
		}
		if hpa != nil {
			logger.Info("Found standalone HPA for target, using HPA surge strategy",
				"hpa", hpa.Name, "target", targetName)
			return &HPASurgeApplier{client: c, hpa: hpa, target: target}, nil
		}
	}

	// No autoscaler found — surge by modifying deployment replicas directly.
	logger.V(1).Info("No KEDA or HPA found, using deployment surge strategy", "target", targetName)
	return &DeploymentSurgeApplier{client: c, target: target}, nil
}

// resolveSurgeOwner picks the applier that actually performed the surge, at TEARDOWN time, by
// which object still carries our surge marker — independent of current topology. Unlike
// detectSurgeApplier (which keys off live topology and is right at apply time, when nothing is
// marked yet), the topology may have changed since the surge — an HPA/KEDA object added or
// removed — so live detection can mis-select on teardown and strand the object we actually
// surged. Matching the marker instead always reverts the right object. Precedence mirrors
// ApplySurge's own object choice: KEDA and HPA mark their autoscaler object; a plain-Deployment
// surge marks the Deployment. Returns (nil, nil) when nothing carries our marker — nobody owns
// an active surge, so there is nothing to revert.
func resolveSurgeOwner(ctx context.Context, c client.Client, namespace, targetName, targetKind string, target Surger) (SurgeApplier, error) {
	if strings.EqualFold(targetKind, ResourceTypeDeployment) {
		so, err := findScaledObjectForTarget(ctx, c, namespace, targetName, targetKind)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, err
		}
		if so != nil && hasSurgeMarker(so.GetAnnotations()) {
			return &KEDASurgeApplier{client: c, scaledObject: so, target: target}, nil
		}
		hpa, err := findHPAForTarget(ctx, c, namespace, targetName, targetKind)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, err
		}
		if hpa != nil && hasSurgeMarker(hpa.GetAnnotations()) {
			return &HPASurgeApplier{client: c, hpa: hpa, target: target}, nil
		}
	}
	if hasTargetAnnotation(target) {
		return &DeploymentSurgeApplier{client: c, target: target}, nil
	}
	// No object carries our marker: nobody owns an active surge, so there is nothing to
	// revert. This is a valid, non-error outcome the caller handles explicitly.
	return nil, nil //nolint:nilnil
}

// hasSurgeMarker reports whether the given annotations carry our active-surge marker.
func hasSurgeMarker(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	_, ok := annotations[EvictionSurgeReplicasAnnotationKey]
	return ok
}

// hasTargetAnnotationWithValue checks if the target has the evictionSurgeReplicas annotation
// with the expected value. Used by DeploymentSurgeApplier for idempotency checks.
func hasTargetAnnotationWithValue(target Surger, value string) bool {
	annotations := target.Obj().GetAnnotations()
	if annotations == nil {
		return false
	}
	v, exists := annotations[EvictionSurgeReplicasAnnotationKey]
	return exists && v == value
}

// hasTargetAnnotation checks if the target has the evictionSurgeReplicas annotation (any value).
func hasTargetAnnotation(target Surger) bool {
	annotations := target.Obj().GetAnnotations()
	if annotations == nil {
		return false
	}
	_, exists := annotations[EvictionSurgeReplicasAnnotationKey]
	return exists
}

// ownsActiveSurge reports whether the applier still owns an active surge we may safely revert.
// A plain-Deployment surge IS the deployment's replica count, so a live count that no longer
// matches what we recorded means a partner has taken over — reverting would fight them (and
// could scale them down), so we require an exact match. For HPA/KEDA the surge is a floor on
// the autoscaler object and RevertSurge is a safe, idempotent reset of that floor (the HPA
// legitimately moves deployment replicas), so an active surge marker alone is sufficient.
func ownsActiveSurge(target Surger, surgeApplier SurgeApplier) bool {
	if !surgeApplier.IsSurgeActive() {
		return false
	}
	if _, isDeployment := surgeApplier.(*DeploymentSurgeApplier); isDeployment {
		recorded, ok := surgeApplier.RecordedSurge()
		return ok && target.GetReplicas() == recorded
	}
	return true
}

// --- DeploymentSurgeApplier ---
// Surges by modifying the deployment/statefulset spec.replicas directly.
// This is the default strategy when no KEDA or HPA is present.

type DeploymentSurgeApplier struct {
	client client.Client
	target Surger
}

var _ SurgeApplier = &DeploymentSurgeApplier{}

func (d *DeploymentSurgeApplier) ApplySurge(ctx context.Context, surgeReplicas int32) error {
	// Persist the pre-surge baseline on the deployment (once) so it survives loss of the
	// EvictionAutoScaler — otherwise the baseline lives only on the CR's Status.MinReplicas
	// and an EAS deleted/recreated mid-surge would revert to a wrong (zero) value. Mirrors
	// the original-min-replicas annotation the HPA/KEDA appliers already write.
	if anns := d.target.Obj().GetAnnotations(); anns == nil || anns[OriginalMinReplicasAnnotationKey] == "" {
		d.target.AddAnnotation(OriginalMinReplicasAnnotationKey, strconv.FormatInt(int64(d.target.GetReplicas()), 10))
	}
	d.target.SetReplicas(surgeReplicas)
	d.target.AddAnnotation(EvictionSurgeReplicasAnnotationKey, strconv.FormatInt(int64(surgeReplicas), 10))
	return d.client.Update(ctx, d.target.Obj())
}

func (d *DeploymentSurgeApplier) RevertSurge(ctx context.Context, originalMinReplicas int32) error {
	// Prefer the durable baseline recorded on the deployment over the passed value, so a
	// revert driven by an EAS with a lost/zero Status.MinReplicas still returns to the true
	// pre-surge count. Only trust the annotation when it parses to a positive value — a
	// tampered/corrupt non-positive annotation must not override a valid passed baseline and
	// wedge the deployment surged via the guard below.
	revertTo := originalMinReplicas
	if anns := d.target.Obj().GetAnnotations(); anns != nil {
		if v, ok := anns[OriginalMinReplicasAnnotationKey]; ok {
			if parsed, err := strconv.ParseInt(v, 10, 32); err == nil && parsed > 0 {
				revertTo = int32(parsed)
			}
		}
	}
	// Guard: refuse a non-positive baseline — leave it surged (over-provisioned but safe)
	// rather than scaling to 0. Strict > 0 here, unlike HPA/KEDA which honor a *recorded* 0.
	// The distinction is intent: minReplicaCount/minReplicas: 0 is a floor a partner can
	// deliberately configure, so a recorded 0 there is real (a bare fallback 0 is still
	// refused). A Deployment has no such setting — nothing lets a partner declare 0 as a
	// baseline — so a 0 is only ever a lost/raced/tampered value, never intent; refuse it.
	if revertTo <= 0 {
		log.FromContext(ctx).Error(nil, "refusing to revert surge to a non-positive baseline; leaving deployment surged",
			"target", d.target.Obj().GetName(), "namespace", d.target.Obj().GetNamespace(), "revertTo", revertTo)
		return nil
	}
	d.target.SetReplicas(revertTo)
	d.target.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
	d.target.RemoveAnnotation(OriginalMinReplicasAnnotationKey)
	return d.client.Update(ctx, d.target.Obj())
}

func (d *DeploymentSurgeApplier) Name() string {
	return "deployment"
}

func (d *DeploymentSurgeApplier) IsSurgeActive() bool {
	return hasTargetAnnotation(d.target)
}

func (d *DeploymentSurgeApplier) RecordedSurge() (int32, bool) {
	return recordedSurgeFromAnnotations(d.target.Obj().GetAnnotations())
}

func (d *DeploymentSurgeApplier) RecordedBaseline() (int32, bool) {
	return recordedBaselineFromAnnotations(d.target.Obj().GetAnnotations())
}
