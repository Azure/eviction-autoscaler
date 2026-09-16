package namespacefilter

import (
	"fmt"
	"strings"

	apivalidation "k8s.io/apimachinery/pkg/util/validation"
)

// ParseNamespaceList parses a comma-separated namespace list from an env var into a
// deduplicated, order-preserving slice. Entries are trimmed and empty entries are
// dropped, so an unset or empty raw value yields no entries (nil). Each remaining
// entry is validated as a DNS-1123 label (the Kubernetes namespace-name rule) so a
// typo fails fast at startup rather than silently becoming a never-matching entry —
// important for a safety-sensitive allowlist. Duplicates are collapsed.
//
// It deliberately does NOT reject AKS-owned namespaces: unlike the customer-facing
// ACTIONED_NAMESPACES list, operator-owned scoping lists may legitimately target
// AKS-owned namespaces (which the eviction-autoscaler always manages).
func ParseNamespaceList(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, entry := range parts {
		ns := strings.TrimSpace(entry)
		if ns == "" {
			continue
		}
		if errs := apivalidation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, fmt.Errorf("invalid namespace %q: %s", ns, strings.Join(errs, "; "))
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out, nil
}
