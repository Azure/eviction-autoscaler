package controllers

import (
	"context"
	"fmt"
	"strconv"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// --- KEDASurgeApplier ---
//
// Surges by temporarily increasing ScaledObject spec.minReplicaCount and setting
// deployment replicas directly for immediate effect.
//
// Why we update deployment replicas in addition to ScaledObject minReplicaCount:
//
// KEDA manages an HPA under the hood. When the ScaledObject's minReplicaCount is
// raised, KEDA propagates this to the managed HPA's minReplicas on its next
// reconcile. The HPA then enforces it — but only after successfully computing
// metrics. This adds two hops of latency (KEDA sync → HPA sync) and still depends
// on metrics availability.
//
// Setting deployment.spec.replicas directly triggers pod creation immediately,
// bypassing both KEDA and HPA sync loops. The raised minReplicaCount prevents
// KEDA/HPA from scaling back down below the surge value.
//
// Note: the HPA controller (which KEDA manages) uses the /scale subresource to set
// deployment replicas, which is equivalent to updating deployment.spec.replicas
// directly. The /scale subresource does NOT bump deployment metadata.generation,
// while a full Update does (if spec changes).

type KEDASurgeApplier struct {
	client       client.Client
	scaledObject *kedav1alpha1.ScaledObject
	target       Surger
}

var _ SurgeApplier = &KEDASurgeApplier{}

func (k *KEDASurgeApplier) ApplySurge(ctx context.Context, surgeReplicas int32) error {
	logger := log.FromContext(ctx)
	surgeVal := strconv.FormatInt(int64(surgeReplicas), 10)

	// Step 1: Update ScaledObject minReplicaCount and annotate the ScaledObject with both:
	//   - evictionSurgeReplicas: the surged value (surge marker)
	//   - original-min-replicas: the pre-surge value (for revert)
	// Skip if ScaledObject is already surged to the target value (idempotent on retry).
	// Annotations are on the ScaledObject (not the deployment) so we don't modify the
	// deployment's metadata, avoiding unnecessary generation tracking complexity.
	annotations := k.scaledObject.GetAnnotations()
	if annotations == nil || annotations[EvictionSurgeReplicasAnnotationKey] != surgeVal {
		logger.Info("Surging KEDA ScaledObject",
			"scaledObject", k.scaledObject.GetName(),
			"namespace", k.scaledObject.GetNamespace(),
			"targetMinReplicaCount", surgeReplicas)
		obj := k.scaledObject.DeepCopy()
		// When minReplicaCount is not set, KEDA defaults it to 0 (scale-to-zero).
		// TODO: Consider skipping PDB/EA creation for ScaledObjects with
		// minReplicaCount=0 (scale-to-zero workloads) in the deployment-to-pdb controller.
		originalMin := int32(0) // KEDA default when minReplicaCount is not set
		if obj.Spec.MinReplicaCount != nil {
			originalMin = *obj.Spec.MinReplicaCount
		}

		obj.Spec.MinReplicaCount = &surgeReplicas
		if obj.Annotations == nil {
			obj.Annotations = make(map[string]string)
		}
		obj.Annotations[EvictionSurgeReplicasAnnotationKey] = surgeVal
		obj.Annotations[OriginalMinReplicasAnnotationKey] = strconv.FormatInt(int64(originalMin), 10)

		k.scaledObject = obj
		if err := k.client.Update(ctx, obj); err != nil {
			return fmt.Errorf("updating ScaledObject minReplicaCount and annotations: %w", err)
		}
		logger.V(1).Info("Updated ScaledObject minReplicaCount and annotated with surge intent",
			"minReplicaCount", surgeReplicas, "originalMin", originalMin)
	}

	// Step 2: Set deployment replicas directly for immediate scale-up.
	// This is a time-saving optimization — KEDA would eventually propagate
	// minReplicaCount to its managed HPA, which would then enforce it on its
	// next sync. But that adds two hops of latency. Setting replicas directly
	// triggers pod creation immediately. Also handles the edge case where the
	// HPA cannot compute metrics (e.g., no metrics-server).
	// On 409 Conflict (stale resourceVersion), the error propagates to the
	// reconcile loop which requeues. On retry, step 1 is skipped (idempotent)
	// and step 2 retries with a fresh object from the informer cache.
	if k.target.GetReplicas() != surgeReplicas {
		k.target.SetReplicas(surgeReplicas)
		if err := k.client.Update(ctx, k.target.Obj()); err != nil {
			return fmt.Errorf("setting deployment replicas: %w", err)
		}
		logger.V(1).Info("Set deployment replicas for immediate surge", "replicas", surgeReplicas)
	}

	return nil
}

func (k *KEDASurgeApplier) RevertSurge(ctx context.Context, originalMinReplicas int32) error {
	logger := log.FromContext(ctx)

	// Read the original minReplicaCount from the ScaledObject annotation if available.
	// The annotation takes priority over EA.Status.MinReplicas because:
	//   1. It's self-describing — the ScaledObject carries its own revert value
	//   2. It was set atomically with the surge in ApplySurge
	//   3. EA.Status.MinReplicas could be stale if the controller restarted
	// Falls back to the passed-in originalMinReplicas (from EA.Status) if
	// the annotation is missing (e.g., manual annotation removal).
	revertTo := originalMinReplicas
	annotations := k.scaledObject.GetAnnotations()
	if annotations != nil {
		if val, exists := annotations[OriginalMinReplicasAnnotationKey]; exists {
			if parsed, err := strconv.ParseInt(val, 10, 32); err == nil {
				revertTo = int32(parsed)
			}
		}
	}

	// Revert ScaledObject minReplicaCount and remove both surge annotations in a single write.
	obj := k.scaledObject.DeepCopy()
	obj.Spec.MinReplicaCount = &revertTo
	delete(obj.Annotations, EvictionSurgeReplicasAnnotationKey)
	delete(obj.Annotations, OriginalMinReplicasAnnotationKey)

	k.scaledObject = obj
	if err := k.client.Update(ctx, obj); err != nil {
		return fmt.Errorf("reverting ScaledObject minReplicaCount and removing annotations: %w", err)
	}
	logger.Info("Reverted KEDA ScaledObject surge",
		"scaledObject", k.scaledObject.GetName(),
		"namespace", k.scaledObject.GetNamespace(),
		"revertTo", revertTo)

	// Don't set deployment replicas directly — let KEDA/HPA handle the scale-down
	// on their next sync. The eviction has already completed so there is no urgency.
	// If KEDA/HPA cannot compute metrics, the deployment will stay at the surged
	// replica count, which is safe (just over-provisioned).
	if k.target.GetReplicas() > revertTo {
		logger.Info("Deployment replicas still above baseline after KEDA revert, "+
			"waiting for KEDA/HPA to scale down on next sync",
			"currentReplicas", k.target.GetReplicas(),
			"originalMinReplicaCount", revertTo,
			"scaledObject", k.scaledObject.GetName())
	}
	return nil
}

func (k *KEDASurgeApplier) Name() string {
	return "keda"
}

func (k *KEDASurgeApplier) IsSurgeActive() bool {
	annotations := k.scaledObject.GetAnnotations()
	if annotations != nil {
		if _, exists := annotations[EvictionSurgeReplicasAnnotationKey]; exists {
			return true
		}
	}
	return false
}
