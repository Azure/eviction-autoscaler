package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var errNotFound = errors.New("not found")

// SurgeApplier abstracts the mechanism for temporarily increasing minimum replicas.
// Depending on whether KEDA, HPA, or neither is present, a different implementation is used.
//
// For multi-resource strategies (HPA, KEDA): writes are ordered for safe partial-failure recovery.
// ApplySurge: annotates the target first (intent marker), then updates the HPA/ScaledObject.
// RevertSurge: reverts the HPA/ScaledObject first, then removes the target annotation.
// This ensures that on retry after a crash between the two writes, the controller can detect
// the incomplete state and re-drive the operation to completion.
type SurgeApplier interface {
	// ApplySurge sets the minimum replica count to surgeReplicas.
	// Callers may invoke this multiple times; implementations must be idempotent.
	ApplySurge(ctx context.Context, c client.Client, surgeReplicas int32) error
	// RevertSurge restores the original minimum replica count.
	// For deployment strategy, originalMinReplicas is used; for HPA/KEDA, the stored annotation is used.
	RevertSurge(ctx context.Context, c client.Client, originalMinReplicas int32) error
	// IsSurgeComplete returns true only when the surge intent annotation exists on the
	// target AND every external resource (HPA, ScaledObject) has been updated to match.
	// A false return with a present intent annotation signals a partial write that must
	// be re-driven.
	IsSurgeComplete() bool
	// Name returns a human-readable name for logging
	Name() string
}

// detectSurgeApplier determines which surge strategy to use:
// 1. If KEDA ScaledObject targets the workload → KEDASurgeApplier (modify minReplicaCount)
// 2. If HPA targets the workload → HPASurgeApplier (modify minReplicas)
// 3. Otherwise → DeploymentSurgeApplier (modify deployment.spec.replicas)
func detectSurgeApplier(ctx context.Context, c client.Client, namespace, targetName, targetKind string, target Surger) (SurgeApplier, error) {
	logger := log.FromContext(ctx)

	// 1. Check for KEDA ScaledObject targeting this workload
	scaledObj, err := findScaledObjectForTarget(ctx, c, namespace, targetName, targetKind)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("checking for KEDA ScaledObject: %w", err)
	}

	// 2. Check for standalone HPA targeting this workload
	hpa, err := findHPAForTarget(ctx, c, namespace, targetName, targetKind)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("checking for HPA: %w", err)
	}

	// 3. If both KEDA and standalone HPA exist, surge both to avoid the HPA blocking scale-up
	if scaledObj != nil && hpa != nil {
		logger.Info("Found both KEDA ScaledObject and standalone HPA for target, using composite surge strategy",
			"scaledObject", scaledObj.GetName(), "hpa", hpa.Name, "target", targetName)
		return &CompositeSurgeApplier{
			appliers: []SurgeApplier{
				&KEDASurgeApplier{scaledObject: scaledObj, target: target},
				&HPASurgeApplier{hpa: hpa, target: target},
			},
			target: target,
		}, nil
	}

	// 4. KEDA only
	if scaledObj != nil {
		logger.Info("Found KEDA ScaledObject for target, using KEDA surge strategy",
			"scaledObject", scaledObj.GetName(), "target", targetName)
		return &KEDASurgeApplier{scaledObject: scaledObj, target: target}, nil
	}

	// 5. HPA only
	if hpa != nil {
		logger.Info("Found HPA for target, using HPA surge strategy",
			"hpa", hpa.Name, "target", targetName)
		return &HPASurgeApplier{hpa: hpa, target: target}, nil
	}

	// 6. Fall back to direct deployment/statefulset replica management
	logger.V(1).Info("No KEDA or HPA found, using deployment surge strategy", "target", targetName)
	return &DeploymentSurgeApplier{target: target}, nil
}

