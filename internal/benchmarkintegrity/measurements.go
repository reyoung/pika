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
	IterationGate   IterationGate       `json:"iteration_performance_gate"`
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

type IterationGate struct {
	SchemaVersion int                   `json:"schema_version"`
	Metrics       []IterationGateMetric `json:"metrics"`
}

type IterationGateMetric struct {
	MetricID                      string  `json:"metric_id"`
	MinimumAggregateSpeedup       float64 `json:"minimum_aggregate_speedup"`
	MaximumCaseRegressionFraction float64 `json:"maximum_case_regression_fraction"`
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
	SchemaVersion     int                  `json:"schema_version,omitempty"`
	Cases             []MeasurementSetCase `json:"cases"`
	IndependentRetest bool                 `json:"independent_retest,omitempty"`
}

type measurementSetEnvelope struct {
	SchemaVersion     int                       `json:"schema_version"`
	Cases             []measurementBaselineCase `json:"cases"`
	IndependentRetest *independentRetest        `json:"independent_retest,omitempty"`
}

// ParseMeasurementSet validates one unpaired reference or candidate set.
func ParseMeasurementSet(definition MeasurementDefinition, raw json.RawMessage) (MeasurementSet, error) {
	var envelope measurementSetEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return MeasurementSet{}, fmt.Errorf("decode benchmark measurement set: %w", err)
	}
	if envelope.SchemaVersion != MeasurementSchemaVersion {
		return MeasurementSet{}, fmt.Errorf("benchmark measurement set schema_version must be %d", MeasurementSchemaVersion)
	}
	if len(envelope.Cases) != len(definition.Cases) {
		return MeasurementSet{}, fmt.Errorf("benchmark measurement set must cover exactly %d frozen cases", len(definition.Cases))
	}
	wantedCases := make(map[string]MeasurementCase, len(definition.Cases))
	for _, item := range definition.Cases {
		wantedCases[item.CaseID] = item
	}
	metricIDs := make(map[string]struct{}, len(definition.Metrics))
	for _, metric := range definition.Metrics {
		metricIDs[metric.ID] = struct{}{}
	}
	result := MeasurementSet{SchemaVersion: envelope.SchemaVersion, IndependentRetest: retestPassed(envelope.IndependentRetest), Cases: make([]MeasurementSetCase, 0, len(envelope.Cases))}
	seen := make(map[string]struct{}, len(envelope.Cases))
	for index, item := range envelope.Cases {
		caseDefinition, exists := wantedCases[item.CaseID]
		if !exists {
			return MeasurementSet{}, fmt.Errorf("benchmark measurement set cases[%d].case_id %q is not frozen", index, item.CaseID)
		}
		if _, duplicate := seen[item.CaseID]; duplicate {
			return MeasurementSet{}, fmt.Errorf("benchmark measurement set contains duplicate %q", item.CaseID)
		}
		seen[item.CaseID] = struct{}{}
		if len(item.Values) != len(metricIDs) {
			return MeasurementSet{}, fmt.Errorf("benchmark measurement set cases[%d].values must contain every frozen metric exactly once", index)
		}
		for metricID := range metricIDs {
			value, present := item.Values[metricID]
			if !present || !finitePositive(value) {
				return MeasurementSet{}, fmt.Errorf("benchmark measurement set cases[%d].values.%s must be finite and greater than zero", index, metricID)
			}
		}
		result.Cases = append(result.Cases, MeasurementSetCase{CaseID: item.CaseID, Weight: caseDefinition.Weight, Values: item.Values})
	}
	return result, nil
}

