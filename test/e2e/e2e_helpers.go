/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"

	types "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/test/utils"
	. "github.com/onsi/gomega" //nolint:staticcheck
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	policy "k8s.io/api/policy/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kubectlGetCmd        = "get"
	outputYamlFlag       = "yaml"
	outputWideFlag       = "wide"
	kubectlNamespaceFlag = "--namespace"
)

// deploymentConfig holds deployment configuration
type deploymentConfig struct {
	Name           string
	Namespace      string
	Replicas       int32
	MaxUnavailable int
	Annotations    map[string]string
	CPURequest     string // optional, e.g. "10m"
}

// createDeployment creates a deployment with the given configuration
func createDeployment(cfg deploymentConfig) error {
	var maxUnavailable string
	if cfg.MaxUnavailable == 0 {
		maxUnavailable = "0"
	} else {
		maxUnavailable = fmt.Sprintf("%d", cfg.MaxUnavailable)
	}

	tmpl := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
{{- if .Annotations}}
  annotations:
{{- range $key, $value := .Annotations}}
    {{$key}}: "{{$value}}"
{{- end}}
{{- end}}
spec:
  replicas: {{.Replicas}}
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: {{.MaxUnavailable}}
  selector:
    matchLabels:
      app: {{.Name}}
  template:
    metadata:
      labels:
        app: {{.Name}}
    spec:
      containers:
        - name: nginx
          image: nginx:latest
          {{- if .CPURequest}}
          resources:
            requests:
              cpu: "{{.CPURequest}}"
          {{- end}}
`
	t, err := template.New("deployment").Parse(tmpl)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	data := struct {
		Name           string
		Namespace      string
		Replicas       int32
		MaxUnavailable string
		Annotations    map[string]string
		CPURequest     string
	}{
		Name:           cfg.Name,
		Namespace:      cfg.Namespace,
		Replicas:       cfg.Replicas,
		MaxUnavailable: maxUnavailable,
		Annotations:    cfg.Annotations,
		CPURequest:     cfg.CPURequest,
	}

	if err := t.Execute(&buf, data); err != nil {
		return err
	}

	cmd := exec.CommandContext(context.Background(), "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(buf.String())
	_, err = utils.Run(cmd)
	return err
}

// createPDB creates a PodDisruptionBudget
func createPDB(name, namespace string, minAvailable int32, matchLabels map[string]string) error {
	var labelsYaml strings.Builder
	for k, v := range matchLabels {
		fmt.Fprintf(&labelsYaml, "      %s: %s\n", k, v)
	}

	pdbYaml := fmt.Sprintf(`apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: %s
  namespace: %s
spec:
  minAvailable: %d
  selector:
    matchLabels:
%s`, name, namespace, minAvailable, labelsYaml.String())

	cmd := exec.CommandContext(context.Background(), "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(pdbYaml)
	_, err := utils.Run(cmd)
	return err
}

// waitForDeployment waits for a deployment to be ready
func waitForDeployment(name, namespace string) error {
	cmd := exec.CommandContext(context.Background(), "kubectl", "wait", "--for=condition=available",
		fmt.Sprintf("deployment/%s", name), kubectlNamespaceFlag, namespace, "--timeout=60s")
	_, err := utils.Run(cmd)
	return err
}

// deleteDeployment deletes a deployment
func deleteDeployment(name, namespace string) {
	cmd := exec.CommandContext(context.Background(), "kubectl", "delete", "deployment", name, kubectlNamespaceFlag, namespace)
	_, _ = utils.Run(cmd)
}

// deletePDB deletes a PodDisruptionBudget
func deletePDB(name, namespace string) {
	cmd := exec.CommandContext(context.Background(), "kubectl", "delete", "pdb", name, kubectlNamespaceFlag, namespace)
	_, _ = utils.Run(cmd)
}

// verifyPdbCreated checks if a PDB exists in the specified namespace with the given name
func verifyPdbCreated(ctx context.Context, clientset client.Client, ns, name string) error {
	var pdbList = &policy.PodDisruptionBudgetList{}
	err := clientset.List(ctx, pdbList, client.InNamespace(ns))
	Expect(err).NotTo(HaveOccurred())
	for _, pdb := range pdbList.Items {
		if pdb.Name == name {
			return nil
		}
	}
	return fmt.Errorf("expected PDB for %s, but found none", name)
}

// verifyNoPdb checks that no PDB exists in the specified namespace with the given name
func verifyNoPdb(ctx context.Context, clientset client.Client, ns, name string) error {
	var pdbList = &policy.PodDisruptionBudgetList{}
	err := clientset.List(ctx, pdbList, client.InNamespace(ns))
	Expect(err).NotTo(HaveOccurred())
	for _, pdb := range pdbList.Items {
		if pdb.Name == name {
			return fmt.Errorf("expected no PDB for %s, but found one", name)
		}
	}
	return nil
}

// verifyPdbMinAvailable checks if a PDB has the expected minAvailable value
func verifyPdbMinAvailable(ctx context.Context, clientset client.Client, ns, name string, expectedMin int32) error {
	var pdb policy.PodDisruptionBudget
	err := clientset.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pdb)
	if err != nil {
		return err
	}
	if pdb.Spec.MinAvailable == nil {
		return fmt.Errorf("PDB minAvailable is nil")
	}
	actualMin := pdb.Spec.MinAvailable.IntVal
	if actualMin != expectedMin {
		return fmt.Errorf("expected PDB minAvailable to be %d, got %d", expectedMin, actualMin)
	}
	return nil
}

// verifyEvictionAutoScalerCreated checks if an EvictionAutoScaler exists in the specified namespace with the given name
func verifyEvictionAutoScalerCreated(ctx context.Context, clientset client.Client, ns, name string) error {
	var evictionAutoScalerList = &types.EvictionAutoScalerList{}
	err := clientset.List(ctx, evictionAutoScalerList, client.InNamespace(ns))
	Expect(err).NotTo(HaveOccurred())
	for _, eas := range evictionAutoScalerList.Items {
		if eas.Name == name {
			return nil
		}
	}
	return fmt.Errorf("expected EvictionAutoScaler for %s, but found none", name)
}

// verifyNoEvictionAutoScaler checks that no EvictionAutoScaler exists in the specified namespace with the given name
func verifyNoEvictionAutoScaler(ctx context.Context, clientset client.Client, ns, name string) error {
	var evictionAutoScalerList = &types.EvictionAutoScalerList{}
	err := clientset.List(ctx, evictionAutoScalerList, client.InNamespace(ns))
	Expect(err).NotTo(HaveOccurred())
	for _, eas := range evictionAutoScalerList.Items {
		if eas.Name == name {
			return fmt.Errorf("expected no EvictionAutoScaler for %s, but found one", name)
		}
	}
	return nil
}

// verifyNoOwnerReference checks that a PDB has no owner references
func verifyNoOwnerReference(ctx context.Context, clientset client.Client, ns, name string) error {
	var pdb policy.PodDisruptionBudget
	err := clientset.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pdb)
	if err != nil {
		return err
	}
	if len(pdb.OwnerReferences) > 0 {
		return fmt.Errorf("PDB still has owner references: %v", pdb.OwnerReferences)
	}
	return nil
}

// verifyHasOwnerReference checks that a PDB has at least one owner reference
func verifyHasOwnerReference(ctx context.Context, clientset client.Client, ns, name string) error {
	var pdb policy.PodDisruptionBudget
	err := clientset.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pdb)
	if err != nil {
		return err
	}
	if len(pdb.OwnerReferences) == 0 {
		return fmt.Errorf("PDB has no owner references")
	}
	return nil
}

// createHPA creates an HPA targeting a deployment
func createHPA(name, namespace, targetDeployment string, minReplicas, maxReplicas int32) error {
	hpaYaml := fmt.Sprintf(`apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: %s
  namespace: %s
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: %s
  minReplicas: %d
  maxReplicas: %d
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 80
`, name, namespace, targetDeployment, minReplicas, maxReplicas)

	cmd := exec.CommandContext(context.Background(), "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(hpaYaml)
	_, err := utils.Run(cmd)
	return err
}

// deleteHPA deletes a HorizontalPodAutoscaler
func deleteHPA(name, namespace string) {
	cmd := exec.Command("kubectl", "delete", "hpa", name, kubectlNamespaceFlag, namespace)
	_, _ = utils.Run(cmd)
}

// verifyHPAMinReplicas checks if an HPA has the expected minReplicas value
func verifyHPAMinReplicas(ctx context.Context, clientset client.Client, ns, name string, expectedMin int32) error {
	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := clientset.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &hpa)
	if err != nil {
		return err
	}
	if hpa.Spec.MinReplicas == nil {
		return fmt.Errorf("HPA minReplicas is nil")
	}
	if *hpa.Spec.MinReplicas != expectedMin {
		return fmt.Errorf("expected HPA minReplicas to be %d, got %d", expectedMin, *hpa.Spec.MinReplicas)
	}
	return nil
}

// verifyHPAAnnotation checks if an HPA has the expected annotation value
func verifyHPAAnnotation(
	ctx context.Context, clientset client.Client, ns, name, annotationKey, expectedValue string,
) error {
	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := clientset.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &hpa)
	if err != nil {
		return err
	}
	val, ok := hpa.Annotations[annotationKey]
	if !ok {
		return fmt.Errorf("annotation %q not found on HPA %s", annotationKey, name)
	}
	if val != expectedValue {
		return fmt.Errorf("expected HPA annotation %q=%q, got %q", annotationKey, expectedValue, val)
	}
	return nil
}

// verifyHPANoAnnotation checks that an HPA does NOT have a specific annotation
func verifyHPANoAnnotation(ctx context.Context, clientset client.Client, ns, name, annotationKey string) error {
	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := clientset.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &hpa)
	if err != nil {
		return err
	}
	if _, ok := hpa.Annotations[annotationKey]; ok {
		return fmt.Errorf("annotation %q should not be present on HPA %s", annotationKey, name)
	}
	return nil
}

// --- KEDA helpers ---

// createKEDAScaledObject creates a KEDA ScaledObject targeting a deployment
func createKEDAScaledObject(name, namespace, targetDeployment string, minReplicaCount, maxReplicaCount int32) error {
	scaledObjectYaml := fmt.Sprintf(`apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: %s
  namespace: %s
spec:
  scaleTargetRef:
    name: %s
    kind: Deployment
  minReplicaCount: %d
  maxReplicaCount: %d
  triggers:
  - type: cron
    metadata:
      timezone: Etc/UTC
      start: "0 0 * * *"
      end: "59 23 * * *"
      desiredReplicas: "%d"
`, name, namespace, targetDeployment, minReplicaCount, maxReplicaCount, minReplicaCount)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(scaledObjectYaml)
	_, err := utils.Run(cmd)
	return err
}

// deleteKEDAScaledObject deletes a KEDA ScaledObject
func deleteKEDAScaledObject(name, namespace string) {
	cmd := exec.Command("kubectl", "delete", "scaledobject", name, kubectlNamespaceFlag, namespace)
	_, _ = utils.Run(cmd)
}

// verifyKEDAScaledObjectMinReplicas checks the minReplicaCount on a ScaledObject using kubectl
func verifyKEDAScaledObjectMinReplicas(name, namespace string, expectedMin int32) error {
	cmd := exec.Command("kubectl", kubectlGetCmd, "scaledobject", name,
		kubectlNamespaceFlag, namespace,
		"-o", "jsonpath={.spec.minReplicaCount}")
	output, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	actual := strings.TrimSpace(string(output))
	expected := fmt.Sprintf("%d", expectedMin)
	if actual != expected {
		return fmt.Errorf("expected ScaledObject minReplicaCount=%s, got %s", expected, actual)
	}
	return nil
}

// verifyKEDAScaledObjectAnnotation checks if a ScaledObject has the expected annotation value
func verifyKEDAScaledObjectAnnotation(name, namespace, annotationKey, expectedValue string) error {
	// Use -o json and parse in Go because kubectl jsonpath doesn't handle
	// annotation keys with dots/slashes (e.g. eviction-autoscaler.azure.com/original-min-replicas).
	cmd := exec.Command("kubectl", kubectlGetCmd, "scaledobject", name,
		kubectlNamespaceFlag, namespace, "-o", "json")
	output, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	annotations, err := extractAnnotations(output)
	if err != nil {
		return err
	}
	actual, exists := annotations[annotationKey]
	if !exists {
		return fmt.Errorf("expected ScaledObject annotation %s=%s, got ", annotationKey, expectedValue)
	}
	if actual != expectedValue {
		return fmt.Errorf("expected ScaledObject annotation %s=%s, got %s", annotationKey, expectedValue, actual)
	}
	return nil
}

// verifyKEDAScaledObjectNoAnnotation checks that a ScaledObject does NOT have a specific annotation
func verifyKEDAScaledObjectNoAnnotation(name, namespace, annotationKey string) error {
	cmd := exec.Command("kubectl", kubectlGetCmd, "scaledobject", name,
		kubectlNamespaceFlag, namespace, "-o", "json")
	output, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	annotations, err := extractAnnotations(output)
	if err != nil {
		return err
	}
	if val, exists := annotations[annotationKey]; exists {
		return fmt.Errorf("annotation %s should not be present on ScaledObject %s, got %s", annotationKey, name, val)
	}
	return nil
}

// extractAnnotations parses kubectl JSON output and returns the annotations map.
func extractAnnotations(jsonOutput []byte) (map[string]string, error) {
	var obj struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(jsonOutput, &obj); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}
	if obj.Metadata.Annotations == nil {
		return map[string]string{}, nil
	}
	return obj.Metadata.Annotations, nil
}

// installKEDA installs KEDA using Helm
// installKEDACRDs registers only the KEDA CRDs on the cluster (without installing the full operator).
// This must be called before the controller starts so its informer cache discovers the ScaledObject GVK.
func installKEDACRDs() error {
	cmd := exec.Command("helm", "repo", "add", "kedacore", "https://kedacore.github.io/charts")
	_, _ = utils.Run(cmd) // ignore error if repo already exists
	cmd = exec.Command("helm", "repo", "update")
	if _, err := utils.Run(cmd); err != nil {
		return err
	}
	// Render only CRD templates from the chart (they live under templates/crds/, not a top-level crds/ dir).
	tmpFile, err := os.CreateTemp("", "keda-crds-*.yaml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()
	_ = tmpFile.Close()

	cmd = exec.Command("helm", "template", "keda-crds", "kedacore/keda",
		"--show-only", "templates/crds/crd-scaledobjects.yaml",
		"--show-only", "templates/crds/crd-scaledjobs.yaml",
		"--show-only", "templates/crds/crd-triggerauthentications.yaml",
		"--show-only", "templates/crds/crd-clustertriggerauthentications.yaml")
	crdYAML, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	if err = os.WriteFile(tmpFile.Name(), crdYAML, 0600); err != nil {
		return err
	}
	cmd = exec.Command("kubectl", "apply", "--server-side", "-f", tmpFile.Name())
	if _, err = utils.Run(cmd); err != nil {
		return err
	}

	// Add Helm ownership metadata so `helm install keda` can adopt these CRDs.
	// KEDA puts CRDs in templates/crds/ (not the standard crds/ dir), so --skip-crds
	// doesn't help and Helm refuses to install if CRDs lack its ownership labels.
	kedaCRDs := []string{
		"scaledobjects.keda.sh",
		"scaledjobs.keda.sh",
		"triggerauthentications.keda.sh",
		"clustertriggerauthentications.keda.sh",
	}
	for _, crd := range kedaCRDs {
		cmd = exec.Command("kubectl", "label", "crd", crd,
			"app.kubernetes.io/managed-by=Helm", "--overwrite")
		if _, err = utils.Run(cmd); err != nil {
			return err
		}
		cmd = exec.Command("kubectl", "annotate", "crd", crd,
			"meta.helm.sh/release-name=keda",
			"meta.helm.sh/release-namespace=keda", "--overwrite")
		if _, err = utils.Run(cmd); err != nil {
			return err
		}
	}
	return nil
}

func installKEDA() error {
	cmd := exec.Command("helm", "repo", "add", "kedacore", "https://kedacore.github.io/charts")
	_, _ = utils.Run(cmd) // ignore error if repo already exists
	cmd = exec.Command("helm", "repo", "update")
	_, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	cmd = exec.Command("helm", "upgrade", "--install", "keda", "kedacore/keda",
		kubectlNamespaceFlag, "keda", "--create-namespace", "--wait", "--timeout", "300s")
	_, err = utils.Run(cmd)
	return err
}

// uninstallKEDA removes KEDA
func uninstallKEDA() {
	cmd := exec.Command("helm", "uninstall", "keda", kubectlNamespaceFlag, "keda")
	_, _ = utils.Run(cmd)
	cmd = exec.Command("kubectl", "delete", "namespace", "keda")
	_, _ = utils.Run(cmd)
}

const e2eLogsDir = "/tmp/e2e-logs"

// dumpClusterState writes controller logs and cluster resource state to e2eLogsDir
// so CI can upload them as artifacts for post-mortem debugging.
func dumpClusterState() {
	if err := os.MkdirAll(e2eLogsDir, 0750); err != nil {
		fmt.Printf("failed to create log dir: %v\n", err)
		return
	}

	dumps := []struct {
		file string
		args []string
	}{
		{"controller.log", []string{"logs", "-n", namespace,
			"-l", "app.kubernetes.io/name=eviction-autoscaler", "--tail=2000"}},
		{"pods.txt", []string{kubectlGetCmd, "pods", "-A", "-o", outputWideFlag}},
		{"nodes.txt", []string{kubectlGetCmd, "nodes", "-o", outputWideFlag}},
		{"deployments.txt", []string{kubectlGetCmd, "deployments", "-A", "-o", outputWideFlag}},
		{"pdb.yaml", []string{kubectlGetCmd, "pdb", "-A", "-o", outputYamlFlag}},
		{"hpa.yaml", []string{kubectlGetCmd, "hpa", "-A", "-o", outputYamlFlag}},
		{"scaledobjects.yaml", []string{kubectlGetCmd, "scaledobjects.keda.sh", "-A", "-o", outputYamlFlag}},
		{"events.txt", []string{kubectlGetCmd, "events", "-A", "--sort-by=.lastTimestamp"}},
	}

	for _, d := range dumps {
		out, err := exec.Command("kubectl", d.args...).CombinedOutput()
		if err != nil {
			out = append(out, fmt.Appendf(nil, "\nerror: %v\n", err)...)
		}
		_ = os.WriteFile(e2eLogsDir+"/"+d.file, out, 0600)
	}
	fmt.Printf("cluster state dumped to %s\n", e2eLogsDir)
}
