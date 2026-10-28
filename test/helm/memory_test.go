package helm_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

func buildValidator(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "validate-chart-resources")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", executable, "./cmd/validate-chart-resources")
	cmd.Dir = filepath.Join("..", "..")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build preflight validator: %v\n%s", err, output)
	}
	return executable
}

func runValidator(t *testing.T, executable string, manifest []byte) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable)
	cmd.Stdin = bytes.NewReader(manifest)
	return cmd.CombinedOutput()
}

func TestMemoryDefaultsAndOmissions(t *testing.T) {
	validator := buildValidator(t)
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
			if validationOutput, err := runValidator(t, validator, output); err != nil {
				t.Fatalf("preflight: %v\n%s", err, validationOutput)
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
	validator := buildValidator(t)
	for _, limit := range []string{"64Mi", "96Mi"} {
		t.Run(limit, func(t *testing.T) {
			output, err := runHelm(t, "template", "memory-test",
				filepath.Join("..", "..", "helm", "eviction-autoscaler"),
				"--show-only", "templates/deployment.yaml",
				"--set-string", "resources.limits.memory="+limit)
			if err != nil {
				t.Fatalf("render: %v\n%s", err, output)
			}
			validationOutput, err := runValidator(t, validator, output)
			if err == nil {
				t.Fatal("expected preflight to reject the default 128Mi request above the overridden limit")
			}
			if !strings.Contains(string(validationOutput), "set both values when lowering the memory limit") {
				t.Fatalf("expected an actionable request/limit error, got: %s", validationOutput)
			}
		})
	}
}

func TestPodCacheRendering(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		enabled string
	}{
		{name: "default", enabled: "true"},
		{name: "enabled", args: []string{"--set", "controllerConfig.podCache.enabled=true"}, enabled: "true"},
		{name: "disabled", args: []string{"--set", "controllerConfig.podCache.enabled=false"}, enabled: "false"},
		{name: "missing section", args: []string{"--set", "controllerConfig.podCache=null"}, enabled: "true"},
		{name: "missing flag", args: []string{"--set", "controllerConfig.podCache.enabled=null"}, enabled: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{
				"template", "cache-test", filepath.Join("..", "..", "helm", "eviction-autoscaler"),
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
			for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "POD_CACHE_ENABLED" {
					if env.Value != test.enabled {
						t.Fatalf("POD_CACHE_ENABLED=%q, want %q", env.Value, test.enabled)
					}
					return
				}
			}
			t.Fatal("POD_CACHE_ENABLED is missing")
		})
	}
}

func TestPodStatusCompatibilityRBAC(t *testing.T) {
	chartRole, err := runHelm(t, "template", "rbac-test",
		filepath.Join("..", "..", "helm", "eviction-autoscaler"),
		"--show-only", "templates/clusterrole.yaml")
	if err != nil {
		t.Fatalf("render RBAC: %v\n%s", err, chartRole)
	}
	generatedRole, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		role []byte
	}{
		{"chart", chartRole},
		{"generated", generatedRole},
	} {
		t.Run(test.name, func(t *testing.T) {
			var role rbacv1.ClusterRole
			if err := yaml.Unmarshal(test.role, &role); err != nil {
				t.Fatal(err)
			}
			for _, rule := range role.Rules {
				if slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, "pods/status") {
					if !slices.Equal(rule.Verbs, []string{"patch", "update"}) {
						t.Fatalf("expected Pod-status patch and compatibility update permissions, got %v", rule.Verbs)
					}
					return
				}
			}
			t.Fatal("legacy Pod-status permissions are missing")
		})
	}
}
