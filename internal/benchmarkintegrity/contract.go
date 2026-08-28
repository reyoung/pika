// Package benchmarkintegrity validates the versioned benchmark correctness
// protocol carried by Baseline definitions and acceptance evidence.
package benchmarkintegrity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
)

const (
	SchemaVersion              = 1
	SuspiciousSpeedupThreshold = 10.0
)

type definitionContract struct {
	SchemaVersion                      int      `json:"schema_version"`
	CaseIDs                            []string `json:"case_ids"`
	BenchmarkRepeats                   *int64   `json:"benchmark_repeats"`
	WarmupInvocationsPerRepeat         *int64   `json:"warmup_invocations_per_repeat"`
	MeasuredInvocationsPerRepeat       *int64   `json:"measured_invocations_per_repeat"`
	CanonicalInputsDeviceResident      *bool    `json:"canonical_inputs_device_resident"`
	CanonicalInputsCandidateVisible    *bool    `json:"canonical_inputs_candidate_visible"`
	OracleOutputsDeviceResident        *bool    `json:"oracle_outputs_device_resident"`
	OracleOutputsImmutable             *bool    `json:"oracle_outputs_immutable"`
	WorkingTensorAddressesStable       *bool    `json:"working_tensor_addresses_stable"`
	RestoreInputsBeforeEveryInvocation *bool    `json:"restore_inputs_before_every_invocation"`
	CheckOutputsAfterEveryInvocation   *bool    `json:"check_outputs_after_every_invocation"`
	DeviceSideValidation               *bool    `json:"device_side_validation"`
	DeferredCompactHostTransfer        *bool    `json:"deferred_compact_host_transfer"`
	KernelTimingExcludesIntegrity      *bool    `json:"kernel_timing_excludes_integrity"`
	EndToEndTimingIncludesIntegrity    *bool    `json:"end_to_end_timing_includes_integrity"`
}

type evidenceContract struct {
	SchemaVersion int            `json:"schema_version"`
	Cases         []caseEvidence `json:"cases"`
}

type caseEvidence struct {
	CaseID                  string `json:"case_id"`
	WarmupInvocations       *int64 `json:"warmup_invocations"`
	MeasuredInvocations     *int64 `json:"measured_invocations"`
	InputRestores           *int64 `json:"input_restores"`
	CheckedInvocations      *int64 `json:"checked_invocations"`
	Mismatches              *int64 `json:"mismatches"`
	Nonfinite               *int64 `json:"nonfinite"`
	CanonicalInputMutations *int64 `json:"canonical_input_mutations"`
	TolerancePassed         *bool  `json:"tolerance_passed"`
}

type performanceClaim struct {
	SchemaVersion  int                `json:"schema_version"`
	PrimarySpeedup float64            `json:"primary_speedup"`
	MaxCaseSpeedup float64            `json:"max_case_speedup"`
	Retest         *independentRetest `json:"independent_retest,omitempty"`
}

type independentRetest struct {
	Passed                 *bool `json:"passed"`
	ChangedCanonicalInputs *bool `json:"changed_canonical_inputs"`
	OutputSentinel         *bool `json:"output_sentinel"`
	ColdStartReported      *bool `json:"cold_start_reported"`
	SetupReported          *bool `json:"setup_reported"`
	SteadyStateReported    *bool `json:"steady_state_reported"`
	EndToEndReported       *bool `json:"end_to_end_reported"`
	TimingBoundaryFair     *bool `json:"timing_boundary_fair"`
}

// ValidateDefinition validates the benchmark_integrity contract embedded in a
// Baseline Definition. Other Definition fields remain owned by the workload.
func ValidateDefinition(raw json.RawMessage) error {
	_, err := parseDefinition(raw)
	return err
}

// ValidateEvidence proves that accepted evidence covers exactly the frozen
// Full Case Set and accounts for every warm-up and measured invocation.
func ValidateEvidence(definition, evidence json.RawMessage) error {
	contract, err := parseDefinition(definition)
	if err != nil {
		return fmt.Errorf("definition: %w", err)
	}
	return validateEvidence(contract, evidence)
}