// findScaledObjectForTarget looks for a KEDA ScaledObject targeting the given workload.
// Returns nil, errNotFound if KEDA is not installed or no matching ScaledObject is found.
func findScaledObjectForTarget(ctx context.Context, c client.Client, namespace, targetName, targetKind string) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "keda.sh",
		Version: "v1alpha1",
		Kind:    "ScaledObjectList",
	})

	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, errNotFound
		}
		return nil, err
	}

	for i := range list.Items {
		item := &list.Items[i]
		scaleTargetRef, found, err := unstructured.NestedMap(item.Object, "spec", "scaleTargetRef")
		if err != nil || !found {
			continue
		}
		name, _ := scaleTargetRef["name"].(string)
		kind, _ := scaleTargetRef["kind"].(string)
		if kind == "" {
			kind = "Deployment" // KEDA default
		}
		if name == targetName && strings.EqualFold(kind, targetKind) {
			return item, nil
		}
	}

	return nil, errNotFound
}

// findHPAForTarget looks for a standalone (non-KEDA-managed) HPA targeting the given workload.
// KEDA-managed HPAs are filtered out because they are owned by the ScaledObject and should
// be controlled via the KEDA strategy instead. This avoids accidentally modifying a KEDA-managed
// HPA when the customer also has their own standalone HPA on a different deployment.
// Returns nil, errNotFound if no matching standalone HPA is found.
func findHPAForTarget(ctx context.Context, c client.Client, namespace, targetName, targetKind string) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	var hpaList autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &hpaList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		if hpa.Spec.ScaleTargetRef.Name == targetName &&
			strings.EqualFold(hpa.Spec.ScaleTargetRef.Kind, targetKind) {
			if isKEDAManagedHPA(hpa) {
				continue // skip KEDA-managed HPAs
			}
			return hpa, nil
		}
	}

	return nil, errNotFound
}

