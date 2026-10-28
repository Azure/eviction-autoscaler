package helm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestKubernetesSchemaValidation(t *testing.T) {
	executable := os.Getenv("KUBECONFORM_BIN")
	if executable == "" {
		var err error
		executable, err = exec.LookPath("kubeconform")
		if err != nil {
			t.Skip("Kubeconform is not on PATH; run make helm-schema-validate")
		}
	}
	version := os.Getenv("KUBECONFORM_KUBERNETES_VERSION")
	if version == "" {
		version = "1.32.0"
	}
	cache := os.Getenv("KUBECONFORM_CACHE")
	if cache == "" {
		cache = t.TempDir()
	}
	rendered, err := runHelm(t, "template", "schema-test",
		filepath.Join("..", "..", "helm", "eviction-autoscaler"),
		"--show-only", "templates/deployment.yaml")
	if err != nil {
		t.Fatalf("render: %v\n%s", err, rendered)
	}
	var deployment map[string]any
	if err := yaml.Unmarshal(rendered, &deployment); err != nil {
		t.Fatal(err)
	}
	containers, found, err := unstructured.NestedSlice(deployment, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		t.Fatalf("read rendered containers: found=%v err=%v", found, err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected container type %T", containers[0])
	}
	container["unexpectedOOMSetting"] = true
	if err := unstructured.SetNestedSlice(deployment, containers, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	unknownField, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	delete(container, "unexpectedOOMSetting")
	container["image"] = float64(123)
	if err := unstructured.SetNestedSlice(deployment, containers, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	wrongType, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		manifest []byte
		wantErr  string
	}{
		{name: "valid Deployment", manifest: rendered},
		{name: "unknown field", manifest: unknownField, wantErr: "unexpectedOOMSetting"},
		{name: "wrong field type", manifest: wrongType, wantErr: "image"},
		{
			name: "missing schema",
			manifest: []byte(`apiVersion: no-schema.example.com/v1
kind: NoSchema
metadata:
  name: missing-schema
`),
			wantErr: "schema",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-strict", "-summary", "-exit-on-error",
				"-kubernetes-version", version, "-cache", cache)
			cmd.Stdin = bytes.NewReader(test.manifest)
			output, err := cmd.CombinedOutput()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("valid Deployment rejected: %v\n%s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), test.wantErr) {
				t.Fatalf("expected validation failure containing %q, got err=%v\n%s", test.wantErr, err, output)
			}
		})
	}
}
