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

package controllers

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var _ = Describe("PDB Validation", func() {

	makePDB := func(minAvailable *intstr.IntOrString, maxUnavailable *intstr.IntOrString) *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pdb", Namespace: "default"},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable:   minAvailable,
				MaxUnavailable: maxUnavailable,
			},
		}
	}

	intOrStr := func(val int) *intstr.IntOrString {
		v := intstr.FromInt32(int32(val))
		return &v
	}

	strOrStr := func(val string) *intstr.IntOrString {
		v := intstr.FromString(val)
		return &v
	}

	Describe("MaxUnavailable validation", func() {
		It("should ERROR when maxUnavailable is 0", func() {
			pdb := makePDB(nil, intOrStr(0))
			result := ValidatePDB(pdb, 3)
			Expect(result.BlocksEvictions).To(BeTrue())
			Expect(result.Issues).To(HaveLen(1))
			Expect(result.Issues[0].Code).To(Equal(PDBCodeBlocksAllEvictions))
			Expect(result.Issues[0].Severity).To(Equal(PDBSeverityError))
		})

		It("should ERROR when maxUnavailable is 0%", func() {
			pdb := makePDB(nil, strOrStr("0%"))
			result := ValidatePDB(pdb, 3)
			Expect(result.BlocksEvictions).To(BeTrue())
			Expect(result.Issues).To(HaveLen(1))
			Expect(result.Issues[0].Code).To(Equal(PDBCodeBlocksAllEvictions))
		})

		It("should WARN when maxUnavailable >= expectedPods", func() {
			pdb := makePDB(nil, intOrStr(5))
			result := ValidatePDB(pdb, 3)
			Expect(result.IsNoOp).To(BeTrue())
			Expect(result.Issues).To(HaveLen(1))
			Expect(result.Issues[0].Code).To(Equal(PDBCodeNoEffect))
			Expect(result.Issues[0].Severity).To(Equal(PDBSeverityWarning))
		})

		It("should WARN when maxUnavailable is 100%", func() {
			pdb := makePDB(nil, strOrStr("100%"))
			result := ValidatePDB(pdb, 3)
			Expect(result.IsNoOp).To(BeTrue())
			Expect(result.Issues[0].Code).To(Equal(PDBCodeNoEffect))
		})

		It("should be valid when maxUnavailable is 1 with 3 replicas", func() {
			pdb := makePDB(nil, intOrStr(1))
			result := ValidatePDB(pdb, 3)
			Expect(result.Issues).To(BeEmpty())
			Expect(result.BlocksEvictions).To(BeFalse())
			Expect(result.IsNoOp).To(BeFalse())
		})

		It("should be valid when maxUnavailable is 25%", func() {
			pdb := makePDB(nil, strOrStr("25%"))
			result := ValidatePDB(pdb, 4)
			Expect(result.Issues).To(BeEmpty())
		})
	})

	Describe("MinAvailable validation", func() {
		It("should ERROR when minAvailable >= expectedPods", func() {
			pdb := makePDB(intOrStr(3), nil)
			result := ValidatePDB(pdb, 3)
			Expect(result.BlocksEvictions).To(BeTrue())
			Expect(result.Issues).To(HaveLen(1))
			Expect(result.Issues[0].Code).To(Equal(PDBCodeBlocksAllEvictions))
		})

		It("should ERROR when minAvailable is absurdly high (9999)", func() {
			pdb := makePDB(intOrStr(9999), nil)
			result := ValidatePDB(pdb, 3)
			Expect(result.BlocksEvictions).To(BeTrue())
			Expect(result.Issues[0].Code).To(Equal(PDBCodeBlocksAllEvictions))
		})

		It("should ERROR when minAvailable is 100%", func() {
			pdb := makePDB(strOrStr("100%"), nil)
			result := ValidatePDB(pdb, 3)
			Expect(result.BlocksEvictions).To(BeTrue())
			Expect(result.Issues[0].Code).To(Equal(PDBCodeBlocksAllEvictions))
		})

		It("should ERROR when minAvailable=1 with 1 replica", func() {
			pdb := makePDB(intOrStr(1), nil)
			result := ValidatePDB(pdb, 1)
			Expect(result.BlocksEvictions).To(BeTrue())
			Expect(result.Issues[0].Code).To(Equal(PDBCodeBlocksAllEvictions))
			Expect(result.Issues[0].Message).To(ContainSubstring("single pod"))
		})

		It("should WARN when minAvailable=0", func() {
			pdb := makePDB(intOrStr(0), nil)
			result := ValidatePDB(pdb, 3)
			Expect(result.IsNoOp).To(BeTrue())
			Expect(result.Issues[0].Code).To(Equal(PDBCodeNoEffect))
		})

		It("should WARN when minAvailable=0%", func() {
			pdb := makePDB(strOrStr("0%"), nil)
			result := ValidatePDB(pdb, 3)
			Expect(result.IsNoOp).To(BeTrue())
			Expect(result.Issues[0].Code).To(Equal(PDBCodeNoEffect))
		})

		It("should WARN (tight budget) when minAvailable=1 with 2 replicas", func() {
			pdb := makePDB(intOrStr(1), nil)
			result := ValidatePDB(pdb, 2)
			Expect(result.Issues).To(HaveLen(1))
			Expect(result.Issues[0].Code).To(Equal(PDBCodeTightBudget))
			Expect(result.Issues[0].Severity).To(Equal(PDBSeverityWarning))
		})

		It("should be valid when minAvailable=1 with 3 replicas", func() {
			pdb := makePDB(intOrStr(1), nil)
			result := ValidatePDB(pdb, 3)
			Expect(result.Issues).To(BeEmpty())
		})

		It("should be valid when minAvailable=50% with 4 replicas", func() {
			pdb := makePDB(strOrStr("50%"), nil)
			result := ValidatePDB(pdb, 4)
			Expect(result.Issues).To(BeEmpty())
		})

		It("should WARN (tight budget) when minAvailable=50% resolves to 1 with 2 replicas", func() {
			pdb := makePDB(strOrStr("50%"), nil)
			result := ValidatePDB(pdb, 2)
			// 50% of 2 = 1 (ceil), expectedPods = 2 → tight budget
			Expect(result.Issues).To(HaveLen(1))
			Expect(result.Issues[0].Code).To(Equal(PDBCodeTightBudget))
		})

		It("should ERROR when minAvailable=80% with 1 replica", func() {
			pdb := makePDB(strOrStr("80%"), nil)
			result := ValidatePDB(pdb, 1)
			// 80% of 1 = ceil(0.8) = 1, expectedPods = 1 → blocks
			Expect(result.BlocksEvictions).To(BeTrue())
		})
	})

	Describe("ClassifyPDBSetting", func() {
		It("classifies blocking PDB as blocks_all", func() {
			pdb := makePDB(nil, intOrStr(0))
			Expect(ClassifyPDBSetting(pdb, 3)).To(Equal("blocks_all"))
		})

		It("classifies no-op PDB as no_effect", func() {
			pdb := makePDB(intOrStr(0), nil)
			Expect(ClassifyPDBSetting(pdb, 3)).To(Equal("no_effect"))
		})

		It("classifies tight PDB as tight_budget", func() {
			pdb := makePDB(intOrStr(1), nil)
			Expect(ClassifyPDBSetting(pdb, 2)).To(Equal("tight_budget"))
		})

		It("classifies good PDB as valid", func() {
			pdb := makePDB(intOrStr(1), nil)
			Expect(ClassifyPDBSetting(pdb, 5)).To(Equal("valid"))
		})
	})

	Describe("resolveIntOrPercent", func() {
		It("resolves integer values directly", func() {
			v := intstr.FromInt32(3)
			Expect(resolveIntOrPercent(&v, 10, true)).To(Equal(int32(3)))
		})

		It("resolves percentage with ceil for minAvailable", func() {
			v := intstr.FromString("33%")
			// 33% of 10 = 3.3, ceil = 4
			Expect(resolveIntOrPercent(&v, 10, true)).To(Equal(int32(4)))
		})

		It("resolves percentage with floor for maxUnavailable", func() {
			v := intstr.FromString("33%")
			// 33% of 10 = 3.3, floor = 3
			Expect(resolveIntOrPercent(&v, 10, false)).To(Equal(int32(3)))
		})

		It("resolves 0% to 0", func() {
			v := intstr.FromString("0%")
			Expect(resolveIntOrPercent(&v, 10, true)).To(Equal(int32(0)))
		})

		It("resolves 100% to total", func() {
			v := intstr.FromString("100%")
			Expect(resolveIntOrPercent(&v, 5, true)).To(Equal(int32(5)))
		})

		It("returns 0 for nil", func() {
			Expect(resolveIntOrPercent(nil, 5, true)).To(Equal(int32(0)))
		})
	})

	Describe("Edge cases", func() {
		It("handles nil PDB gracefully", func() {
			result := ValidatePDB(nil, 3)
			Expect(result.Issues).To(BeEmpty())
		})

		It("handles zero expectedPods gracefully", func() {
			pdb := makePDB(intOrStr(1), nil)
			result := ValidatePDB(pdb, 0)
			Expect(result.Issues).To(BeEmpty())
		})

		It("handles negative expectedPods gracefully", func() {
			pdb := makePDB(intOrStr(1), nil)
			result := ValidatePDB(pdb, -1)
			Expect(result.Issues).To(BeEmpty())
		})
	})
})
