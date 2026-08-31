package benchmarkintegrity

import (
	"encoding/json"
	"fmt"
	"math"
)

const MeasurementSchemaVersion = 1

const (
	MetricRolePrimary       = "primary"
	MetricRoleGuard         = "guard"
	MetricRoleInformational = "informational"

	LowerIsBetter  = "lower_is_better"
	HigherIsBetter = "higher_is_better"

	WeightedGeomeanOfRatios        = "weighted_geomean_of_ratios"
	RatioOfWeightedArithmeticMeans = "ratio_of_weighted_arithmetic_means"
)

// MeasurementDefinition is the frozen, workload-independent benchmark model.
// It is deliberately expressed in domain terms so storage and UI code do not
// need to interpret the raw Definition JSON.
type MeasurementDefinition struct {
	SchemaVersion   int                 `json:"schema_version"`
	Cases           []MeasurementCase   `json:"cases"`
	Metrics         []MeasurementMetric `json:"metrics"`
	PrimaryMetricID string              `json:"-"`
}

type MeasurementCase struct {
	CaseID string  `json:"case_id"`
	Weight float64 `json:"weight"`
}

type MeasurementMetric struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	Unit            string `json:"unit"`
	Role            string `json:"role"`
	Direction       string `json:"direction"`
	SampleStatistic string `json:"sample_statistic"`
	Aggregation     string `json:"aggregation"`
}

type measurementComparisonEnvelope struct {
	SchemaVersion     int                         `json:"schema_version"`
	Comparisons       []measurementComparisonCase `json:"comparisons"`
	IndependentRetest *independentRetest          `json:"independent_retest,omitempty"`
}

type measurementBaselineEnvelope struct {
	SchemaVersion int                       `json:"schema_version"`
	Baseline      []measurementBaselineCase `json:"baseline"`
}

type measurementBaselineCase struct {
	CaseID string             `json:"case_id"`
	Values map[string]float64 `json:"values"`
}

type MeasurementSet struct {
	Cases []MeasurementSetCase `json:"cases"`
}

type MeasurementSetCase struct {
	CaseID string             `json:"case_id"`
	Weight float64            `json:"weight"`
	Values map[string]float64 `json:"values"`
}

type measurementComparisonCase struct {
	CaseID    string             `json:"case_id"`
	Reference map[string]float64 `json:"reference"`
	Candidate map[string]float64 `json:"candidate"`
}

type MeasurementComparison struct {
	PrimaryMetricID   string                              `json:"primary_metric_id"`
	Cases             []DerivedMeasurementCase            `json:"cases"`
	Metrics           map[string]DerivedMeasurementMetric `json:"metrics"`
	IndependentRetest bool                                `json:"independent_retest"`
}

type DerivedMeasurementCase struct {
	CaseID  string                             `json:"case_id"`
	Weight  float64                            `json:"weight"`
	Metrics map[string]DerivedMeasurementValue `json:"metrics"`
}

type DerivedMeasurementValue struct {
	Reference          float64 `json:"reference"`
	Candidate          float64 `json:"candidate"`
	Speedup            float64 `json:"speedup"`
	Regression         bool    `json:"regression"`
	RegressionFraction float64 `json:"regression_fraction"`
}

type DerivedMeasurementMetric struct {
	AggregateSpeedup float64 `json:"aggregate_speedup"`
	MaxCaseSpeedup   float64 `json:"max_case_speedup"`
	MaxCaseID        string  `json:"max_case_id"`
}

// ParseMeasurementDefinition parses and validates benchmark_measurements v1.
func ParseMeasurementDefinition(raw json.RawMessage) (MeasurementDefinition, error) {
	var definition MeasurementDefinition
	if err := decodeField(raw, "benchmark_measurements", &definition); err != nil {
		return MeasurementDefinition{}, err
	}
	if definition.SchemaVersion != MeasurementSchemaVersion {
		return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.schema_version must be %d", MeasurementSchemaVersion)
	}
	if len(definition.Cases) == 0 {
		return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.cases must be non-empty")
	}
	caseIDs := make(map[string]struct{}, len(definition.Cases))
	for index, item := range definition.Cases {
		if item.CaseID == "" || !finitePositive(item.Weight) {
			return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.cases[%d] requires a non-empty case_id and finite positive weight", index)
		}
		if _, exists := caseIDs[item.CaseID]; exists {
			return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.cases contains duplicate %q", item.CaseID)
		}
		caseIDs[item.CaseID] = struct{}{}
	}
	if len(definition.Metrics) == 0 {
		return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.metrics must be non-empty")
	}
	metricIDs := make(map[string]struct{}, len(definition.Metrics))
	primaryCount := 0
	for index, metric := range definition.Metrics {
		prefix := fmt.Sprintf("benchmark_measurements.metrics[%d]", index)
		if metric.ID == "" || metric.Label == "" || metric.Unit == "" || metric.SampleStatistic == "" {
			return MeasurementDefinition{}, fmt.Errorf("%s requires non-empty id, label, unit, and sample_statistic", prefix)
		}
		if _, exists := metricIDs[metric.ID]; exists {
			return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.metrics contains duplicate %q", metric.ID)
		}
		metricIDs[metric.ID] = struct{}{}
		switch metric.Role {
		case MetricRolePrimary:
			primaryCount++
			definition.PrimaryMetricID = metric.ID
		case MetricRoleGuard, MetricRoleInformational:
		default:
			return MeasurementDefinition{}, fmt.Errorf("%s.role is unsupported", prefix)
		}
		if metric.Direction != LowerIsBetter && metric.Direction != HigherIsBetter {
			return MeasurementDefinition{}, fmt.Errorf("%s.direction is unsupported", prefix)
		}
		if metric.Aggregation != WeightedGeomeanOfRatios && metric.Aggregation != RatioOfWeightedArithmeticMeans {
			return MeasurementDefinition{}, fmt.Errorf("%s.aggregation is unsupported", prefix)
		}
	}
	if primaryCount != 1 {
		return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.metrics must contain exactly one primary metric")
	}
	return definition, nil
}

