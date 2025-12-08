package namespacefilter

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const EnableEvictionAutoscalerAnnotationKey = "eviction-autoscaler.azure.com/enable"

type nsfilter struct {
	disabledByDefault bool
	hardcoded         []string
}

func New(hardcoded []string, disabledByDefault bool) *nsfilter {
	return &nsfilter{
		hardcoded:         hardcoded,
		disabledByDefault: disabledByDefault,
	}
}

type Reader interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
}

func (n *nsfilter) Filter(ctx context.Context, c Reader, ns string) (bool, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Fetch the namespace to check for the annotation
	namespace := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: ns}, namespace)
	if err != nil {
		return false, fmt.Errorf("failed to get namespace %s: %w", ns, err)
	}

	//annotation takes precedence
	val, ok := namespace.Annotations[EnableEvictionAutoscalerAnnotationKey]
	if ok {
		value, err := strconv.ParseBool(val)
		if err != nil {
			return false, fmt.Errorf("failed to parse annotation value %s: %w", val, err)
		}
		logger.Info("namespace filtering decision", "namespace", ns, "annotation", EnableEvictionAutoscalerAnnotationKey, "value", value, "filtering", value)
		return value, nil
	}

	// if namespaces are disabled by default (disabledByDefault=true) and namespace is in hardcoded list, enable it
	// if namespaces are enabled by default (disabledByDefault=false), hardcoded list is ignored
	if n.disabledByDefault && slices.Contains(n.hardcoded, ns) {
		logger.Info("namespace filtering decision", "namespace", ns, "source", "hardcoded", "disabledByDefault", true, "filtering", true)
		return true, nil
	}

	// If the namespace is not in the hardcoded list, return the default value
	// disabledByDefault=true (ENABLED_BY_DEFAULT=false): return false (disabled by default)
	// disabledByDefault=false (ENABLED_BY_DEFAULT=true): return true (enabled by default)
	defaultValue := !n.disabledByDefault
	logger.Info("namespace filtering decision", "namespace", ns, "source", "default", "disabledByDefault", n.disabledByDefault, "filtering", defaultValue)
	return defaultValue, nil
}