// ValidateIntegration applies the evidence contract and escalates primary or
// per-Case performance claims at or above one order of magnitude.
func ValidateIntegration(definition, validation json.RawMessage) error {
	contract, err := parseDefinition(definition)
	if err != nil {
		return fmt.Errorf("definition: %w", err)
	}
	if err := validateEvidence(contract, validation); err != nil {
		return err
	}
	var claim performanceClaim
	if err := decodeField(validation, "performance_claim", &claim); err != nil {
		return err
	}
	if claim.SchemaVersion != SchemaVersion {
		return fmt.Errorf("performance_claim.schema_version must be %d", SchemaVersion)
	}
	if !finitePositive(claim.PrimarySpeedup) || !finitePositive(claim.MaxCaseSpeedup) {
		return fmt.Errorf("performance_claim speedups must be finite and greater than zero")
	}
	if math.Max(claim.PrimarySpeedup, claim.MaxCaseSpeedup) < SuspiciousSpeedupThreshold {
		return nil
	}
	if claim.Retest == nil {
		return fmt.Errorf("performance_claim.independent_retest is required for speedups >= %.0fx", SuspiciousSpeedupThreshold)
	}
	checks := []struct {
		name  string
		value *bool
	}{
		{"passed", claim.Retest.Passed},
		{"changed_canonical_inputs", claim.Retest.ChangedCanonicalInputs},
		{"output_sentinel", claim.Retest.OutputSentinel},
		{"cold_start_reported", claim.Retest.ColdStartReported},
		{"setup_reported", claim.Retest.SetupReported},
		{"steady_state_reported", claim.Retest.SteadyStateReported},
		{"end_to_end_reported", claim.Retest.EndToEndReported},
		{"timing_boundary_fair", claim.Retest.TimingBoundaryFair},
	}
	for _, check := range checks {
		if check.value == nil || !*check.value {
			return fmt.Errorf("performance_claim.independent_retest.%s must be true for speedups >= %.0fx", check.name, SuspiciousSpeedupThreshold)
		}
	}
	return nil
}

func parseDefinition(raw json.RawMessage) (definitionContract, error) {
	var contract definitionContract
	if err := decodeField(raw, "benchmark_integrity", &contract); err != nil {
		return definitionContract{}, err
	}
	if contract.SchemaVersion != SchemaVersion {
		return definitionContract{}, fmt.Errorf("benchmark_integrity.schema_version must be %d", SchemaVersion)
	}
	if len(contract.CaseIDs) == 0 {
		return definitionContract{}, fmt.Errorf("benchmark_integrity.case_ids must be non-empty")
	}
	seen := make(map[string]struct{}, len(contract.CaseIDs))
	for index, caseID := range contract.CaseIDs {
		if caseID == "" {
			return definitionContract{}, fmt.Errorf("benchmark_integrity.case_ids[%d] must be non-empty", index)
		}
		if _, exists := seen[caseID]; exists {
			return definitionContract{}, fmt.Errorf("benchmark_integrity.case_ids contains duplicate %q", caseID)
		}
		seen[caseID] = struct{}{}
	}
	if contract.BenchmarkRepeats == nil || contract.WarmupInvocationsPerRepeat == nil || contract.MeasuredInvocationsPerRepeat == nil ||
		*contract.BenchmarkRepeats <= 0 || *contract.WarmupInvocationsPerRepeat < 0 || *contract.MeasuredInvocationsPerRepeat <= 0 {
		return definitionContract{}, fmt.Errorf("benchmark_integrity requires positive benchmark_repeats and measured_invocations_per_repeat, and non-negative warmup_invocations_per_repeat")
	}
	if _, _, _, err := expectedCounts(contract); err != nil {
		return definitionContract{}, err
	}
	requiredTrue := []struct {
		name  string
		value *bool
	}{
		{"canonical_inputs_device_resident", contract.CanonicalInputsDeviceResident},
		{"oracle_outputs_device_resident", contract.OracleOutputsDeviceResident},
		{"oracle_outputs_immutable", contract.OracleOutputsImmutable},
		{"working_tensor_addresses_stable", contract.WorkingTensorAddressesStable},
		{"restore_inputs_before_every_invocation", contract.RestoreInputsBeforeEveryInvocation},
		{"check_outputs_after_every_invocation", contract.CheckOutputsAfterEveryInvocation},
		{"device_side_validation", contract.DeviceSideValidation},
		{"deferred_compact_host_transfer", contract.DeferredCompactHostTransfer},
		{"kernel_timing_excludes_integrity", contract.KernelTimingExcludesIntegrity},
		{"end_to_end_timing_includes_integrity", contract.EndToEndTimingIncludesIntegrity},
	}
	for _, requirement := range requiredTrue {
		if requirement.value == nil || !*requirement.value {
			return definitionContract{}, fmt.Errorf("benchmark_integrity.%s must be true", requirement.name)
		}
	}
	if contract.CanonicalInputsCandidateVisible == nil || *contract.CanonicalInputsCandidateVisible {
		return definitionContract{}, fmt.Errorf("benchmark_integrity.canonical_inputs_candidate_visible must be false")
	}
	return contract, nil
}