// ParseFrozenMeasurementDefinition additionally proves that measurement Cases
// are exactly the benchmark_integrity Full Case Set.
func ParseFrozenMeasurementDefinition(raw json.RawMessage) (MeasurementDefinition, error) {
	definition, err := ParseMeasurementDefinition(raw)
	if err != nil {
		return MeasurementDefinition{}, err
	}
	integrity, err := parseDefinition(raw)
	if err != nil {
		return MeasurementDefinition{}, err
	}
	if len(integrity.CaseIDs) != len(definition.Cases) {
		return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.cases must match benchmark_integrity.case_ids")
	}
	wanted := make(map[string]struct{}, len(integrity.CaseIDs))
	for _, caseID := range integrity.CaseIDs {
		wanted[caseID] = struct{}{}
	}
	for _, item := range definition.Cases {
		if _, exists := wanted[item.CaseID]; !exists {
			return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements case %q is not in benchmark_integrity.case_ids", item.CaseID)
		}
	}
	return definition, nil
}

// ParseBaselineMeasurements validates the Development Baseline measurement
// set against the frozen cases and metrics.
func ParseBaselineMeasurements(definition MeasurementDefinition, raw json.RawMessage) (MeasurementSet, error) {
	var envelope measurementBaselineEnvelope
	if err := decodeField(raw, "benchmark_measurements", &envelope); err != nil {
		return MeasurementSet{}, err
	}
	if envelope.SchemaVersion != MeasurementSchemaVersion {
		return MeasurementSet{}, fmt.Errorf("benchmark_measurements.schema_version must be %d", MeasurementSchemaVersion)
	}
	if len(envelope.Baseline) != len(definition.Cases) {
		return MeasurementSet{}, fmt.Errorf("benchmark_measurements.baseline must cover exactly %d frozen cases", len(definition.Cases))
	}
	wantedCases := make(map[string]MeasurementCase, len(definition.Cases))
	for _, item := range definition.Cases {
		wantedCases[item.CaseID] = item
	}
	metricIDs := make(map[string]struct{}, len(definition.Metrics))
	for _, metric := range definition.Metrics {
		metricIDs[metric.ID] = struct{}{}
	}
	result := MeasurementSet{Cases: make([]MeasurementSetCase, 0, len(envelope.Baseline))}
	seen := make(map[string]struct{}, len(envelope.Baseline))
	for index, item := range envelope.Baseline {
		caseDefinition, exists := wantedCases[item.CaseID]
		if !exists {
			return MeasurementSet{}, fmt.Errorf("benchmark_measurements.baseline[%d].case_id %q is not frozen", index, item.CaseID)
		}
		if _, exists := seen[item.CaseID]; exists {
			return MeasurementSet{}, fmt.Errorf("benchmark_measurements.baseline contains duplicate %q", item.CaseID)
		}
		seen[item.CaseID] = struct{}{}
		if len(item.Values) != len(metricIDs) {
			return MeasurementSet{}, fmt.Errorf("benchmark_measurements.baseline[%d].values must contain every frozen metric exactly once", index)
		}
		for metricID := range metricIDs {
			value, exists := item.Values[metricID]
			if !exists || !finitePositive(value) {
				return MeasurementSet{}, fmt.Errorf("benchmark_measurements.baseline[%d].values.%s must be finite and greater than zero", index, metricID)
			}
		}
		result.Cases = append(result.Cases, MeasurementSetCase{CaseID: item.CaseID, Weight: caseDefinition.Weight, Values: item.Values})
	}
	return result, nil
}

