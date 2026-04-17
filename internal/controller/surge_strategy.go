package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const maxConflictRetries = 3

// reGetAndUpdateTarget re-fetches the target to get the latest resourceVersion,
// applies the given mutate function, and retries on conflict up to maxConflictRetries times.
//
// This is needed because HPA/KEDA ApplySurge and RevertSurge are two-step operations:
// step 1 updates the HPA/ScaledObject, step 2 updates the deployment. Between these steps,
// the HPA controller may modify the deployment via the /scale subresource, changing its
// resourceVersion. If we used the stale resourceVersion from the start of the reconcile,
// the deployment update would fail with a 409 Conflict (optimistic concurrency).
// Re-fetching gets the latest resourceVersion, and retrying handles the case where
// the HPA races us again between the re-fetch and the update.
func reGetAndUpdateTarget(ctx context.Context, c client.Client, target Surger, mutate func()) error {
	for i := 0; i < maxConflictRetries; i++ {
		// Re-fetch to get fresh resourceVersion
		obj := target.Obj()
		if err := c.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, obj); err != nil {
			return fmt.Errorf("re-fetching target: %w", err)
		}
		mutate()
		// Use target.Obj() after mutate because SetReplicas may replace the underlying
		// object via DeepCopy, making the captured 'obj' pointer stale.
		err := c.Update(ctx, target.Obj())
		if err == nil {
			return nil
		}
		if !kerrors.IsConflict(err) {
			return err
		}
		log.FromContext(ctx).V(1).Info("Conflict updating target, retrying", "attempt", i+1)
	}
	return fmt.Errorf("failed to update target after %d conflict retries", maxConflictRetries)
}

// SurgeApplier abstracts the mechanism for temporarily increasing minimum replicas.
// Depending on whether KEDA, HPA, or neither is present, a different implementation is used.
//
// For multi-resource strategies (HPA, KEDA): the HPA/KEDA floor is raised first, then
// deployment replicas are set directly for immediate effect. On failure, the reconcile
// loop retries ApplySurge idempotently until the deployment write succeeds.
type SurgeApplier interface {
	// ApplySurge sets the minimum replica count to surgeReplicas.
	// Callers may invoke this multiple times; implementations must be idempotent.
	ApplySurge(ctx context.Context, c client.Client, surgeReplicas int32) error
	// RevertSurge restores the original minimum replica count.
	RevertSurge(ctx context.Context, c client.Client, originalMinReplicas int32) error
	// Name returns a human-readable name for logging
	Name() string
}

// detectSurgeApplier determines which surge strategy to use by building a
// composite of all applicable appliers. DeploymentSurgeApplier is always
// included as the base (it sets deployment.spec.replicas directly). KEDA and
// HPA appliers are added when their resources target this workload, ensuring
// their floors are raised before the deployment scales.
func detectSurgeApplier(ctx context.Context, c client.Client, namespace, targetName, targetKind string, target Surger) (SurgeApplier, error) {
	logger := log.FromContext(ctx)

	var appliers []SurgeApplier

	// Check for KEDA ScaledObject targeting this workload
	scaledObj, err := findScaledObjectForTarget(ctx, c, namespace, targetName, targetKind)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("checking for KEDA ScaledObject: %w", err)
	}
	if scaledObj != nil {
		logger.Info("Found KEDA ScaledObject for target, adding KEDA surge applier",
			"scaledObject", scaledObj.GetName(), "target", targetName)
		appliers = append(appliers, &KEDASurgeApplier{scaledObject: scaledObj, target: target})
	}

	// Check for standalone HPA targeting this workload
	hpa, err := findHPAForTarget(ctx, c, namespace, targetName, targetKind)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("checking for HPA: %w", err)
	}
	if hpa != nil {
		logger.Info("Found HPA for target, adding HPA surge applier",
			"hpa", hpa.Name, "target", targetName)
		appliers = append(appliers, &HPASurgeApplier{hpa: hpa, target: target})
	}

	// DeploymentSurgeApplier is only needed when no autoscaler appliers were added.
	// HPA/KEDA appliers already handle setting deployment replicas and the surge
	// annotation internally (via reGetAndUpdateTarget), so adding DeploymentSurgeApplier
	// alongside them would cause a stale-object conflict on the second Update.
	if len(appliers) == 0 {
		logger.V(1).Info("No KEDA or HPA found, using deployment surge strategy", "target", targetName)
		appliers = append(appliers, &DeploymentSurgeApplier{target: target})
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
	target Surger
}

