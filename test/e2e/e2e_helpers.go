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
	"context"
	"fmt"

	. "github.com/onsi/gomega"
	types "github.com/azure/eviction-autoscaler/api/v1"
	policy "k8s.io/api/policy/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
