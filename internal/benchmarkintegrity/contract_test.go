package benchmarkintegrity_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
)

const validDefinition = `{
  "target": "kernel",
  "benchmark_integrity": {
    "schema_version": 1,
    "case_ids": ["case-a", "case-b"],
    "benchmark_repeats": 5,
    "warmup_invocations_per_repeat": 10,
    "measured_invocations_per_repeat": 100,
    "canonical_inputs_device_resident": true,
    "canonical_inputs_candidate_visible": false,
    "oracle_outputs_device_resident": true,
    "oracle_outputs_immutable": true,
    "working_tensor_addresses_stable": true,
    "restore_inputs_before_every_invocation": true,
    "check_outputs_after_every_invocation": true,
    "device_side_validation": true,
    "deferred_compact_host_transfer": true,
    "kernel_timing_excludes_integrity": true,
    "end_to_end_timing_includes_integrity": true
  }
}`

const validEvidence = `{
  "summary": "full matrix passed",
  "benchmark_integrity": {
    "schema_version": 1,
    "cases": [
      {"case_id":"case-a","warmup_invocations":50,"measured_invocations":500,"input_restores":550,"checked_invocations":550,"mismatches":0,"nonfinite":0,"canonical_input_mutations":0,"tolerance_passed":true},
      {"case_id":"case-b","warmup_invocations":50,"measured_invocations":500,"input_restores":550,"checked_invocations":550,"mismatches":0,"nonfinite":0,"canonical_input_mutations":0,"tolerance_passed":true}
    ]
  }
}`

func TestDefinitionRequiresVersionedPerInvocationContract(t *testing.T) {
	t.Parallel()

	if err := benchmarkintegrity.ValidateDefinition(json.RawMessage(validDefinition)); err != nil {
		t.Fatalf("ValidateDefinition(valid) = %v", err)
	}
	for name, input := range map[string]string{
		"missing contract":               `{"target":"kernel"}`,
		"missing warmup field":           strings.Replace(validDefinition, "    \"warmup_invocations_per_repeat\": 10,\n", "", 1),
		"candidate sees canonical input": strings.Replace(validDefinition, `"canonical_inputs_candidate_visible": false`, `"canonical_inputs_candidate_visible": true`, 1),
		"last invocation only":           strings.Replace(validDefinition, `"check_outputs_after_every_invocation": true`, `"check_outputs_after_every_invocation": false`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := benchmarkintegrity.ValidateDefinition(json.RawMessage(input)); err == nil {
				t.Fatal("ValidateDefinition unexpectedly accepted invalid contract")
			}
		})
	}
}

func TestEvidenceRequiresEveryBenchmarkInvocationToBeChecked(t *testing.T) {
	t.Parallel()

	if err := benchmarkintegrity.ValidateEvidence(json.RawMessage(validDefinition), json.RawMessage(validEvidence)); err != nil {
		t.Fatalf("ValidateEvidence(valid) = %v", err)
	}
	unchecked := strings.Replace(validEvidence, `"checked_invocations":550`, `"checked_invocations":549`, 1)
	if err := benchmarkintegrity.ValidateEvidence(json.RawMessage(validDefinition), json.RawMessage(unchecked)); err == nil || !strings.Contains(err.Error(), "checked_invocations") {
		t.Fatalf("ValidateEvidence(unchecked) = %v, want checked_invocations error", err)
	}
	mutated := strings.Replace(validEvidence, `"canonical_input_mutations":0`, `"canonical_input_mutations":1`, 1)
	if err := benchmarkintegrity.ValidateEvidence(json.RawMessage(validDefinition), json.RawMessage(mutated)); err == nil || !strings.Contains(err.Error(), "canonical_input_mutations") {
		t.Fatalf("ValidateEvidence(mutated) = %v, want canonical_input_mutations error", err)
	}
	missingZeroCount := strings.Replace(validEvidence, `"mismatches":0,`, ``, 1)
	if err := benchmarkintegrity.ValidateEvidence(json.RawMessage(validDefinition), json.RawMessage(missingZeroCount)); err == nil || !strings.Contains(err.Error(), "mismatches") {
		t.Fatalf("ValidateEvidence(missing mismatch count) = %v, want mismatches error", err)
	}
}