var _ SurgeApplier = &DeploymentSurgeApplier{}

func (d *DeploymentSurgeApplier) ApplySurge(ctx context.Context, c client.Client, surgeReplicas int32) error {
	d.target.SetReplicas(surgeReplicas)
	d.target.AddAnnotation(EvictionSurgeReplicasAnnotationKey, strconv.FormatInt(int64(surgeReplicas), 10))
	return c.Update(ctx, d.target.Obj())
}

func (d *DeploymentSurgeApplier) RevertSurge(ctx context.Context, c client.Client, originalMinReplicas int32) error {
	d.target.SetReplicas(originalMinReplicas)
	d.target.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
	return c.Update(ctx, d.target.Obj())
}

func (d *DeploymentSurgeApplier) Name() string {
	return "deployment"
}

// --- HPASurgeApplier ---
// Surges by temporarily increasing HPA spec.minReplicas.

type HPASurgeApplier struct {
	hpa    *autoscalingv2.HorizontalPodAutoscaler
	target Surger
}

var _ SurgeApplier = &HPASurgeApplier{}

func (h *HPASurgeApplier) ApplySurge(ctx context.Context, c client.Client, surgeReplicas int32) error {
	logger := log.FromContext(ctx)

	// Step 1: Update HPA minReplicas first to raise the floor.
	// This must happen before setting deployment replicas to prevent a race where
	// the HPA sync loop scales the deployment back down between the two writes.
	hpa := h.hpa.DeepCopy()
	hpa.Spec.MinReplicas = &surgeReplicas
	h.hpa = hpa
	if err := c.Update(ctx, hpa); err != nil {
		return fmt.Errorf("updating HPA minReplicas: %w", err)
	}
	logger.V(1).Info("Updated HPA minReplicas", "minReplicas", surgeReplicas)

	// Step 2: Set deployment replicas directly for immediate scale-up and annotate
	// with surge intent marker. This avoids waiting for the HPA sync loop (~15s)
	// and handles the case where the HPA cannot compute metrics (e.g., no metrics-server).
	// Skip if already annotated (e.g., by another applier in a composite).
	surgeVal := strconv.FormatInt(int64(surgeReplicas), 10)
	if !hasTargetAnnotationWithValue(h.target, surgeVal) {
		if err := reGetAndUpdateTarget(ctx, c, h.target, func() {
			if h.target.GetReplicas() != surgeReplicas {
				h.target.SetReplicas(surgeReplicas)
			}
			h.target.AddAnnotation(EvictionSurgeReplicasAnnotationKey, surgeVal)
		}); err != nil {
			return fmt.Errorf("setting replicas and annotating target: %w", err)
		}
		logger.V(1).Info("Set replicas and annotated target with surge intent", "replicas", surgeReplicas)
	}
	return nil
}

func (h *HPASurgeApplier) RevertSurge(ctx context.Context, c client.Client, originalMinReplicas int32) error {
	logger := log.FromContext(ctx)

	// Step 1: Revert HPA minReplicas using EA.Status.MinReplicas (the effective floor)
	hpa := h.hpa.DeepCopy()
	hpa.Spec.MinReplicas = &originalMinReplicas
	h.hpa = hpa
	if err := c.Update(ctx, hpa); err != nil {
		return fmt.Errorf("reverting HPA minReplicas: %w", err)
	}
	logger.V(1).Info("Reverted HPA minReplicas", "originalMin", originalMinReplicas)

	// Step 2: Remove surge annotation and set replicas directly for immediate scale-down.
	// This avoids waiting for the HPA sync loop to enforce the reverted minReplicas.
	if hasTargetAnnotation(h.target) {
		if err := reGetAndUpdateTarget(ctx, c, h.target, func() {
			if h.target.GetReplicas() != originalMinReplicas {
				h.target.SetReplicas(originalMinReplicas)
			}
			h.target.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
		}); err != nil {
			return fmt.Errorf("removing surge annotation from target: %w", err)
		}
	}
	return nil
}

func (h *HPASurgeApplier) Name() string {
	return "hpa"
}

// --- KEDASurgeApplier ---
// Surges by temporarily increasing ScaledObject spec.minReplicaCount.

type KEDASurgeApplier struct {
	scaledObject *unstructured.Unstructured
	target       Surger
}

var _ SurgeApplier = &KEDASurgeApplier{}