// isKEDAManagedHPA returns true if the HPA is owned/managed by KEDA.
// KEDA-managed HPAs have either an owner reference with kind "ScaledObject"
// or the label "scaledobject.keda.sh/name".
func isKEDAManagedHPA(hpa *autoscalingv2.HorizontalPodAutoscaler) bool {
	// Check for KEDA label
	if _, ok := hpa.Labels["scaledobject.keda.sh/name"]; ok {
		return true
	}
	// Check for owner reference from a ScaledObject
	for _, ref := range hpa.OwnerReferences {
		if ref.Kind == "ScaledObject" {
			return true
		}
	}
	return false
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

// ResolveMinReplicas returns the effective minimum replica count for a workload.
// Priority: KEDA ScaledObject minReplicaCount > HPA minReplicas > deployment.spec.replicas.
// This ensures PDB minAvailable reflects the true floor when an autoscaler controls replicas.
func ResolveMinReplicas(ctx context.Context, c client.Client, namespace, targetName, targetKind string, deployReplicas int32) int32 {
	logger := log.FromContext(ctx)

	// 1. Check KEDA ScaledObject
	scaledObj, err := findScaledObjectForTarget(ctx, c, namespace, targetName, targetKind)
	if err == nil && scaledObj != nil {
		if val, found, _ := unstructured.NestedInt64(scaledObj.Object, "spec", "minReplicaCount"); found && val > 0 {
			logger.V(1).Info("Using KEDA ScaledObject minReplicaCount for PDB minAvailable",
				"target", targetName, "minReplicaCount", val)
			return int32(val)
		}
	}

	// 2. Check standalone HPA
	hpa, err := findHPAForTarget(ctx, c, namespace, targetName, targetKind)
	if err == nil && hpa != nil && hpa.Spec.MinReplicas != nil {
		logger.V(1).Info("Using HPA minReplicas for PDB minAvailable",
			"target", targetName, "hpa", hpa.Name, "minReplicas", *hpa.Spec.MinReplicas)
		return *hpa.Spec.MinReplicas
	}

	// 3. Fall back to deployment replicas
	return deployReplicas
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

func (d *DeploymentSurgeApplier) IsSurgeComplete() bool {
	// For the deployment strategy, annotation and replicas are written in a single
	// atomic update, so the presence of the annotation implies completeness.
	return hasTargetAnnotation(d.target)
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

	// Step 1: Annotate the target as intent marker.
	// If we crash after this but before updating the HPA, the next reconcile
	// will see the annotation and re-drive the HPA update.
	// Skip if already annotated (e.g., by another applier in a composite).
	surgeVal := strconv.FormatInt(int64(surgeReplicas), 10)
	if !hasTargetAnnotationWithValue(h.target, surgeVal) {
		h.target.AddAnnotation(EvictionSurgeReplicasAnnotationKey, surgeVal)
		if err := c.Update(ctx, h.target.Obj()); err != nil {
			return fmt.Errorf("annotating target with surge intent: %w", err)
		}
		logger.V(1).Info("Annotated target with surge intent", "replicas", surgeReplicas)
	}

	// Step 2: Update HPA minReplicas so the HPA doesn't scale back down below the surge
	hpa := h.hpa.DeepCopy()
	hpa.Spec.MinReplicas = &surgeReplicas

	h.hpa = hpa
	return c.Update(ctx, hpa)
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

	// Step 2: Remove surge annotation from target (skip if already removed by another applier)
	if hasTargetAnnotation(h.target) {
		h.target.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
		if err := c.Update(ctx, h.target.Obj()); err != nil {
			return fmt.Errorf("removing surge annotation from target: %w", err)
		}
	}
	return nil
}

func (h *HPASurgeApplier) IsSurgeComplete() bool {
	if !hasTargetAnnotation(h.target) {
		return false
	}
	// Verify HPA minReplicas matches the surge value from the target annotation.
	// If they don't match, the HPA was never updated (partial write).
	surgeVal := h.target.Obj().GetAnnotations()[EvictionSurgeReplicasAnnotationKey]
	surgeReplicas, err := strconv.ParseInt(surgeVal, 10, 32)
	if err != nil {
		return false
	}
	return h.hpa.Spec.MinReplicas != nil && *h.hpa.Spec.MinReplicas == int32(surgeReplicas)
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

	// Step 1: Annotate the target first as intent marker.
	// Skip if already annotated (e.g., by another applier in a composite).
	surgeVal := strconv.FormatInt(int64(surgeReplicas), 10)
	if !hasTargetAnnotationWithValue(k.target, surgeVal) {
		k.target.AddAnnotation(EvictionSurgeReplicasAnnotationKey, surgeVal)
		if err := c.Update(ctx, k.target.Obj()); err != nil {
			return fmt.Errorf("annotating target with surge intent: %w", err)
		}
		logger.V(1).Info("Annotated target with surge intent", "replicas", surgeReplicas)
	}

	// Step 2: Update ScaledObject minReplicaCount
	obj := k.scaledObject.DeepCopy()
	if err := unstructured.SetNestedField(obj.Object, int64(surgeReplicas), "spec", "minReplicaCount"); err != nil {
		return fmt.Errorf("setting minReplicaCount: %w", err)
	}

	k.scaledObject = obj
	return c.Update(ctx, obj)
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

	// Step 2: Remove surge annotation from target (skip if already removed by another applier)
	if hasTargetAnnotation(k.target) {
		k.target.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
		if err := c.Update(ctx, k.target.Obj()); err != nil {
			return fmt.Errorf("removing surge annotation from target: %w", err)
		}
	}
	return nil
}

func (k *KEDASurgeApplier) IsSurgeComplete() bool {
	if !hasTargetAnnotation(k.target) {
		return false
	}
	// Verify ScaledObject minReplicaCount matches the surge value from the target annotation.
	// If they don't match, the ScaledObject was never updated (partial write).
	surgeVal := k.target.Obj().GetAnnotations()[EvictionSurgeReplicasAnnotationKey]
	surgeReplicas, err := strconv.ParseInt(surgeVal, 10, 64)
	if err != nil {
		return false
	}
	val, found, _ := unstructured.NestedInt64(k.scaledObject.Object, "spec", "minReplicaCount")
	return found && val == surgeReplicas
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

func (comp *CompositeSurgeApplier) IsSurgeComplete() bool {
	for _, a := range comp.appliers {
		if !a.IsSurgeComplete() {
			return false
		}
	}
	return true
}

func (comp *CompositeSurgeApplier) Name() string {
	names := make([]string, len(comp.appliers))
	for i, a := range comp.appliers {
		names[i] = a.Name()
	}
	return "composite(" + strings.Join(names, "+") + ")"
}