// CompareMeasurementSets derives the authoritative comparison from separately
// recorded reference and candidate values. Neither Agent supplies speedups.
func CompareMeasurementSets(definition MeasurementDefinition, reference, candidate MeasurementSet) (MeasurementComparison, error) {
	if len(reference.Cases) != len(definition.Cases) || len(candidate.Cases) != len(definition.Cases) {
		return MeasurementComparison{}, fmt.Errorf("reference and candidate measurement sets must cover the frozen cases")
	}
	referenceByCase := make(map[string]MeasurementSetCase, len(reference.Cases))
	for _, item := range reference.Cases {
		referenceByCase[item.CaseID] = item
	}
	comparison := MeasurementComparison{
		PrimaryMetricID: definition.PrimaryMetricID, Cases: make([]DerivedMeasurementCase, 0, len(candidate.Cases)),
		Metrics: make(map[string]DerivedMeasurementMetric, len(definition.Metrics)), IndependentRetest: candidate.IndependentRetest,
	}
	for _, candidateCase := range candidate.Cases {
		referenceCase, found := referenceByCase[candidateCase.CaseID]
		if !found {
			return MeasurementComparison{}, fmt.Errorf("candidate case %q has no reference measurement", candidateCase.CaseID)
		}
		derived := DerivedMeasurementCase{CaseID: candidateCase.CaseID, Weight: candidateCase.Weight, Metrics: make(map[string]DerivedMeasurementValue, len(definition.Metrics))}
		for _, metric := range definition.Metrics {
			referenceValue, referenceFound := referenceCase.Values[metric.ID]
			candidateValue, candidateFound := candidateCase.Values[metric.ID]
			if !referenceFound || !candidateFound || !finitePositive(referenceValue) || !finitePositive(candidateValue) {
				return MeasurementComparison{}, fmt.Errorf("case %q metric %q requires finite reference and candidate values", candidateCase.CaseID, metric.ID)
			}
			speedup := referenceValue / candidateValue
			if metric.Direction == HigherIsBetter {
				speedup = candidateValue / referenceValue
			}
			derived.Metrics[metric.ID] = DerivedMeasurementValue{Reference: referenceValue, Candidate: candidateValue, Speedup: speedup, Regression: speedup < 1, RegressionFraction: math.Max(0, 1-speedup)}
		}
		comparison.Cases = append(comparison.Cases, derived)
	}
	for _, metric := range definition.Metrics {
		value := aggregateMetric(metric, comparison.Cases)
		comparison.Metrics[metric.ID] = value
		if math.Max(value.AggregateSpeedup, value.MaxCaseSpeedup) >= SuspiciousSpeedupThreshold && !comparison.IndependentRetest {
			return MeasurementComparison{}, fmt.Errorf("benchmark_measurements.independent_retest is required for computed speedups >= %.0fx", SuspiciousSpeedupThreshold)
		}
	}
	return comparison, nil
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
	metricByID := make(map[string]MeasurementMetric, len(definition.Metrics))
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
		metricByID[metric.ID] = metric
	}
	if primaryCount != 1 {
		return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.metrics must contain exactly one primary metric")
	}
	if len(definition.IterationGate.Metrics) == 0 && definition.IterationGate.SchemaVersion == 0 {
		return definition, nil
	}
	if definition.IterationGate.SchemaVersion != MeasurementSchemaVersion {
		return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.iteration_performance_gate.schema_version must be %d", MeasurementSchemaVersion)
	}
	if len(definition.IterationGate.Metrics) == 0 {
		return MeasurementDefinition{}, fmt.Errorf("benchmark_measurements.iteration_performance_gate.metrics must be non-empty")
	}
	return definition, validateIterationGateDefinition(definition, metricByID)
}

func validateIterationGateDefinition(definition MeasurementDefinition, metricByID map[string]MeasurementMetric) error {
	requiredMetricCount := 0
	for _, metric := range definition.Metrics {
		if metric.Role == MetricRolePrimary || metric.Role == MetricRoleGuard {
			requiredMetricCount++
		}
	}
	if len(definition.IterationGate.Metrics) != requiredMetricCount {
		return fmt.Errorf("benchmark_measurements.iteration_performance_gate.metrics must include exactly one entry for each primary or guard metric")
	}
	seenGateMetrics := map[string]struct{}{}
	for index, gate := range definition.IterationGate.Metrics {
		prefix := fmt.Sprintf("benchmark_measurements.iteration_performance_gate.metrics[%d]", index)
		if gate.MetricID == "" {
			return fmt.Errorf("%s.metric_id is required", prefix)
		}
		if _, duplicate := seenGateMetrics[gate.MetricID]; duplicate {
			return fmt.Errorf("benchmark_measurements.iteration_performance_gate.metrics contains duplicate %q", gate.MetricID)
		}
		seenGateMetrics[gate.MetricID] = struct{}{}
		metric, exists := metricByID[gate.MetricID]
		if !exists {
			return fmt.Errorf("%s.metric_id %q is not declared in benchmark_measurements.metrics", prefix, gate.MetricID)
		}
		if metric.Role == MetricRoleInformational {
			return fmt.Errorf("%s.metric_id %q cannot target an informational metric", prefix, gate.MetricID)
		}
		if !finitePositive(gate.MinimumAggregateSpeedup) {
			return fmt.Errorf("%s.minimum_aggregate_speedup must be finite and greater than zero", prefix)
		}
		if metric.Role == MetricRolePrimary && gate.MinimumAggregateSpeedup <= 1 {
			return fmt.Errorf("%s.minimum_aggregate_speedup must be greater than 1 for primary metrics", prefix)
		}
		if !finite(gate.MaximumCaseRegressionFraction) || gate.MaximumCaseRegressionFraction < 0 || gate.MaximumCaseRegressionFraction >= 1 {
			return fmt.Errorf("%s.maximum_case_regression_fraction must be in [0, 1)", prefix)
		}
	}
	for _, metric := range definition.Metrics {
		if metric.Role == MetricRoleInformational {
			continue
		}
		if _, exists := seenGateMetrics[metric.ID]; !exists {
			return fmt.Errorf("benchmark_measurements.iteration_performance_gate.metrics is missing %q", metric.ID)
		}
	}
	return nil
}

