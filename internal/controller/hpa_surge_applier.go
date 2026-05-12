package controllers

import (
	"context"
	"fmt"
	"strconv"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// --- HPASurgeApplier ---
//
// Surges by temporarily increasing HPA spec.minReplicas and setting deployment
// replicas directly for immediate effect.
//
// Why we update deployment replicas in addition to HPA minReplicas:
//
// The HPA controller only enforces minReplicas after it successfully computes a
// desired replica count from metrics. If metric computation fails (e.g., no
// metrics-server, metrics API unavailable, new deployment with no historical data),
// the HPA preserves the current replica count and does NOT scale up to minReplicas.
// This means raising HPA minReplicas alone is not enough to guarantee the surge
// takes effect — the deployment could stay at 1 replica indefinitely.
//
// Setting deployment.spec.replicas directly triggers the deployment controller to
// create new pods immediately, regardless of HPA metrics availability. The HPA's
// raised minReplicas prevents it from scaling back down below the surge value on
// its next successful metrics evaluation.
//
// Note: the HPA controller uses the /scale subresource to set deployment replicas,
// which is equivalent to updating deployment.spec.replicas directly. Both result in
// the same field being set; the /scale subresource is just a convenience API that
// avoids sending the full deployment object. However, /scale does NOT bump
// deployment metadata.generation, while a full Update does (if spec changes).

type HPASurgeApplier struct {
	client client.Client
	hpa    *autoscalingv2.HorizontalPodAutoscaler
	target Surger
}

var _ SurgeApplier = &HPASurgeApplier{}

func (h *HPASurgeApplier) ApplySurge(ctx context.Context, surgeReplicas int32) error {
	logger := log.FromContext(ctx)
	surgeVal := strconv.FormatInt(int64(surgeReplicas), 10)

	// Step 1: Update HPA minReplicas and annotate the HPA with both:
	//   - evictionSurgeReplicas: the surged value (surge marker)
	//   - original-min-replicas: the pre-surge value (for revert)
	// Skip if HPA is already surged to the target value (idempotent on retry).
	// Annotations are on the HPA (not the deployment) so we don't modify the
	// deployment's metadata, avoiding unnecessary generation tracking complexity.
	if h.hpa.Annotations == nil || h.hpa.Annotations[EvictionSurgeReplicasAnnotationKey] != surgeVal {
		hpa := h.hpa.DeepCopy()
		// When minReplicas is nil, Kubernetes defaults it to 1 for standard HPAs.
		// With KEDA, nil minReplicas can mean scale-to-zero is enabled, indicating
		// the user doesn't require minimum availability. In that case, creating a
		// PDB/EvictionAutoScaler may not be appropriate — but that decision belongs
		// in the PDB creation logic, not here. If we get here, we have an EA and
		// should surge.
		// TODO: Consider skipping PDB/EA creation for HPAs with nil minReplicas
		// (scale-to-zero workloads) in the deployment-to-pdb controller.
		originalMin := int32(1) // Kubernetes default when minReplicas is nil
		if hpa.Spec.MinReplicas != nil {
			originalMin = *hpa.Spec.MinReplicas
		}
		hpa.Spec.MinReplicas = &surgeReplicas
		if hpa.Annotations == nil {
			hpa.Annotations = make(map[string]string)
		}
		hpa.Annotations[EvictionSurgeReplicasAnnotationKey] = surgeVal
		hpa.Annotations[OriginalMinReplicasAnnotationKey] = strconv.FormatInt(int64(originalMin), 10)
		h.hpa = hpa
		if err := h.client.Update(ctx, hpa); err != nil {
			return fmt.Errorf("updating HPA minReplicas and annotations: %w", err)
		}
		logger.V(1).Info("Updated HPA minReplicas and annotated with surge intent",
			"minReplicas", surgeReplicas, "originalMin", originalMin)
	}

	// Step 2: Set deployment replicas directly for immediate scale-up.
	// This is a time-saving optimization — the HPA would eventually enforce
	// minReplicas on its next sync (~15s), but setting replicas directly
	// triggers pod creation immediately. This also handles the edge case
	// where the HPA cannot compute metrics (e.g., no metrics-server).
	// See type-level comment for full rationale.
	// On 409 Conflict (stale resourceVersion), the error propagates to the
	// reconcile loop which requeues. On retry, step 1 is skipped (idempotent)
	// and step 2 retries with a fresh object from the informer cache.
	if h.target.GetReplicas() != surgeReplicas {
		h.target.SetReplicas(surgeReplicas)
		if err := h.client.Update(ctx, h.target.Obj()); err != nil {
			return fmt.Errorf("setting deployment replicas: %w", err)
		}
		logger.V(1).Info("Set deployment replicas for immediate surge", "replicas", surgeReplicas)
	}

	return nil
}

func (h *HPASurgeApplier) RevertSurge(ctx context.Context, originalMinReplicas int32) error {
	logger := log.FromContext(ctx)

	// Read the original minReplicas from the HPA annotation if available.
	// The annotation takes priority over EA.Status.MinReplicas because:
	//   1. It's self-describing — the HPA carries its own revert value
	//   2. It was set atomically with the surge in ApplySurge
	//   3. EA.Status.MinReplicas could be stale if the controller restarted
	// Falls back to the passed-in originalMinReplicas (from EA.Status) if
	// the annotation is missing (e.g., manual annotation removal).
	revertTo := originalMinReplicas
	if h.hpa.Annotations != nil {
		if val, exists := h.hpa.Annotations[OriginalMinReplicasAnnotationKey]; exists {
			if parsed, err := strconv.ParseInt(val, 10, 32); err == nil {
				revertTo = int32(parsed)
			}
		}
	}

	// Revert HPA minReplicas and remove both surge annotations in a single write.
	hpa := h.hpa.DeepCopy()
	hpa.Spec.MinReplicas = &revertTo
	delete(hpa.Annotations, EvictionSurgeReplicasAnnotationKey)
	delete(hpa.Annotations, OriginalMinReplicasAnnotationKey)
	h.hpa = hpa
	if err := h.client.Update(ctx, hpa); err != nil {
		return fmt.Errorf("reverting HPA minReplicas and removing annotations: %w", err)
	}
	logger.V(1).Info("Reverted HPA minReplicas and removed surge annotations", "revertTo", revertTo)

	// Don't set deployment replicas directly — let HPA handle the scale-down
	// on its next sync (~15s). The eviction has already completed so there is
	// no urgency. If the HPA cannot compute metrics, the deployment will stay
	// at the surged replica count, which is safe (just over-provisioned).
	if h.target.GetReplicas() > revertTo {
		logger.Info("Deployment replicas still above baseline after HPA revert, "+
			"waiting for HPA to scale down on next sync",
			"currentReplicas", h.target.GetReplicas(),
			"originalMinReplicas", revertTo,
			"hpa", h.hpa.Name)
	}
	return nil
}

func (h *HPASurgeApplier) Name() string {
	return "hpa"
}

func (h *HPASurgeApplier) IsSurgeActive() bool {
	if h.hpa.Annotations != nil {
		if _, exists := h.hpa.Annotations[EvictionSurgeReplicasAnnotationKey]; exists {
			return true
		}
	}
	return false
}
