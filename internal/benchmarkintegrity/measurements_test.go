package benchmarkintegrity_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
)

func TestMeasurementContractComputesRatiosAndAggregates(t *testing.T) {
	t.Parallel()

	definition := json.RawMessage(`{
		"benchmark_measurements": {
			"schema_version": 1,
			"cases": [
				{"case_id": "case-a", "weight": 1},
				{"case_id": "case-b", "weight": 3}
			],
			"metrics": [
				{"id": "latency", "label": "Latency", "unit": "us", "role": "primary", "direction": "lower_is_better", "sample_statistic": "median", "aggregation": "weighted_geomean_of_ratios"},
				{"id": "throughput", "label": "Throughput", "unit": "items/s", "role": "informational", "direction": "higher_is_better", "sample_statistic": "median", "aggregation": "ratio_of_weighted_arithmetic_means"}
			]
		}
	}`)
	validation := json.RawMessage(`{
		"benchmark_measurements": {
			"schema_version": 1,
			"comparisons": [
				{"case_id": "case-a", "reference": {"latency": 100, "throughput": 10}, "candidate": {"latency": 50, "throughput": 20}},
				{"case_id": "case-b", "reference": {"latency": 80, "throughput": 20}, "candidate": {"latency": 40, "throughput": 40}}
			]
		}
	}`)

	contract, err := benchmarkintegrity.ParseMeasurementDefinition(definition)
	if err != nil {
		t.Fatalf("ParseMeasurementDefinition() = %v", err)
	}
	comparison, err := benchmarkintegrity.ParseMeasurementComparison(contract, validation)
	if err != nil {
		t.Fatalf("ParseMeasurementComparison() = %v", err)
	}
	if comparison.PrimaryMetricID != "latency" {
		t.Fatalf("PrimaryMetricID = %q, want latency", comparison.PrimaryMetricID)
	}
	assertClose(t, comparison.Metrics["latency"].AggregateSpeedup, 2)
	assertClose(t, comparison.Metrics["latency"].MaxCaseSpeedup, 2)
	assertClose(t, comparison.Metrics["throughput"].AggregateSpeedup, 2)
	if got := comparison.Cases[0].Metrics["latency"].Speedup; got != 2 {
		t.Fatalf("case-a latency speedup = %v, want 2", got)
	}
}

func TestMeasurementContractUsesComputedTenXGate(t *testing.T) {
	t.Parallel()
	definition := json.RawMessage(`{"benchmark_measurements":{"schema_version":1,"cases":[{"case_id":"case-a","weight":1}],"metrics":[{"id":"latency","label":"Latency","unit":"us","role":"primary","direction":"lower_is_better","sample_statistic":"median","aggregation":"weighted_geomean_of_ratios"}]}}`)
	contract, err := benchmarkintegrity.ParseMeasurementDefinition(definition)
	if err != nil {
		t.Fatal(err)
	}
	withoutRetest := json.RawMessage(`{"performance_claim":{"primary_speedup":1.01},"benchmark_measurements":{"schema_version":1,"comparisons":[{"case_id":"case-a","reference":{"latency":100},"candidate":{"latency":10}}]}}`)
	if _, err := benchmarkintegrity.ParseMeasurementComparison(contract, withoutRetest); err == nil || !strings.Contains(err.Error(), "independent_retest") {
		t.Fatalf("ParseMeasurementComparison() = %v, want computed 10x gate", err)
	}
	withRetest := json.RawMessage(`{"performance_claim":{"primary_speedup":1.01},"benchmark_measurements":{"schema_version":1,"comparisons":[{"case_id":"case-a","reference":{"latency":100},"candidate":{"latency":10}}],"independent_retest":{"passed":true,"changed_canonical_inputs":true,"output_sentinel":true,"cold_start_reported":true,"setup_reported":true,"steady_state_reported":true,"end_to_end_reported":true,"timing_boundary_fair":true}}}`)
	comparison, err := benchmarkintegrity.ParseMeasurementComparison(contract, withRetest)
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, comparison.Metrics["latency"].AggregateSpeedup, 10)
}

func TestBaselineMeasurementsRequireExactFrozenMatrix(t *testing.T) {
	t.Parallel()
	definition := json.RawMessage(`{"benchmark_measurements":{"schema_version":1,"cases":[{"case_id":"case-a","weight":1}],"metrics":[{"id":"latency","label":"Latency","unit":"us","role":"primary","direction":"lower_is_better","sample_statistic":"median","aggregation":"weighted_geomean_of_ratios"}]}}`)
	contract, err := benchmarkintegrity.ParseMeasurementDefinition(definition)
	if err != nil {
		t.Fatal(err)
	}
	set, err := benchmarkintegrity.ParseBaselineMeasurements(contract, json.RawMessage(`{"benchmark_measurements":{"schema_version":1,"baseline":[{"case_id":"case-a","values":{"latency":123.5}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	assertClose(t, set.Cases[0].Values["latency"], 123.5)
	if _, err := benchmarkintegrity.ParseBaselineMeasurements(contract, json.RawMessage(`{"benchmark_measurements":{"schema_version":1,"baseline":[{"case_id":"case-a","values":{}}]}}`)); err == nil {
		t.Fatal("ParseBaselineMeasurements accepted a missing metric")
	}
}

func assertClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("got %v, want %v", got, want)
	}
}