func TestIterationEvidenceCoversExactlyRoundSnapshot(t *testing.T) {
	t.Parallel()

	caseAOnly := strings.Replace(validEvidence, `,
      {"case_id":"case-b","warmup_invocations":50,"measured_invocations":500,"input_restores":550,"checked_invocations":550,"mismatches":0,"nonfinite":0,"canonical_input_mutations":0,"tolerance_passed":true}`, "", 1)
	if err := benchmarkintegrity.ValidateIterationEvidence(json.RawMessage(validDefinition), []string{"case-a"}, json.RawMessage(caseAOnly)); err != nil {
		t.Fatalf("ValidateIterationEvidence(snapshot) = %v", err)
	}
	if err := benchmarkintegrity.ValidateIterationEvidence(json.RawMessage(validDefinition), []string{"case-a"}, json.RawMessage(validEvidence)); err == nil || !strings.Contains(err.Error(), "exactly 1") {
		t.Fatalf("ValidateIterationEvidence(extra case) = %v, want exact coverage error", err)
	}
	if err := benchmarkintegrity.ValidateIterationEvidence(json.RawMessage(validDefinition), []string{"missing"}, json.RawMessage(caseAOnly)); err == nil || !strings.Contains(err.Error(), "Full Case Set") {
		t.Fatalf("ValidateIterationEvidence(invalid snapshot) = %v, want Full Case Set error", err)
	}
	got, err := benchmarkintegrity.FullCaseIDs(json.RawMessage(validDefinition))
	if err != nil || len(got) != 2 || got[0] != "case-a" || got[1] != "case-b" {
		t.Fatalf("FullCaseIDs = %v, %v", got, err)
	}
}

func TestIntegrationValidationEscalatesTenXClaims(t *testing.T) {
	t.Parallel()

	lowSpeedup := json.RawMessage(strings.TrimSuffix(validEvidence, "\n}") + `,
  "performance_claim": {"schema_version":1,"primary_speedup":1.4,"max_case_speedup":2.1}
}`)
	if err := benchmarkintegrity.ValidateIntegration(json.RawMessage(validDefinition), lowSpeedup); err != nil {
		t.Fatalf("ValidateIntegration(low speedup) = %v", err)
	}

	highSpeedupWithoutRetest := json.RawMessage(strings.Replace(string(lowSpeedup), `"max_case_speedup":2.1`, `"max_case_speedup":10.0`, 1))
	if err := benchmarkintegrity.ValidateIntegration(json.RawMessage(validDefinition), highSpeedupWithoutRetest); err == nil || !strings.Contains(err.Error(), "independent_retest") {
		t.Fatalf("ValidateIntegration(high speedup without retest) = %v, want independent_retest error", err)
	}

	highSpeedup := json.RawMessage(strings.Replace(string(highSpeedupWithoutRetest), `"max_case_speedup":10.0`, `"max_case_speedup":10.0,"independent_retest":{"passed":true,"changed_canonical_inputs":true,"output_sentinel":true,"cold_start_reported":true,"setup_reported":true,"steady_state_reported":true,"end_to_end_reported":true,"timing_boundary_fair":true}`, 1))
	if err := benchmarkintegrity.ValidateIntegration(json.RawMessage(validDefinition), highSpeedup); err != nil {
		t.Fatalf("ValidateIntegration(high speedup with retest) = %v", err)
	}
}

func TestToolSchemasExposeVersionedContracts(t *testing.T) {
	t.Parallel()

	for name, schema := range map[string]map[string]any{
		"definition":  benchmarkintegrity.DefinitionSchema(),
		"evidence":    benchmarkintegrity.EvidenceSchema(),
		"integration": benchmarkintegrity.IntegrationSchema(),
	} {
		encoded, err := json.Marshal(schema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", name, err)
		}
		if !strings.Contains(string(encoded), `"benchmark_integrity"`) || !strings.Contains(string(encoded), `"schema_version"`) {
			t.Fatalf("%s schema does not expose versioned benchmark integrity: %s", name, encoded)
		}
		if name == "integration" && (!strings.Contains(string(encoded), `"performance_claim"`) || !strings.Contains(string(encoded), `"independent_retest"`)) {
			t.Fatalf("Integration schema omits suspicious-speedup evidence: %s", encoded)
		}
	}
}
