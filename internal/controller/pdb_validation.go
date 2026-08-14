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
	"math"
	"strconv"
	"strings"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// PDBValidationSeverity represents the severity of a PDB configuration issue.
type PDBValidationSeverity string

const (
	// PDBSeverityError means the PDB configuration will block all evictions.
	PDBSeverityError PDBValidationSeverity = "ERROR"
	// PDBSeverityWarning means the PDB configuration is likely a misconfiguration or no-op.
	PDBSeverityWarning PDBValidationSeverity = "WARNING"
)

// PDBValidationIssue represents a single validation issue found in a PDB configuration.
type PDBValidationIssue struct {
	Severity PDBValidationSeverity
	Code     string
	Message  string
}

// PDBValidationResult holds the full validation result for a PDB.
type PDBValidationResult struct {
	Issues          []PDBValidationIssue
	BlocksEvictions bool // True if PDB will block ALL evictions
	IsNoOp          bool // True if PDB provides no protection at all
}

const (
	// Validation issue codes
	PDBCodeBlocksAllEvictions = "PDB_BLOCKS_ALL_EVICTIONS"
	PDBCodeNoEffect           = "PDB_NO_EFFECT"
	PDBCodeTightBudget        = "PDB_TIGHT_BUDGET"
)

// ValidatePDB checks a PDB's configuration against the number of expected pods
// and returns any issues found. This helps identify misconfigured PDBs that either
// block all voluntary disruptions (preventing upgrades/drains) or provide no protection.
func ValidatePDB(pdb *policyv1.PodDisruptionBudget, expectedPods int32) PDBValidationResult {
	result := PDBValidationResult{}

	if pdb == nil || expectedPods <= 0 {
		return result
	}

	if pdb.Spec.MaxUnavailable != nil {
		validateMaxUnavailable(pdb.Spec.MaxUnavailable, expectedPods, &result)
	}

	if pdb.Spec.MinAvailable != nil {
		validateMinAvailable(pdb.Spec.MinAvailable, expectedPods, &result)
	}

	return result
}

// validateMaxUnavailable checks maxUnavailable-based PDB configurations.
func validateMaxUnavailable(maxUnavail *intstr.IntOrString, expectedPods int32, result *PDBValidationResult) {
	resolved := resolveIntOrPercent(maxUnavail, expectedPods, false)

	switch {
	case resolved == 0:
		// MaxUnavailable = 0 → nothing can be evicted
		result.Issues = append(result.Issues, PDBValidationIssue{
			Severity: PDBSeverityError,
			Code:     PDBCodeBlocksAllEvictions,
			Message:  "maxUnavailable=0 prevents all voluntary disruptions; cluster upgrades and node drains will be blocked",
		})
		result.BlocksEvictions = true

	case resolved >= expectedPods:
		// MaxUnavailable >= expectedPods → PDB is a no-op
		result.Issues = append(result.Issues, PDBValidationIssue{
			Severity: PDBSeverityWarning,
			Code:     PDBCodeNoEffect,
			Message:  "maxUnavailable >= expectedPods; PDB provides no disruption protection",
		})
		result.IsNoOp = true
	}
}

// validateMinAvailable checks minAvailable-based PDB configurations.
func validateMinAvailable(minAvail *intstr.IntOrString, expectedPods int32, result *PDBValidationResult) {
	resolved := resolveIntOrPercent(minAvail, expectedPods, true)

	switch {
	case resolved >= expectedPods:
		// MinAvailable >= expectedPods → nothing can be evicted
		result.Issues = append(result.Issues, PDBValidationIssue{
			Severity: PDBSeverityError,
			Code:     PDBCodeBlocksAllEvictions,
			Message:  "minAvailable >= expectedPods; no pod can ever be evicted",
		})
		result.BlocksEvictions = true

	case resolved == 0:
		// MinAvailable = 0 → PDB is a no-op
		result.Issues = append(result.Issues, PDBValidationIssue{
			Severity: PDBSeverityWarning,
			Code:     PDBCodeNoEffect,
			Message:  "minAvailable=0 allows all pods to be disrupted; PDB provides no protection",
		})
		result.IsNoOp = true

	case expectedPods == 2 && resolved == 1:
		// MinAvailable = 1 with 2 replicas → tight budget, only 1 pod can drain at a time
		result.Issues = append(result.Issues, PDBValidationIssue{
			Severity: PDBSeverityWarning,
			Code:     PDBCodeTightBudget,
			Message:  "minAvailable=1 with 2 replicas; only 1 pod can drain at a time which may slow upgrades",
		})

	case expectedPods == 1 && resolved == 1:
		// MinAvailable = 1 with 1 replica → the single pod can never be evicted
		result.Issues = append(result.Issues, PDBValidationIssue{
			Severity: PDBSeverityError,
			Code:     PDBCodeBlocksAllEvictions,
			Message:  "minAvailable=1 with only 1 replica; the single pod can never be evicted",
		})
		result.BlocksEvictions = true
	}
}

// resolveIntOrPercent resolves an IntOrString value against the total pod count.
// For percentages with minAvailable, it rounds up (ceil); for maxUnavailable, it rounds down (floor).
func resolveIntOrPercent(val *intstr.IntOrString, total int32, roundUp bool) int32 {
	if val == nil {
		return 0
	}

	if val.Type == intstr.Int {
		return val.IntVal
	}

	// String type — must be a percentage
	strVal := val.StrVal
	if !strings.HasSuffix(strVal, "%") {
		// Try parsing as bare integer string
		if intVal, err := strconv.ParseInt(strVal, 10, 32); err == nil {
			return int32(intVal)
		}
		return 0
	}

	percentStr := strings.TrimSuffix(strVal, "%")
	percentage, err := strconv.ParseFloat(percentStr, 64)
	if err != nil {
		return 0
	}

	value := float64(total) * percentage / 100.0
	if roundUp {
		return int32(math.Ceil(value))
	}
	return int32(math.Floor(value))
}

// ClassifyPDBSetting returns a classification label for metrics tracking.
// Returns one of: "blocks_all", "no_effect", "tight_budget", "valid"
func ClassifyPDBSetting(pdb *policyv1.PodDisruptionBudget, expectedPods int32) string {
	result := ValidatePDB(pdb, expectedPods)
	if result.BlocksEvictions {
		return "blocks_all"
	}
	if result.IsNoOp {
		return "no_effect"
	}
	for _, issue := range result.Issues {
		if issue.Code == PDBCodeTightBudget {
			return "tight_budget"
		}
	}
	return "valid"
}
