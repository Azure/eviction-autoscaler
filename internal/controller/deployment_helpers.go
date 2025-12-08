package controllers

import (
	v1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// HasNonZeroMaxUnavailable returns true if the deployment has maxUnavailable set to a non-zero value.
// Deployments with maxUnavailable != 0 already tolerate downtime, so PDB creation is skipped.
func hasNonZeroMaxUnavailable(deployment *v1.Deployment) bool {
	if deployment.Spec.Strategy.RollingUpdate == nil {
		return false
	}
	maxUnavailable := deployment.Spec.Strategy.RollingUpdate.MaxUnavailable
	if maxUnavailable == nil {
		return false
	}
	if maxUnavailable.Type == intstr.Int {
		return maxUnavailable.IntVal != 0
	}
	// String type - check for "0" or "0%"
	return maxUnavailable.StrVal != "0" && maxUnavailable.StrVal != "0%"
}