func validateEvidence(contract definitionContract, raw json.RawMessage) error {
	var evidence evidenceContract
	if err := decodeField(raw, "benchmark_integrity", &evidence); err != nil {
		return err
	}
	if evidence.SchemaVersion != SchemaVersion {
		return fmt.Errorf("benchmark_integrity.schema_version must be %d", SchemaVersion)
	}
	if len(evidence.Cases) != len(contract.CaseIDs) {
		return fmt.Errorf("benchmark_integrity.cases must cover exactly %d frozen cases", len(contract.CaseIDs))
	}
	warmups, measured, invocations, err := expectedCounts(contract)
	if err != nil {
		return err
	}
	wanted := make(map[string]struct{}, len(contract.CaseIDs))
	for _, caseID := range contract.CaseIDs {
		wanted[caseID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(evidence.Cases))
	for index, item := range evidence.Cases {
		prefix := fmt.Sprintf("benchmark_integrity.cases[%d]", index)
		if _, exists := wanted[item.CaseID]; !exists {
			return fmt.Errorf("%s.case_id %q is not in the frozen Full Case Set", prefix, item.CaseID)
		}
		if _, exists := seen[item.CaseID]; exists {
			return fmt.Errorf("%s.case_id %q is duplicated", prefix, item.CaseID)
		}
		seen[item.CaseID] = struct{}{}
		if item.WarmupInvocations == nil || *item.WarmupInvocations != warmups {
			return fmt.Errorf("%s.warmup_invocations must equal %d", prefix, warmups)
		}
		if item.MeasuredInvocations == nil || *item.MeasuredInvocations != measured {
			return fmt.Errorf("%s.measured_invocations must equal %d", prefix, measured)
		}
		if item.InputRestores == nil || *item.InputRestores != invocations {
			return fmt.Errorf("%s.input_restores must equal %d", prefix, invocations)
		}
		if item.CheckedInvocations == nil || *item.CheckedInvocations != invocations {
			return fmt.Errorf("%s.checked_invocations must equal %d", prefix, invocations)
		}
		if item.Mismatches == nil || item.Nonfinite == nil || item.CanonicalInputMutations == nil || *item.Mismatches != 0 || *item.Nonfinite != 0 || *item.CanonicalInputMutations != 0 {
			return fmt.Errorf("%s requires mismatches=0, nonfinite=0, and canonical_input_mutations=0", prefix)
		}
		if item.TolerancePassed == nil || !*item.TolerancePassed {
			return fmt.Errorf("%s.tolerance_passed must be true", prefix)
		}
	}
	return nil
}

func expectedCounts(contract definitionContract) (warmups, measured, invocations int64, err error) {
	multiply := func(left, right int64) (int64, error) {
		if left != 0 && right > math.MaxInt64/left {
			return 0, fmt.Errorf("benchmark_integrity invocation count overflows int64")
		}
		return left * right, nil
	}
	warmups, err = multiply(*contract.BenchmarkRepeats, *contract.WarmupInvocationsPerRepeat)
	if err != nil {
		return 0, 0, 0, err
	}
	measured, err = multiply(*contract.BenchmarkRepeats, *contract.MeasuredInvocationsPerRepeat)
	if err != nil {
		return 0, 0, 0, err
	}
	if warmups > math.MaxInt64-measured {
		return 0, 0, 0, fmt.Errorf("benchmark_integrity invocation count overflows int64")
	}
	return warmups, measured, warmups + measured, nil
}

func decodeField(raw json.RawMessage, field string, target any) error {
	if !json.Valid(raw) {
		return fmt.Errorf("payload must be valid JSON")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope == nil {
		return fmt.Errorf("payload must be a JSON object")
	}
	value, exists := envelope[field]
	if !exists {
		return fmt.Errorf("%s is required", field)
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%s must contain exactly one JSON object", field)
	}
	return nil
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}
