package benchmarkintegrity

// DefinitionSchema returns the discoverable JSON Schema for the versioned
// benchmark_integrity object. Workload-specific outer fields remain allowed.
func DefinitionSchema() map[string]any {
	return envelopeSchema("benchmark_integrity", definitionContractSchema())
}

// EvidenceSchema returns the discoverable evidence shape. Runtime validation
// applies the exact Full Case Set or Iteration Case Snapshot cardinality.
func EvidenceSchema() map[string]any {
	return envelopeSchema("benchmark_integrity", evidenceContractSchema())
}

// IntegrationSchema returns the Integration validation schema, including the
// performance claim whose >=10x branch is enforced by ValidateIntegration.
func IntegrationSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"benchmark_integrity": evidenceContractSchema(),
			"performance_claim":   performanceClaimSchema(),
		},
		"required":             []string{"benchmark_integrity", "performance_claim"},
		"additionalProperties": true,
	}
}

func envelopeSchema(field string, schema map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{field: schema},
		"required":             []string{field},
		"additionalProperties": true,
	}
}

func strictObject(properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             append([]string{}, required...),
		"additionalProperties": false,
	}
}

func definitionContractSchema() map[string]any {
	properties := map[string]any{
		"schema_version":                         map[string]any{"type": "integer", "const": SchemaVersion},
		"case_ids":                               map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1}},
		"benchmark_repeats":                      map[string]any{"type": "integer", "minimum": 1},
		"warmup_invocations_per_repeat":          map[string]any{"type": "integer", "minimum": 0},
		"measured_invocations_per_repeat":        map[string]any{"type": "integer", "minimum": 1},
		"canonical_inputs_device_resident":       map[string]any{"const": true},
		"canonical_inputs_candidate_visible":     map[string]any{"const": false},
		"oracle_outputs_device_resident":         map[string]any{"const": true},
		"oracle_outputs_immutable":               map[string]any{"const": true},
		"working_tensor_addresses_stable":        map[string]any{"const": true},
		"restore_inputs_before_every_invocation": map[string]any{"const": true},
		"check_outputs_after_every_invocation":   map[string]any{"const": true},
		"device_side_validation":                 map[string]any{"const": true},
		"deferred_compact_host_transfer":         map[string]any{"const": true},
		"kernel_timing_excludes_integrity":       map[string]any{"const": true},
		"end_to_end_timing_includes_integrity":   map[string]any{"const": true},
	}
	required := []string{
		"schema_version", "case_ids", "benchmark_repeats", "warmup_invocations_per_repeat", "measured_invocations_per_repeat",
		"canonical_inputs_device_resident", "canonical_inputs_candidate_visible", "oracle_outputs_device_resident", "oracle_outputs_immutable",
		"working_tensor_addresses_stable", "restore_inputs_before_every_invocation", "check_outputs_after_every_invocation", "device_side_validation",
		"deferred_compact_host_transfer", "kernel_timing_excludes_integrity", "end_to_end_timing_includes_integrity",
	}
	return strictObject(properties, required...)
}

func evidenceContractSchema() map[string]any {
	caseSchema := strictObject(map[string]any{
		"case_id":                   map[string]any{"type": "string", "minLength": 1},
		"warmup_invocations":        map[string]any{"type": "integer", "minimum": 0},
		"measured_invocations":      map[string]any{"type": "integer", "minimum": 1},
		"input_restores":            map[string]any{"type": "integer", "minimum": 1},
		"checked_invocations":       map[string]any{"type": "integer", "minimum": 1},
		"mismatches":                map[string]any{"type": "integer", "const": 0},
		"nonfinite":                 map[string]any{"type": "integer", "const": 0},
		"canonical_input_mutations": map[string]any{"type": "integer", "const": 0},
		"tolerance_passed":          map[string]any{"const": true},
	}, "case_id", "warmup_invocations", "measured_invocations", "input_restores", "checked_invocations", "mismatches", "nonfinite", "canonical_input_mutations", "tolerance_passed")
	return strictObject(map[string]any{
		"schema_version": map[string]any{"type": "integer", "const": SchemaVersion},
		"cases":          map[string]any{"type": "array", "minItems": 1, "items": caseSchema},
	}, "schema_version", "cases")
}

func performanceClaimSchema() map[string]any {
	retest := strictObject(map[string]any{
		"passed":                   map[string]any{"const": true},
		"changed_canonical_inputs": map[string]any{"const": true},
		"output_sentinel":          map[string]any{"const": true},
		"cold_start_reported":      map[string]any{"const": true},
		"setup_reported":           map[string]any{"const": true},
		"steady_state_reported":    map[string]any{"const": true},
		"end_to_end_reported":      map[string]any{"const": true},
		"timing_boundary_fair":     map[string]any{"const": true},
	}, "passed", "changed_canonical_inputs", "output_sentinel", "cold_start_reported", "setup_reported", "steady_state_reported", "end_to_end_reported", "timing_boundary_fair")
	return strictObject(map[string]any{
		"schema_version":     map[string]any{"type": "integer", "const": SchemaVersion},
		"primary_speedup":    map[string]any{"type": "number", "exclusiveMinimum": 0},
		"max_case_speedup":   map[string]any{"type": "number", "exclusiveMinimum": 0},
		"independent_retest": retest,
	}, "schema_version", "primary_speedup", "max_case_speedup")
}
