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
	// Name returns a human-readable name for logging
	Name() string
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

// --- DeploymentSurgeApplier ---
// Surges by modifying the deployment/statefulset spec.replicas directly.
// This is the default strategy when no KEDA or HPA is present.

type DeploymentSurgeApplier struct {
	client client.Client
	target Surger
}

var _ SurgeApplier = &DeploymentSurgeApplier{}

func (d *DeploymentSurgeApplier) ApplySurge(ctx context.Context, surgeReplicas int32) error {
	d.target.SetReplicas(surgeReplicas)
	d.target.AddAnnotation(EvictionSurgeReplicasAnnotationKey, strconv.FormatInt(int64(surgeReplicas), 10))
	return d.client.Update(ctx, d.target.Obj())
}

func (d *DeploymentSurgeApplier) RevertSurge(ctx context.Context, originalMinReplicas int32) error {
	d.target.SetReplicas(originalMinReplicas)
	d.target.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
	return d.client.Update(ctx, d.target.Obj())
}

func (d *DeploymentSurgeApplier) Name() string {
	return "deployment"
}

func (d *DeploymentSurgeApplier) IsSurgeActive() bool {
	return hasTargetAnnotation(d.target)
}
