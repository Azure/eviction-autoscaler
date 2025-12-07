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
	"fmt"
	"os/exec"
	"strings"
	"text/template"

	types "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/test/utils"
	. "github.com/onsi/gomega"
	policy "k8s.io/api/policy/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deploymentConfig holds deployment configuration
type deploymentConfig struct {
	Name           string
	Namespace      string
	Replicas       int32
	MaxUnavailable int
	Annotations    map[string]string
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
	}{
		Name:           cfg.Name,
		Namespace:      cfg.Namespace,
		Replicas:       cfg.Replicas,
		MaxUnavailable: maxUnavailable,
		Annotations:    cfg.Annotations,
	}

	if err := t.Execute(&buf, data); err != nil {
		return err
	}

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(buf.String())
	_, err = utils.Run(cmd)
	return err
}

// createPDB creates a PodDisruptionBudget
func createPDB(name, namespace string, minAvailable int32, matchLabels map[string]string) error {
	var labelsYaml strings.Builder
	for k, v := range matchLabels {
		labelsYaml.WriteString(fmt.Sprintf("      %s: %s\n", k, v))
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

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(pdbYaml)
	_, err := utils.Run(cmd)
	return err
}

// waitForDeployment waits for a deployment to be ready
func waitForDeployment(name, namespace string) error {
	cmd := exec.Command("kubectl", "wait", "--for=condition=available",
		fmt.Sprintf("deployment/%s", name), "--namespace", namespace, "--timeout=60s")
	_, err := utils.Run(cmd)
	return err
}

// deleteDeployment deletes a deployment
func deleteDeployment(name, namespace string) error {
	cmd := exec.Command("kubectl", "delete", "deployment", name, "--namespace", namespace)
	_, _ = utils.Run(cmd)
	return nil
}

// deletePDB deletes a PodDisruptionBudget
func deletePDB(name, namespace string) error {
	cmd := exec.Command("kubectl", "delete", "pdb", name, "--namespace", namespace)
	_, _ = utils.Run(cmd)
	return nil
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
