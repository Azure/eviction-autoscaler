/*
MIT License

Copyright (c) 2024 Paul Miller / Javier Garcia

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
*/

package synthesizer

import (
	"embed"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed config/*
var configFS embed.FS

// EmptyInput represents the empty input struct for the synthesizer
type EmptyInput struct{}

// SynthFunc is the Eno synthesizer function that generates manifests
func SynthFunc(input EmptyInput) ([]client.Object, error) {
	var objects []client.Object

	// List of config files to process
	configFiles := []string{
		"config/crd.yaml",
		"config/service_account.yaml",
		"config/role.yaml",
		"config/role_binding.yaml",
		"config/manager.yaml",
	}

	// Create a decoder
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	// Process each config file
	for _, configFile := range configFiles {
		// Read the file content
		content, err := configFS.ReadFile(configFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file %s: %w", configFile, err)
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
