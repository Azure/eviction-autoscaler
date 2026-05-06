package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var errNotFound = errors.New("not found")

// findScaledObjectForTarget looks for a KEDA ScaledObject targeting the given workload.
// We use unstructured types instead of importing github.com/kedacore/keda/v2 because
// KEDA's module pulls in 80+ direct dependencies (AWS, Azure, GCP SDKs, Kafka, etc.)
// which would massively bloat our dependency tree for just two types.
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

	for _, item := range list.Items {
		scaleTargetRef, found, err := unstructured.NestedMap(item.Object, "spec", "scaleTargetRef")
		if err != nil {
			return nil, fmt.Errorf("failed to read scaleTargetRef from ScaledObject %s/%s: %w", namespace, item.GetName(), err)
		}
		if !found {
			log.FromContext(ctx).V(1).Info("ScaledObject missing scaleTargetRef, skipping",
				"namespace", namespace, "name", item.GetName())
			continue
		}
		name, _ := scaleTargetRef["name"].(string)
		kind, _ := scaleTargetRef["kind"].(string)
		if kind == "" {
			kind = "Deployment" // KEDA default
		}
		if name == targetName && strings.EqualFold(kind, targetKind) {
			return &item, nil
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

// HasAutoscaler returns true if an HPA or KEDA ScaledObject targets this workload.
// Returns an error on real API failures (not errNotFound) so the caller can retry.
func HasAutoscaler(ctx context.Context, c client.Client, namespace, targetName, targetKind string) (bool, error) {
	_, err := findScaledObjectForTarget(ctx, c, namespace, targetName, targetKind)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errNotFound) {
		return false, err
	}

	_, err = findHPAForTarget(ctx, c, namespace, targetName, targetKind)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errNotFound) {
		return false, err
	}

	return false, nil
}

// ResolveMinReplicas returns the effective minimum replica count for a workload.
// Priority: KEDA ScaledObject minReplicaCount > HPA minReplicas > deployment.spec.replicas.
//
// KEDA always takes precedence because it is the primary autoscaler; its managed
// HPA is filtered out by isKEDAManagedHPA, so only standalone HPAs are considered
// at tier 2. This value drives the surge calculation (baseline + 1), while each
// individual applier guards against lowering its own floor below the current value.
//
// The returned bool indicates whether an autoscaler (KEDA or HPA) was found.
// When true, the int32 is the autoscaler's floor (which may be 0 for KEDA scale-to-zero).
// When false, the int32 is the deployReplicas fallback.
// Returns an error on real API failures so the caller can retry rather than using a wrong value.
func ResolveMinReplicas(ctx context.Context, c client.Client, namespace, targetName, targetKind string, deployReplicas int32) (int32, bool, error) {
	logger := log.FromContext(ctx)

	// 1. Check KEDA ScaledObject
	scaledObj, err := findScaledObjectForTarget(ctx, c, namespace, targetName, targetKind)
	if err != nil && !errors.Is(err, errNotFound) {
		return 0, false, err
	}
	if err == nil && scaledObj != nil {
		if val, found, _ := unstructured.NestedInt64(scaledObj.Object, "spec", "minReplicaCount"); found {
			logger.V(1).Info("Using KEDA ScaledObject minReplicaCount",
				"target", targetName, "minReplicaCount", val)
			return int32(val), true, nil
		}
	}

	// 2. Check standalone HPA
	hpa, err := findHPAForTarget(ctx, c, namespace, targetName, targetKind)
	if err != nil && !errors.Is(err, errNotFound) {
		return 0, false, err
	}
	if err == nil && hpa != nil && hpa.Spec.MinReplicas != nil {
		logger.V(1).Info("Using HPA minReplicas",
			"target", targetName, "hpa", hpa.Name, "minReplicas", *hpa.Spec.MinReplicas)
		return *hpa.Spec.MinReplicas, true, nil
	}

	// 3. Fall back to deployment replicas
	return deployReplicas, false, nil
}
