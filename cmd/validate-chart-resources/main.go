package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func main() {
	log.SetFlags(0)
	if err := validateDeployment(os.Stdin); err != nil {
		log.Fatal(err)
	}
}

func validateDeployment(input io.Reader) error {
	decoder := yaml.NewYAMLOrJSONDecoder(input, 4096)
	var deployment appsv1.Deployment
	if err := decoder.Decode(&deployment); err != nil {
		return fmt.Errorf("decode rendered Deployment: %w", err)
	}
	if deployment.Kind != "Deployment" || len(deployment.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("expected a Deployment with containers; render with --show-only templates/deployment.yaml")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("expected a single Deployment; render with --show-only templates/deployment.yaml")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing YAML: %w", err)
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if err := validateMemory(container.Resources); err != nil {
			return fmt.Errorf("container %q: %w", container.Name, err)
		}
	}
	return nil
}

func validateMemory(resources corev1.ResourceRequirements) error {
	request, hasRequest := resources.Requests[corev1.ResourceMemory]
	limit, hasLimit := resources.Limits[corev1.ResourceMemory]
	if request.Sign() < 0 {
		return fmt.Errorf("resources.requests.memory must be non-negative, got %s", request.String())
	}
	if limit.Sign() < 0 {
		return fmt.Errorf("resources.limits.memory must be non-negative, got %s", limit.String())
	}
	if hasRequest && hasLimit && request.Cmp(limit) > 0 {
		return fmt.Errorf("resources.requests.memory (%s) must not exceed resources.limits.memory (%s); "+
			"set both values when lowering the memory limit", request.String(), limit.String())
	}
	return nil
}
