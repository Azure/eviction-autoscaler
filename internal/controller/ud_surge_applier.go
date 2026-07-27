package controllers

import (
	"context"
	"fmt"
	"strconv"

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
	u.target.SetReplicas(surgeReplicas)
	u.target.AddAnnotation(EvictionSurgeReplicasAnnotationKey, strconv.FormatInt(int64(surgeReplicas), 10))
	return u.client.Update(ctx, u.target.Obj())
}

func (u *UnitedDeploymentSurgeApplier) RevertSurge(ctx context.Context, _ int32) error {
	// Restore the pre-surge topology from the snapshot (original per-subset
	// config + total), not just a flat replica count, so percentage/remainder
	// subsets are returned exactly as the author set them.
	wrapper, ok := u.target.(*UnitedDeploymentWrapper)
	if !ok {
		return fmt.Errorf("UnitedDeploymentSurgeApplier: unexpected target type %T", u.target)
	}
	wrapper.RestoreOriginal()
	u.target.RemoveAnnotation(EvictionSurgeReplicasAnnotationKey)
	return u.client.Update(ctx, u.target.Obj())
}

func (u *UnitedDeploymentSurgeApplier) IsSurgeActive() bool {
	return hasTargetAnnotation(u.target)
}

func (u *UnitedDeploymentSurgeApplier) Name() string {
	return "uniteddeployment"
}
