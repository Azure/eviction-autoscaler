package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/azure/eviction-autoscaler/internal/metrics"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UnitedDeploymentSurgeApplier surges an OpenKruise UnitedDeployment by clamping
// the UD's own spec (via UnitedDeploymentWrapper) instead of scaling a child
// Deployment directly — which the UD controller would revert. It mirrors the
// autoscaler-floor pattern (HPA/KEDA): mutate the owner the platform controls so
// it cooperates rather than fights.
type UnitedDeploymentSurgeApplier struct {
	client client.Client
	target Surger
}

var _ SurgeApplier = &UnitedDeploymentSurgeApplier{}

func (u *UnitedDeploymentSurgeApplier) ApplySurge(ctx context.Context, surgeReplicas int32) error {
	wrapper, ok := u.target.(*UnitedDeploymentWrapper)
	if !ok {
		return fmt.Errorf("UnitedDeploymentSurgeApplier: unexpected target type %T", u.target)
	}
	// Capture the pre-surge snapshot with safety guards (status convergence,
	// lost-snapshot detection) before mutating replicas.
	if err := wrapper.PrepareSurge(); err != nil {
		metrics.UnitedDeploymentSurgeFailureCounter.WithLabelValues(
			wrapper.Obj().GetNamespace(), surgeFailureReason(err)).Inc()
		return err
	}
	wrapper.SetReplicas(surgeReplicas)
	wrapper.AddAnnotation(EvictionSurgeReplicasAnnotationKey, strconv.FormatInt(int64(surgeReplicas), 10))
	return u.client.Update(ctx, wrapper.Obj())
}

// surgeFailureReason maps a PrepareSurge error to a bounded metric label.
func surgeFailureReason(err error) string {
	switch {
	case errors.Is(err, errStatusNotConverged):
		return "status_not_converged"
	case errors.Is(err, errSnapshotLostDuringSurge):
		return "snapshot_lost"
	default:
		return "snapshot_write_failed"
	}
}

func (u *UnitedDeploymentSurgeApplier) RevertSurge(ctx context.Context, originalMinReplicas int32) error {
	// Restore the pre-surge topology from the snapshot (original per-subset
	// config + total), not just a flat replica count, so percentage/remainder
	// subsets are returned exactly as the author set them.
	wrapper, ok := u.target.(*UnitedDeploymentWrapper)
	if !ok {
		return fmt.Errorf("UnitedDeploymentSurgeApplier: unexpected target type %T", u.target)
	}
	if _, hasSnapshot := wrapper.readSnapshot(); hasSnapshot {
		wrapper.RestoreOriginal()
	} else {
		// Snapshot lost: we can't restore the exact topology, but we must still
		// scale the UnitedDeployment back down to the floor so it isn't left
		// stuck at the surged replica count.
		metrics.UnitedDeploymentSurgeFailureCounter.WithLabelValues(
			wrapper.Obj().GetNamespace(), "revert_without_snapshot").Inc()
		wrapper.ForceScaleDown(originalMinReplicas)
	}
	wrapper.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
	return u.client.Update(ctx, wrapper.Obj())
}

func (u *UnitedDeploymentSurgeApplier) IsSurgeActive() bool {
	return hasTargetAnnotation(u.target)
}

func (u *UnitedDeploymentSurgeApplier) Name() string {
	return "uniteddeployment"
}
