package namespacefilter

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const EnableEvictionAutoscalerAnnotationKey = "eviction-autoscaler.azure.com/enable"

type nsfilter struct {
	optin     bool
	hardcoded []string
}

func New(hardcoded []string, optin bool) *nsfilter {
	return &nsfilter{
		hardcoded: hardcoded,
		optin:     optin,
	}
}

type Reader interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
}

func (n *nsfilter) Filter(ctx context.Context, c Reader, ns string) (bool, error) {

	// Fetch the namespace to check for the annotation
	namespace := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: ns}, namespace)
	if err != nil {
		return false, fmt.Errorf("failed to get namespace %s: %w", ns, err)
	}

	//annotation takes preceence
	val, ok := namespace.Annotations[EnableEvictionAutoscalerAnnotationKey]
	if ok {
		value, err := strconv.ParseBool(val)
		if err != nil {
			return false, fmt.Errorf("failed to parse annotation value %s: %w", val, err)
		}
		return value, nil
	}

	// if hardcoded flip it.
	if slices.Contains(n.hardcoded, ns) {
		return !n.optin, nil
	}

	// If the namespace is not in the hardcoded list, return the default value based on the optin flag
	return n.optin, nil
}
