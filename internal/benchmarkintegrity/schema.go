package benchmarkintegrity

import "github.com/reyoung/pika-go/internal/candidatepolicy"

// DefinitionSchema returns the discoverable JSON Schema for the versioned
// benchmark_integrity object. Workload-specific outer fields remain allowed.
func DefinitionSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"candidate_change_policy": candidatepolicy.Schema(),
			"benchmark_integrity":     definitionContractSchema(),
			"benchmark_measurements":  measurementDefinitionSchema(),
		},
		// The terminal transaction enforces benchmark_measurements for v1
		// Baseline Revisions. Keeping it optional in the static tool catalog is
		// required so grandfathered in-flight Revisions can still finish.
		"required":             []string{"candidate_change_policy", "benchmark_integrity"},
		"additionalProperties": true,
	}
}

// EvidenceSchema returns the discoverable evidence shape. Runtime validation
// applies the exact Full Case Set or Iteration Case Snapshot cardinality.
func EvidenceSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"benchmark_integrity":    evidenceContractSchema(),
			"benchmark_measurements": measurementBaselineSchema(),
		},
		"required":             []string{"benchmark_integrity"},
		"additionalProperties": true,
	}
}

// StrictEvidenceSchema exposes the versioned evidence object when it is
// nested inside another strict MCP contract (for example an Experiment).
func StrictEvidenceSchema() map[string]any { return evidenceContractSchema() }

// StrictMeasurementComparisonSchema exposes benchmark_measurements comparison
// schema when nested inside strict contracts such as Iteration Experiment v1.
func StrictMeasurementComparisonSchema() map[string]any { return measurementComparisonSchema() }

// StrictMeasurementSetSchema is the reference-only/candidate-only v1 shape
// used by flow-v3 Benchmark and Iteration Work.
func StrictMeasurementSetSchema() map[string]any {
	values := map[string]any{"type": "object", "minProperties": 1, "additionalProperties": map[string]any{"type": "number", "exclusiveMinimum": 0}}
	item := strictObject(map[string]any{
		"case_id": map[string]any{"type": "string", "minLength": 1},
		"values":  values,
	}, "case_id", "values")
	return strictObject(map[string]any{
		"schema_version":     map[string]any{"type": "integer", "const": MeasurementSchemaVersion},
		"cases":              map[string]any{"type": "array", "minItems": 1, "items": item},
		"independent_retest": performanceClaimSchema()["properties"].(map[string]any)["independent_retest"],
	}, "schema_version", "cases")
}

// IntegrationSchema returns the Integration validation schema, including the
// performance claim whose >=10x branch is enforced by ValidateIntegration.
func IntegrationSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"benchmark_integrity":    evidenceContractSchema(),
			"benchmark_measurements": measurementComparisonSchema(),
			"performance_claim":      performanceClaimSchema(),
		},
		"required": []string{"benchmark_integrity"},
		"anyOf": []any{
			map[string]any{"required": []string{"benchmark_measurements"}},
			map[string]any{"required": []string{"performance_claim"}},
		},
		"additionalProperties": true,
	}
}

func measurementDefinitionSchema() map[string]any {
	caseSchema := strictObject(map[string]any{
		"case_id": map[string]any{"type": "string", "minLength": 1},
		"weight":  map[string]any{"type": "number", "exclusiveMinimum": 0},
	}, "case_id", "weight")
	metricSchema := strictObject(map[string]any{
		"id":               map[string]any{"type": "string", "minLength": 1},
		"label":            map[string]any{"type": "string", "minLength": 1},
		"unit":             map[string]any{"type": "string", "minLength": 1},
		"role":             map[string]any{"enum": []string{MetricRolePrimary, MetricRoleGuard, MetricRoleInformational}},
		"direction":        map[string]any{"enum": []string{LowerIsBetter, HigherIsBetter}},
		"sample_statistic": map[string]any{"type": "string", "minLength": 1},
		"aggregation":      map[string]any{"enum": []string{WeightedGeomeanOfRatios, RatioOfWeightedArithmeticMeans}},
	}, "id", "label", "unit", "role", "direction", "sample_statistic", "aggregation")
	gateMetricSchema := strictObject(map[string]any{
		"metric_id":                        map[string]any{"type": "string", "minLength": 1},
		"minimum_aggregate_speedup":        map[string]any{"type": "number", "exclusiveMinimum": 0},
		"maximum_case_regression_fraction": map[string]any{"type": "number", "minimum": 0, "exclusiveMaximum": 1},
	}, "metric_id", "minimum_aggregate_speedup", "maximum_case_regression_fraction")
	gateSchema := strictObject(map[string]any{
		"schema_version": map[string]any{"type": "integer", "const": MeasurementSchemaVersion},
		"metrics":        map[string]any{"type": "array", "minItems": 1, "items": gateMetricSchema},
	}, "schema_version", "metrics")
	return strictObject(map[string]any{
		"schema_version":             map[string]any{"type": "integer", "const": MeasurementSchemaVersion},
		"cases":                      map[string]any{"type": "array", "minItems": 1, "items": caseSchema},
		"metrics":                    map[string]any{"type": "array", "minItems": 1, "items": metricSchema},
		"iteration_performance_gate": gateSchema,
	}, "schema_version", "cases", "metrics", "iteration_performance_gate")
}

func measurementBaselineSchema() map[string]any {
	caseSchema := strictObject(map[string]any{
		"case_id": map[string]any{"type": "string", "minLength": 1},
		"values":  map[string]any{"type": "object", "minProperties": 1, "additionalProperties": map[string]any{"type": "number", "exclusiveMinimum": 0}},
	}, "case_id", "values")
	return strictObject(map[string]any{
		"schema_version": map[string]any{"type": "integer", "const": MeasurementSchemaVersion},
		"baseline":       map[string]any{"type": "array", "minItems": 1, "items": caseSchema},
	}, "schema_version", "baseline")
}

func measurementComparisonSchema() map[string]any {
	values := map[string]any{"type": "object", "minProperties": 1, "additionalProperties": map[string]any{"type": "number", "exclusiveMinimum": 0}}
	caseSchema := strictObject(map[string]any{
		"case_id":   map[string]any{"type": "string", "minLength": 1},
		"reference": values,
		"candidate": values,
	}, "case_id", "reference", "candidate")
	return strictObject(map[string]any{
		"schema_version":     map[string]any{"type": "integer", "const": MeasurementSchemaVersion},
		"comparisons":        map[string]any{"type": "array", "minItems": 1, "items": caseSchema},
		"independent_retest": performanceClaimSchema()["properties"].(map[string]any)["independent_retest"],
	}, "schema_version", "comparisons")
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
