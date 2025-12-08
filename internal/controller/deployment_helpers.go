package controllers

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HasNonZeroMaxUnavailable returns true if the deployment has maxUnavailable set to a non-zero value.
// Deployments with maxUnavailable != 0 already tolerate downtime, so PDB creation is skipped.
func HasNonZeroMaxUnavailable(deployment *v1.Deployment) bool {
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

// Watch Namespace calls this to handle dynamic enable/disable via annotations.
// When a namespace's eviction-autoscaler.azure.com/enable annotation changes,
// we need to reconcile all deployments in that namespace to create or delete PDBs accordingly.
//
// Performance Note: The List call below reads from the controller-runtime client cache,
// NOT directly from the Kubernetes API server. This cache is maintained in-memory and
// automatically kept up-to-date via watches. Therefore, listing deployments is a fast
// in-memory operation with no API server round-trip overhead. This makes it acceptable
// to list all deployments in a namespace when its configuration changes.
func requeueDeploymentsOnNamespaceChange(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		logger := log.FromContext(ctx)
		ns, ok := obj.(*corev1.Namespace)
		if !ok {
			return nil
		}

		// List all deployments in the namespace and trigger reconciliation for each
		var deploymentList v1.DeploymentList
		if err := c.List(ctx, &deploymentList, client.InNamespace(ns.Name)); err != nil {
			//kind of a bad error as we ae going to miss cleanup theoretically
			logger.Error(err, "Failed to list deployments in namespace", "namespace", ns.Name)
			return nil
		}

		var requests []reconcile.Request
		for _, deployment := range deploymentList.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: deployment.Namespace,
					Name:      deployment.Name,
				},
			})
		}
		return requests
	}
}

// triggerOnAnnotationChange checks if a deployment update event should trigger reconciliation
// by comparing the annotations between old and new deployment
// could collapse with pdbhelpers trigggerOnAnnotationChange
func triggerOnAnnotationChange(e event.UpdateEvent, logger logr.Logger) bool {
	oldDeployment, okOld := e.ObjectOld.(*v1.Deployment)
	newDeployment, okNew := e.ObjectNew.(*v1.Deployment)
	if okOld && okNew {
		oldVal := oldDeployment.Annotations[PDBCreateAnnotationKey]
		newVal := newDeployment.Annotations[PDBCreateAnnotationKey]
		if oldVal != newVal {
			logger.Info("Update event detected, annotation value changed",
				"oldValue", oldVal, "newValue", newVal)
			return true
		}
	}
	return false
}

// triggerOnReplicaChange checks if a deployment update event should trigger reconciliation
// by comparing the number of replicas between old and new deployment
func triggerOnReplicaChange(e event.UpdateEvent, logger logr.Logger) bool {
	if oldDeployment, ok := e.ObjectOld.(*v1.Deployment); ok {
		newDeployment := e.ObjectNew.(*v1.Deployment)
		if lo.FromPtr(oldDeployment.Spec.Replicas) != lo.FromPtr(newDeployment.Spec.Replicas) {
			logger.Info("Update event detected, num of replicas changed",
				"newReplicas", lo.FromPtr(newDeployment.Spec.Replicas),
				"oldReplicas", lo.FromPtr(oldDeployment.Spec.Replicas))
			return true
		}
	}
	return false
}
