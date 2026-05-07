package controllers

import (
	"context"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	scaledObject *unstructured.Unstructured
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
		// If someone intentionally has minReplicaCount unset for scale-to-zero,
		// creating a PDB/EvictionAutoScaler may not be appropriate — but that
		// decision belongs in the PDB creation logic, not here. If we get here,
		// we have an EA and should surge.
		// TODO: Consider skipping PDB/EA creation for ScaledObjects with
		// minReplicaCount=0 (scale-to-zero workloads) in the deployment-to-pdb controller.
		originalMin := int64(0) // KEDA default when minReplicaCount is not set
		if val, found, _ := unstructured.NestedInt64(obj.Object, "spec", "minReplicaCount"); found {
			originalMin = val
		}

		if err := unstructured.SetNestedField(obj.Object, int64(surgeReplicas), "spec", "minReplicaCount"); err != nil {
			return fmt.Errorf("setting minReplicaCount: %w", err)
		}
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = make(map[string]string)
		}
		ann[EvictionSurgeReplicasAnnotationKey] = surgeVal
		ann[OriginalMinReplicasAnnotationKey] = strconv.FormatInt(originalMin, 10)
		obj.SetAnnotations(ann)

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
	revertTo := int64(originalMinReplicas)
	annotations := k.scaledObject.GetAnnotations()
	if annotations != nil {
		if val, exists := annotations[OriginalMinReplicasAnnotationKey]; exists {
			if parsed, err := strconv.ParseInt(val, 10, 64); err == nil {
				revertTo = parsed
			}
		}
	}

	// Revert ScaledObject minReplicaCount and remove both surge annotations in a single write.
	obj := k.scaledObject.DeepCopy()
	if err := unstructured.SetNestedField(obj.Object, revertTo, "spec", "minReplicaCount"); err != nil {
		return fmt.Errorf("restoring minReplicaCount: %w", err)
	}
	ann := obj.GetAnnotations()
	delete(ann, EvictionSurgeReplicasAnnotationKey)
	delete(ann, OriginalMinReplicasAnnotationKey)
	obj.SetAnnotations(ann)

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
	if k.target.GetReplicas() > int32(revertTo) {
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
