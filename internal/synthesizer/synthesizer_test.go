/*
MIT License

Copyright (c) 2024 Paul Miller / Javier Garcia

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
*/

package synthesizer

import (
	"testing"
)

func TestSynthFunc(t *testing.T) {
	input := EmptyInput{}

	objects, err := SynthFunc(input)
	if err != nil {
		t.Fatalf("SynthFunc failed: %v", err)
	}

	if len(objects) == 0 {
		t.Fatal("Expected objects to be generated, got none")
	}

	// Check that we have some expected objects
	foundCRD := false
	foundDeployment := false
	foundRole := false
	foundRoleBinding := false
	foundServiceAccount := false
	foundNamespace := false
	foundPDB := false

	for _, obj := range objects {
		switch obj.GetObjectKind().GroupVersionKind().Kind {
		case "CustomResourceDefinition":
			foundCRD = true
		case "Deployment":
			foundDeployment = true
		case "ClusterRole":
			foundRole = true
		case "ClusterRoleBinding":
			foundRoleBinding = true
		case "ServiceAccount":
			foundServiceAccount = true
		case "Namespace":
			foundNamespace = true
		case "PodDisruptionBudget":
			foundPDB = true
		}
	}

	if !foundCRD {
		t.Error("Expected to find CustomResourceDefinition")
	}

	if !foundDeployment {
		t.Error("Expected to find Deployment")
	}

	if !foundRole {
		t.Error("Expected to find ClusterRole")
	}

	if !foundRoleBinding {
		t.Error("Expected to find ClusterRoleBinding")
	}

	if !foundServiceAccount {
		t.Error("Expected to find ServiceAccount")
	}

	if !foundNamespace {
		t.Error("Expected to find Namespace")
	}

	if !foundPDB {
		t.Error("Expected to find PodDisruptionBudget")
	}

	t.Logf("Generated %d objects successfully", len(objects))
}
