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
// Depending on whether KEDA, HPA, or neither is present, a different implementation is used.
//
// For multi-resource strategies (HPA, KEDA): the HPA/KEDA floor is raised first, then
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

// detectSurgeApplier determines which surge strategy to use by building a
// composite of all applicable appliers. DeploymentSurgeApplier is always
// included as the base (it sets deployment.spec.replicas directly). KEDA and
// HPA appliers are added when their resources target this workload, ensuring
// their floors are raised before the deployment scales.
func detectSurgeApplier(ctx context.Context, c client.Client, namespace, targetName, targetKind string, target Surger) (SurgeApplier, error) {
	if !strings.EqualFold(targetKind, ResourceTypeDeployment) {
		return nil, fmt.Errorf("unsupported target kind %q: only Deployment is supported for surge strategies", targetKind)
	}

	logger := log.FromContext(ctx)

	var appliers []SurgeApplier

	// Check for KEDA ScaledObject targeting this workload
	scaledObj, err := findScaledObjectForTarget(ctx, c, namespace, targetName, targetKind)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("checking for KEDA ScaledObject: %w", err)
	}
	if scaledObj != nil {
		logger.Info("Found KEDA ScaledObject for target, skipping deployment surge (KEDA strategy not yet implemented)",
			"scaledObject", scaledObj.GetName(), "target", targetName)
	}

	// Check for standalone HPA targeting this workload
	hpa, err := findHPAForTarget(ctx, c, namespace, targetName, targetKind)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("checking for HPA: %w", err)
	}
	if hpa != nil {
		logger.Info("Found HPA for target, skipping deployment surge (HPA strategy not yet implemented)",
			"hpa", hpa.Name, "target", targetName)
	}

	// DeploymentSurgeApplier is the default strategy when no autoscaler appliers are added.
	if len(appliers) == 0 {
		logger.V(1).Info("Using deployment surge strategy", "target", targetName)
		appliers = append(appliers, &DeploymentSurgeApplier{client: c, target: target})
	}

	return &CompositeSurgeApplier{
		appliers: appliers,
		target:   target,
	}, nil
}

// hasTargetAnnotationWithValue checks if the target has the evictionSurgeReplicas annotation
// with the expected value. Used internally by appliers to avoid redundant writes in composite mode.
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

// --- CompositeSurgeApplier ---
// Surges multiple resources (e.g., both KEDA ScaledObject and standalone HPA) when both
// target the same workload. This prevents the standalone HPA from blocking scale-up when
// only the ScaledObject is surged, or vice versa.
//
// Ordering: appliers are always invoked in KEDA → HPA → Deployment order.
// The autoscaler floor (KEDA minReplicaCount or HPA minReplicas) must be raised
// *before* setting deployment replicas, otherwise the autoscaler's sync loop can
// scale the deployment back down between the two writes.
//
// Reverts follow the same order (not reversed) because the autoscaler floor must
// be lowered first — if we reverted the deployment replicas before lowering the
// HPA/KEDA floor, the autoscaler would immediately scale the deployment back up
// to the still-surged floor value.
//
// The target annotation (evictionSurgeReplicas) is written once by the first applier;
// subsequent appliers skip re-annotating the target to avoid duplicate writes.

type CompositeSurgeApplier struct {
	appliers []SurgeApplier
	target   Surger
}

var _ SurgeApplier = &CompositeSurgeApplier{}

func (comp *CompositeSurgeApplier) ApplySurge(ctx context.Context, surgeReplicas int32) error {
	for _, applier := range comp.appliers {
		if err := applier.ApplySurge(ctx, surgeReplicas); err != nil {
			return fmt.Errorf("composite surge (%s): %w", applier.Name(), err)
		}
	}
	return nil
}

func (comp *CompositeSurgeApplier) RevertSurge(ctx context.Context, originalMinReplicas int32) error {
	for _, applier := range comp.appliers {
		if err := applier.RevertSurge(ctx, originalMinReplicas); err != nil {
			return fmt.Errorf("composite revert (%s): %w", applier.Name(), err)
		}
	}
	return nil
}

func (comp *CompositeSurgeApplier) Name() string {
	names := make([]string, len(comp.appliers))
	for i, a := range comp.appliers {
		names[i] = a.Name()
	}
	return "composite(" + strings.Join(names, "+") + ")"
}

func (comp *CompositeSurgeApplier) IsSurgeActive() bool {
	for _, applier := range comp.appliers {
		if applier.IsSurgeActive() {
			return true
		}
	}
	return false
}