// ParseMeasurementComparison validates paired per-Case results and derives all
// ratios. Numeric Agent claims are intentionally ignored.
func ParseMeasurementComparison(definition MeasurementDefinition, raw json.RawMessage) (MeasurementComparison, error) {
	var envelope measurementComparisonEnvelope
	if err := decodeField(raw, "benchmark_measurements", &envelope); err != nil {
		return MeasurementComparison{}, err
	}
	if envelope.SchemaVersion != MeasurementSchemaVersion {
		return MeasurementComparison{}, fmt.Errorf("benchmark_measurements.schema_version must be %d", MeasurementSchemaVersion)
	}
	if len(envelope.Comparisons) != len(definition.Cases) {
		return MeasurementComparison{}, fmt.Errorf("benchmark_measurements.comparisons must cover exactly %d frozen cases", len(definition.Cases))
	}
	wantedCases := make(map[string]MeasurementCase, len(definition.Cases))
	for _, item := range definition.Cases {
		wantedCases[item.CaseID] = item
	}
	metricDefinitions := make(map[string]MeasurementMetric, len(definition.Metrics))
	for _, metric := range definition.Metrics {
		metricDefinitions[metric.ID] = metric
	}
	result := MeasurementComparison{
		PrimaryMetricID:   definition.PrimaryMetricID,
		Cases:             make([]DerivedMeasurementCase, 0, len(envelope.Comparisons)),
		Metrics:           make(map[string]DerivedMeasurementMetric, len(definition.Metrics)),
		IndependentRetest: retestPassed(envelope.IndependentRetest),
	}
	seen := make(map[string]struct{}, len(envelope.Comparisons))
	for index, item := range envelope.Comparisons {
		caseDefinition, exists := wantedCases[item.CaseID]
		if !exists {
			return MeasurementComparison{}, fmt.Errorf("benchmark_measurements.comparisons[%d].case_id %q is not frozen", index, item.CaseID)
		}
		if _, exists := seen[item.CaseID]; exists {
			return MeasurementComparison{}, fmt.Errorf("benchmark_measurements.comparisons contains duplicate %q", item.CaseID)
		}
		seen[item.CaseID] = struct{}{}
		if len(item.Reference) != len(definition.Metrics) || len(item.Candidate) != len(definition.Metrics) {
			return MeasurementComparison{}, fmt.Errorf("benchmark_measurements.comparisons[%d] must contain every frozen metric exactly once", index)
		}
		derivedCase := DerivedMeasurementCase{CaseID: item.CaseID, Weight: caseDefinition.Weight, Metrics: make(map[string]DerivedMeasurementValue, len(definition.Metrics))}
		for metricID, metric := range metricDefinitions {
			reference, referenceExists := item.Reference[metricID]
			candidate, candidateExists := item.Candidate[metricID]
			if !referenceExists || !candidateExists || !finitePositive(reference) || !finitePositive(candidate) {
				return MeasurementComparison{}, fmt.Errorf("benchmark_measurements.comparisons[%d].%s requires finite positive reference and candidate values", index, metricID)
			}
			speedup := reference / candidate
			if metric.Direction == HigherIsBetter {
				speedup = candidate / reference
			}
			derivedCase.Metrics[metricID] = DerivedMeasurementValue{
				Reference: reference, Candidate: candidate, Speedup: speedup,
				Regression: speedup < 1, RegressionFraction: math.Max(0, 1-speedup),
			}
		}
		result.Cases = append(result.Cases, derivedCase)
	}
	for _, metric := range definition.Metrics {
		derived := aggregateMetric(metric, result.Cases)
		result.Metrics[metric.ID] = derived
		if math.Max(derived.AggregateSpeedup, derived.MaxCaseSpeedup) >= SuspiciousSpeedupThreshold && !result.IndependentRetest {
			return MeasurementComparison{}, fmt.Errorf("benchmark_measurements.independent_retest is required for computed speedups >= %.0fx", SuspiciousSpeedupThreshold)
		}
	}
	return result, nil
}

func aggregateMetric(metric MeasurementMetric, cases []DerivedMeasurementCase) DerivedMeasurementMetric {
	var weightSum, logarithmSum, referenceSum, candidateSum float64
	result := DerivedMeasurementMetric{}
	for _, item := range cases {
		value := item.Metrics[metric.ID]
		weightSum += item.Weight
		logarithmSum += item.Weight * math.Log(value.Speedup)
		referenceSum += item.Weight * value.Reference
		candidateSum += item.Weight * value.Candidate
		if value.Speedup > result.MaxCaseSpeedup {
			result.MaxCaseSpeedup = value.Speedup
			result.MaxCaseID = item.CaseID
		}
	}
	if metric.Aggregation == WeightedGeomeanOfRatios {
		result.AggregateSpeedup = math.Exp(logarithmSum / weightSum)
	} else if metric.Direction == LowerIsBetter {
		result.AggregateSpeedup = referenceSum / candidateSum
	} else {
		result.AggregateSpeedup = candidateSum / referenceSum
	}
	return result
}

func retestPassed(retest *independentRetest) bool {
	if retest == nil {
		return false
	}
	checks := []*bool{retest.Passed, retest.ChangedCanonicalInputs, retest.OutputSentinel, retest.ColdStartReported, retest.SetupReported, retest.SteadyStateReported, retest.EndToEndReported, retest.TimingBoundaryFair}
	for _, check := range checks {
		if check == nil || !*check {
			return false
		}
	}
	return true
}
