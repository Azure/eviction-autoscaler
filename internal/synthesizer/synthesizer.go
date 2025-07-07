/*
MIT License

Copyright (c) 2024 Paul Miller / Javier Garcia

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
*/

package synthesizer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EmptyInput represents the empty input struct for the synthesizer
type EmptyInput struct{}

// SynthFunc is the Eno synthesizer function that generates manifests
func SynthFunc(input EmptyInput) ([]client.Object, error) {
	var objects []client.Object

	// List of config files to process (relative to repository root)
	configFiles := []string{
		"config/crd/bases/eviction-autoscaler.azure.com_evictionautoscalers.yaml",
		"config/rbac/service_account.yaml",
		"config/rbac/role.yaml",
		"config/rbac/role_binding.yaml",
		"config/manager/manager.yaml",
	}

	// Create a decoder
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	// Get the current working directory and find the repository root
	currentDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current directory: %w", err)
	}

	// Look for repository root by finding config directory
	repoRoot := currentDir
	for {
		configPath := filepath.Join(repoRoot, "config")
		if _, err := os.Stat(configPath); err == nil {
			break
		}
		parent := filepath.Dir(repoRoot)
		if parent == repoRoot {
			return nil, fmt.Errorf("could not find repository root (config directory not found)")
		}
		repoRoot = parent
	}

	// Process each config file
	for _, configFile := range configFiles {
		// Read the file content
		fullPath := filepath.Join(repoRoot, configFile)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file %s: %w", fullPath, err)
		}

		// Split by document separator and process each document
		documents := strings.Split(string(content), "\n---\n")
		for _, doc := range documents {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}

			// Parse YAML document using runtime decoder
			obj := &unstructured.Unstructured{}
			_, _, err := decoder.Decode([]byte(doc), nil, obj)
			if err != nil {
				return nil, fmt.Errorf("failed to decode YAML from %s: %w", configFile, err)
			}

			// Skip empty objects
			if obj.GetKind() == "" {
				continue
			}

			objects = append(objects, obj)
		}
	}

	return objects, nil
}