func (k *KEDASurgeApplier) ApplySurge(ctx context.Context, c client.Client, surgeReplicas int32) error {
	logger := log.FromContext(ctx)

	// Step 1: Update ScaledObject minReplicaCount first to raise the floor.
	// This must happen before setting deployment replicas to prevent a race where
	// KEDA/HPA scales the deployment back down between the two writes.
	obj := k.scaledObject.DeepCopy()
	if err := unstructured.SetNestedField(obj.Object, int64(surgeReplicas), "spec", "minReplicaCount"); err != nil {
		return fmt.Errorf("setting minReplicaCount: %w", err)
	}
	k.scaledObject = obj
	if err := c.Update(ctx, obj); err != nil {
		return fmt.Errorf("updating ScaledObject minReplicaCount: %w", err)
	}
	logger.V(1).Info("Updated ScaledObject minReplicaCount", "minReplicaCount", surgeReplicas)

	// Step 2: Set deployment replicas directly for immediate scale-up and annotate
	// with surge intent marker. This avoids waiting for the KEDA→HPA sync loop
	// and handles the case where the HPA cannot compute metrics (e.g., no metrics-server).
	// Skip if already annotated (e.g., by another applier in a composite).
	surgeVal := strconv.FormatInt(int64(surgeReplicas), 10)
	if !hasTargetAnnotationWithValue(k.target, surgeVal) {
		if err := reGetAndUpdateTarget(ctx, c, k.target, func() {
			if k.target.GetReplicas() != surgeReplicas {
				k.target.SetReplicas(surgeReplicas)
			}
			k.target.AddAnnotation(EvictionSurgeReplicasAnnotationKey, surgeVal)
		}); err != nil {
			return fmt.Errorf("setting replicas and annotating target: %w", err)
		}
		logger.V(1).Info("Set replicas and annotated target with surge intent", "replicas", surgeReplicas)
	}
	return nil
}

func (k *KEDASurgeApplier) RevertSurge(ctx context.Context, c client.Client, originalMinReplicas int32) error {
	logger := log.FromContext(ctx)

	// Step 1: Revert ScaledObject minReplicaCount using EA.Status.MinReplicas (the effective floor)
	obj := k.scaledObject.DeepCopy()
	if err := unstructured.SetNestedField(obj.Object, int64(originalMinReplicas), "spec", "minReplicaCount"); err != nil {
		return fmt.Errorf("restoring minReplicaCount: %w", err)
	}

	k.scaledObject = obj
	if err := c.Update(ctx, obj); err != nil {
		return fmt.Errorf("reverting ScaledObject minReplicaCount: %w", err)
	}
	logger.V(1).Info("Reverted ScaledObject minReplicaCount", "originalMin", originalMinReplicas)

	// Step 2: Remove surge annotation and set replicas directly for immediate scale-down.
	// This avoids waiting for the KEDA→HPA sync loop to enforce the reverted minReplicaCount.
	if hasTargetAnnotation(k.target) {
		if err := reGetAndUpdateTarget(ctx, c, k.target, func() {
			if k.target.GetReplicas() != originalMinReplicas {
				k.target.SetReplicas(originalMinReplicas)
			}
			k.target.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
		}); err != nil {
			return fmt.Errorf("removing surge annotation from target: %w", err)
		}
	}
	return nil
}

func (k *KEDASurgeApplier) Name() string {
	return "keda"
}

// --- CompositeSurgeApplier ---
// Surges multiple resources (e.g., both KEDA ScaledObject and standalone HPA) when both
// target the same workload. This prevents the standalone HPA from blocking scale-up when
// only the ScaledObject is surged, or vice versa.
// The target annotation (evictionSurgeReplicas) is written once by the first applier;
// subsequent appliers skip re-annotating the target to avoid duplicate writes.

type CompositeSurgeApplier struct {
	appliers []SurgeApplier
	target   Surger
}

var _ SurgeApplier = &CompositeSurgeApplier{}

func (comp *CompositeSurgeApplier) ApplySurge(ctx context.Context, c client.Client, surgeReplicas int32) error {
	for _, applier := range comp.appliers {
		if err := applier.ApplySurge(ctx, c, surgeReplicas); err != nil {
			return fmt.Errorf("composite surge (%s): %w", applier.Name(), err)
		}
	}
	return nil
}

func (comp *CompositeSurgeApplier) RevertSurge(ctx context.Context, c client.Client, originalMinReplicas int32) error {
	for _, applier := range comp.appliers {
		if err := applier.RevertSurge(ctx, c, originalMinReplicas); err != nil {
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
