package helm_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

func runHelm(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	executable := os.Getenv("HELM_BIN")
	if executable == "" {
		var err error
		executable, err = exec.LookPath("helm")
		if err != nil {
			t.Skip("Helm is not on PATH; run make helm-validate to run chart tests")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, executable, args...).CombinedOutput()
}

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
			output, err := runHelm(t, "template", "memory-test",
				filepath.Join("..", "..", "helm", "eviction-autoscaler"),
				"--show-only", "templates/deployment.yaml",
				"--set-string", "resources.requests.memory="+quantities.request,
				"--set-string", "resources.limits.memory="+quantities.limit)
			if wantError {
				if err == nil {
					t.Fatal("expected invalid memory settings to fail rendering")
				}
				if !strings.Contains(string(output), "resources.") ||
					!strings.Contains(string(output), ".memory") {
					t.Fatalf("expected a memory configuration error, got: %s", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid memory settings failed rendering: %v\n%s", err, output)
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal(output, &deployment); err != nil {
				t.Fatal(err)
			}
			resources := deployment.Spec.Template.Spec.Containers[0].Resources
			gotRequest := resources.Requests[corev1.ResourceMemory]
			gotLimit := resources.Limits[corev1.ResourceMemory]
			if gotRequest.Cmp(request) != 0 || gotLimit.Cmp(limit) != 0 {
				t.Fatalf("memory settings were changed: request=%s limit=%s", gotRequest.String(), gotLimit.String())
			}
		})
	}
}

func TestMemoryDefaultsAndOmissions(t *testing.T) {
	for _, test := range []struct {
		name        string
		args        []string
		wantRequest string
		wantLimit   string
	}{
		{name: "defaults", wantRequest: "128Mi", wantLimit: "512Mi"},
		{
			name: "numeric bytes",
			args: []string{
				"--set", "resources.requests.memory=134217728",
				"--set", "resources.limits.memory=536870912",
			},
			wantRequest: "128Mi", wantLimit: "512Mi",
		},
		{
			name: "no limit", args: []string{"--set", "resources.limits.memory=null"},
			wantRequest: "128Mi",
		},
		{
			name: "no request", args: []string{"--set", "resources.requests.memory=null"},
			wantLimit: "512Mi",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{
				"template", "memory-test", filepath.Join("..", "..", "helm", "eviction-autoscaler"),
				"--show-only", "templates/deployment.yaml",
			}
			output, err := runHelm(t, append(args, test.args...)...)
			if err != nil {
				t.Fatalf("render: %v\n%s", err, output)
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal(output, &deployment); err != nil {
				t.Fatal(err)
			}
			resources := deployment.Spec.Template.Spec.Containers[0].Resources
			for _, setting := range []struct {
				name     string
				values   corev1.ResourceList
				expected string
			}{
				{"request", resources.Requests, test.wantRequest},
				{"limit", resources.Limits, test.wantLimit},
			} {
				got, exists := setting.values[corev1.ResourceMemory]
				if setting.expected == "" {
					if exists {
						t.Errorf("expected omitted memory %s, got %s", setting.name, got.String())
					}
					continue
				}
				want := resource.MustParse(setting.expected)
				if !exists || got.Cmp(want) != 0 {
					t.Errorf("memory %s = %s, want %s", setting.name, got.String(), want.String())
				}
			}
		})
	}
}

func TestMemoryLimitOnlyOverrides(t *testing.T) {
	for _, limit := range []string{"64Mi", "96Mi"} {
		t.Run(limit, func(t *testing.T) {
			output, err := runHelm(t, "template", "memory-test",
				filepath.Join("..", "..", "helm", "eviction-autoscaler"),
				"--set-string", "resources.limits.memory="+limit)
			if err == nil {
				t.Fatal("expected rendering to reject the default 128Mi request above the overridden limit")
			}
			if !strings.Contains(string(output), "set both values when lowering the memory limit") {
				t.Fatalf("expected an actionable request/limit error, got: %s", output)
			}
		})
	}
}
