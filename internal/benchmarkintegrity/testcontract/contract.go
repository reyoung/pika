// Package testcontract provides the smallest valid benchmark integrity payloads
// for Pika's fake agents and cross-package lifecycle tests.
package testcontract

import "encoding/json"

var (
	definition = json.RawMessage(`{"target":"kernel","benchmark_integrity":{"schema_version":1,"case_ids":["case-1"],"benchmark_repeats":1,"warmup_invocations_per_repeat":0,"measured_invocations_per_repeat":1,"canonical_inputs_device_resident":true,"canonical_inputs_candidate_visible":false,"oracle_outputs_device_resident":true,"oracle_outputs_immutable":true,"working_tensor_addresses_stable":true,"restore_inputs_before_every_invocation":true,"check_outputs_after_every_invocation":true,"device_side_validation":true,"deferred_compact_host_transfer":true,"kernel_timing_excludes_integrity":true,"end_to_end_timing_includes_integrity":true},"benchmark_measurements":{"schema_version":1,"cases":[{"case_id":"case-1","weight":1}],"metrics":[{"id":"latency","label":"Latency","unit":"us","role":"primary","direction":"lower_is_better","sample_statistic":"median","aggregation":"weighted_geomean_of_ratios"}]}}`)
	evidence   = json.RawMessage(`{"verified":true,"benchmark_integrity":{"schema_version":1,"cases":[{"case_id":"case-1","warmup_invocations":0,"measured_invocations":1,"input_restores":1,"checked_invocations":1,"mismatches":0,"nonfinite":0,"canonical_input_mutations":0,"tolerance_passed":true}]},"benchmark_measurements":{"schema_version":1,"baseline":[{"case_id":"case-1","values":{"latency":110}}]}}`)
	validation = json.RawMessage(`{"guard":"passed","benchmark_integrity":{"schema_version":1,"cases":[{"case_id":"case-1","warmup_invocations":0,"measured_invocations":1,"input_restores":1,"checked_invocations":1,"mismatches":0,"nonfinite":0,"canonical_input_mutations":0,"tolerance_passed":true}]},"benchmark_measurements":{"schema_version":1,"comparisons":[{"case_id":"case-1","reference":{"latency":110},"candidate":{"latency":100}}]},"performance_claim":{"schema_version":1,"primary_speedup":1.1,"max_case_speedup":1.1}}`)
)

func Definition() json.RawMessage { return append(json.RawMessage(nil), definition...) }
func Evidence() json.RawMessage   { return append(json.RawMessage(nil), evidence...) }
func Validation() json.RawMessage { return append(json.RawMessage(nil), validation...) }
