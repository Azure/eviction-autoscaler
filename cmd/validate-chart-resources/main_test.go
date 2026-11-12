package main

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMemoryQuantities(t *testing.T) {
	for _, quantities := range []struct {
		request string
		limit   string
	}{
		{"128Mi", "512Mi"},
		{"64Mi", "64Mi"},
		{"128Mi", "64Mi"},
		{"128Mi", "96Mi"},
		{"1Gi", "1024Mi"},
		{"1025Mi", "1Gi"},
		{"1.5Gi", "1536Mi"},
		{".5Gi", "512Mi"},
		{"+128Mi", "512Mi"},
		{"512M", "512Mi"},
		{"128Mi", "128M"},
		{"1k", "1Ki"},
		{"1Ki", "1k"},
		{"1T", "1Ti"},
		{"1Pi", "1P"},
		{"1E", "1e18"},
		{"1e3", "1000"},
		{"1E+3", "1k"},
		{"1.28e8", "128M"},
		{"1e100", "1e101"},
		{"0", "0"},
		{"-0", "0"},
		{"0", "1n"},
		{"1n", "0"},
		{"1u", "1000n"},
		{"1m", "1000u"},
		{"0.1n", "0.01n"},
		{"1.0000000001", "1.000000001"},
		{"1.0000000001", "1"},
		{"0.0000000001Gi", "0.107374183"},
		{"9007199254740993", "9007199254740992"},
		{"9007199254740992", "9007199254740993"},
		{"8Ei", "9223372036854775807"},
		{"9223372036854775808", "8Ei"},
		{"-1Mi", "512Mi"},
		{"128Mi", "-1Gi"},
		{"invalid", "512Mi"},
		{"128Mi", "512mi"},
		{"128GiB", "512Gi"},
		{"", "512Mi"},
		{"128Mi", "NaN"},
	} {
		t.Run(quantities.request+"/"+quantities.limit, func(t *testing.T) {
			request, requestErr := resource.ParseQuantity(quantities.request)
			limit, limitErr := resource.ParseQuantity(quantities.limit)
			wantError := requestErr != nil || limitErr != nil ||
				request.Sign() < 0 || limit.Sign() < 0 || request.Cmp(limit) > 0
			manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: eviction-autoscaler
          resources:
            requests:
              memory: %q
            limits:
              memory: %q
`, quantities.request, quantities.limit)
			err := validateDeployment(strings.NewReader(manifest))
			if wantError && err == nil {
				t.Fatal("expected invalid memory settings to fail preflight")
			}
			if !wantError && err != nil {
				t.Fatalf("valid memory settings failed preflight: %v", err)
			}
		})
	}
}

func TestValidateDeploymentInput(t *testing.T) {
	for _, test := range []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{name: "empty", wantErr: "decode rendered Deployment"},
		{name: "invalid YAML", manifest: "kind: [", wantErr: "decode rendered Deployment"},
		{name: "wrong kind", manifest: "kind: Pod", wantErr: "expected a Deployment"},
		{name: "no containers", manifest: "kind: Deployment", wantErr: "expected a Deployment"},
		{
			name: "no resources",
			manifest: `kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: controller
`,
		},
		{
			name: "multiple documents",
			manifest: `kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: controller
---
kind: Deployment
`,
			wantErr: "expected a single Deployment",
		},
		{
			name: "invalid trailing YAML",
			manifest: `kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: controller
---
kind: [
`,
			wantErr: "decode trailing YAML",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateDeployment(strings.NewReader(test.manifest))
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
			}
		})
	}
}