func RequireIterationGate(definition MeasurementDefinition) error {
	if len(definition.IterationGate.Metrics) == 0 {
		return fmt.Errorf("benchmark_measurements.iteration_performance_gate is required")
	}
	metricByID := make(map[string]MeasurementMetric, len(definition.Metrics))
	for _, metric := range definition.Metrics {
		metricByID[metric.ID] = metric
	}
	return validateIterationGateDefinition(definition, metricByID)
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

func FrozenCaseSubset(definition MeasurementDefinition, caseIDs []string) (MeasurementDefinition, error) {
	if len(caseIDs) == 0 {
		return MeasurementDefinition{}, fmt.Errorf("frozen Iteration Case subset must be non-empty")
	}
	byID := make(map[string]MeasurementCase, len(definition.Cases))
	for _, item := range definition.Cases {
		byID[item.CaseID] = item
	}
	seen := map[string]struct{}{}
	subset := make([]MeasurementCase, 0, len(caseIDs))
	for index, caseID := range caseIDs {
		if caseID == "" {
			return MeasurementDefinition{}, fmt.Errorf("frozen Iteration Case subset contains an empty case_id at index %d", index)
		}
		if _, duplicate := seen[caseID]; duplicate {
			return MeasurementDefinition{}, fmt.Errorf("frozen Iteration Case subset contains duplicate %q", caseID)
		}
		seen[caseID] = struct{}{}
		item, exists := byID[caseID]
		if !exists {
			return MeasurementDefinition{}, fmt.Errorf("frozen Iteration Case %q is not in benchmark_measurements.cases", caseID)
		}
		subset = append(subset, item)
	}
	filtered := definition
	filtered.Cases = subset
	return filtered, nil
}

func ValidateIterationGate(definition MeasurementDefinition, comparison MeasurementComparison) error {
	gateByMetric := make(map[string]IterationGateMetric, len(definition.IterationGate.Metrics))
	for _, gate := range definition.IterationGate.Metrics {
		gateByMetric[gate.MetricID] = gate
	}
	for _, metric := range definition.Metrics {
		if metric.Role == MetricRoleInformational {
			continue
		}
		gate, exists := gateByMetric[metric.ID]
		if !exists {
			return fmt.Errorf("iteration_performance_gate is missing %q", metric.ID)
		}
		aggregate, exists := comparison.Metrics[metric.ID]
		if !exists {
			return fmt.Errorf("derived comparison is missing metric %q", metric.ID)
		}
		if aggregate.AggregateSpeedup < gate.MinimumAggregateSpeedup {
			return fmt.Errorf("metric %q aggregate speedup %.6g is below minimum %.6g", metric.ID, aggregate.AggregateSpeedup, gate.MinimumAggregateSpeedup)
		}
		for _, item := range comparison.Cases {
			value, exists := item.Metrics[metric.ID]
			if !exists {
				return fmt.Errorf("derived comparison case %q is missing metric %q", item.CaseID, metric.ID)
			}
			if value.RegressionFraction > gate.MaximumCaseRegressionFraction {
				return fmt.Errorf("metric %q case %q regression fraction %.6g exceeds maximum %.6g", metric.ID, item.CaseID, value.RegressionFraction, gate.MaximumCaseRegressionFraction)
			}
		}
	}
	return nil
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

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
