// Package kdacontract owns the versioned wire contracts exchanged through KDA
// MCP.  Keeping these schemas outside the MCP adapter prevents a provider
// prompt from becoming the only source of protocol truth.
package kdacontract

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func strict(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": append([]string{}, required...), "additionalProperties": false}
}
func str() map[string]any { return map[string]any{"type": "string", "minLength": 1} }
func artifact() map[string]any {
	return strict(map[string]any{"path": str(), "kind": str()}, "path", "kind")
}

// EnvironmentProfileSchema is the contract-declared extension point for
// hardware and software facts. contracts.md intentionally does not freeze
// vendor field names, so keys are non-empty strings while values have a
// finite scalar shape rather than an unbounded JSON object.
func EnvironmentProfileSchema() map[string]any {
	return map[string]any{"type": "object", "propertyNames": map[string]any{"type": "string", "minLength": 1}, "additionalProperties": map[string]any{"type": []string{"string", "number", "boolean", "null"}}}
}

type DiagnosisReport struct {
	SchemaVersion int64 `json:"schema_version"`
	Subject       struct {
		BaselineRevisionID string         `json:"baseline_revision_id"`
		BestSHA            string         `json:"best_sha"`
		Hardware           map[string]any `json:"hardware"`
		Software           map[string]any `json:"software"`
	} `json:"subject"`
	Coverage struct {
		CaseIDs       []string `json:"case_ids"`
		DispatchPaths []string `json:"dispatch_paths"`
	} `json:"coverage"`
	Artifacts    []ArtifactRef          `json:"artifacts"`
	Observations []DiagnosisObservation `json:"observations"`
	Bottlenecks  []DiagnosisBottleneck  `json:"bottlenecks"`
	Hypotheses   []DiagnosisHypothesis  `json:"hypotheses"`
	Limitations  []DiagnosisLimitation  `json:"limitations"`
}

// ArtifactRef is the shared path-and-kind identity used by KDA reports.
type ArtifactRef struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type DiagnosisObservation struct {
	ID              string   `json:"id"`
	CaseIDs         []string `json:"case_ids"`
	Metric          string   `json:"metric"`
	Value           float64  `json:"value"`
	Unit            string   `json:"unit"`
	SourceArtifacts []string `json:"source_artifacts"`
	Summary         string   `json:"summary"`
}

type DiagnosisBottleneck struct {
	ID             string   `json:"id"`
	Class          string   `json:"class"`
	Confidence     string   `json:"confidence"`
	ObservationIDs []string `json:"observation_ids"`
	Summary        string   `json:"summary"`
}

type DiagnosisHypothesis struct {
	ID             string   `json:"id"`
	Rank           int64    `json:"rank"`
	Summary        string   `json:"summary"`
	Mechanism      string   `json:"mechanism"`
	TargetCaseIDs  []string `json:"target_case_ids"`
	ExpectedEffect string   `json:"expected_effect"`
	Risk           string   `json:"risk"`
	BottleneckIDs  []string `json:"bottleneck_ids"`
	KnowledgeRefs  []string `json:"knowledge_refs"`
}

type DiagnosisLimitation struct {
	AttemptedCommand string `json:"attempted_command"`
	FailureCategory  string `json:"failure_category"`
	ErrorText        string `json:"error_text"`
	ExitStatus       *int64 `json:"exit_status,omitempty"`
}

// Experiment is the canonical v1 artifact contract carried inside a
// record_iteration_experiment request. Keeping the DTO beside its JSON Schema
// prevents runtime and test producers from maintaining handwritten shadows of
// the wire contract.
type Experiment struct {
	SchemaVersion         int64                  `json:"schema_version"`
	ParentCheckpointSHA   string                 `json:"parent_checkpoint_sha"`
	Hypothesis            ExperimentHypothesis   `json:"hypothesis"`
	Change                ExperimentChange       `json:"change"`
	Outcome               string                 `json:"outcome"`
	CheckpointSHA         string                 `json:"checkpoint_sha,omitempty"`
	Correctness           *ExperimentCorrectness `json:"correctness,omitempty"`
	BenchmarkMeasurements json.RawMessage        `json:"benchmark_measurements,omitempty"`
	Artifacts             []ArtifactRef          `json:"artifacts"`
	Summary               string                 `json:"summary"`
}

type ExperimentHypothesis struct {
	DiagnosisHypothesisID string `json:"diagnosis_hypothesis_id,omitempty"`
	Summary               string `json:"summary,omitempty"`
}

type ExperimentChange struct {
	Summary   string   `json:"summary"`
	Paths     []string `json:"paths"`
	Mechanism string   `json:"mechanism"`
}

type ExperimentCorrectness struct {
	BenchmarkIntegrity json.RawMessage `json:"benchmark_integrity,omitempty"`
}

func ParseDiagnosisReport(raw json.RawMessage) (DiagnosisReport, error) {
	var report DiagnosisReport
	if err := ValidateDiagnosisReport(raw); err != nil {
		return report, err
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return report, fmt.Errorf("decode Diagnosis report: %w", err)
	}
	return report, nil
}

func ParseExperiment(raw json.RawMessage) (Experiment, error) {
	var experiment Experiment
	if err := ValidateExperiment(raw); err != nil {
		return experiment, err
	}
	if err := json.Unmarshal(raw, &experiment); err != nil {
		return experiment, fmt.Errorf("decode Experiment contract: %w", err)
	}
	return experiment, nil
}

// DiagnosisReportSchema is the canonical v1 artifact contract persisted by a
// successful finish_diagnosis call.  Context materialization consumes this
// same schema rather than trusting a separately maintained summary field.
func DiagnosisReportSchema() map[string]any {
	limitation := strict(map[string]any{"attempted_command": str(), "failure_category": str(), "error_text": str(), "exit_status": map[string]any{"type": "integer"}}, "attempted_command", "failure_category", "error_text")
	observation := strict(map[string]any{"id": str(), "case_ids": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": str()}, "source_artifacts": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": str()}, "summary": str(), "metric": str(), "value": map[string]any{"type": "number"}, "unit": str()}, "id", "case_ids", "source_artifacts", "summary", "metric", "value", "unit")
	bottleneck := strict(map[string]any{"id": str(), "confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}}, "observation_ids": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": str()}, "summary": str(), "class": str()}, "id", "confidence", "observation_ids", "summary", "class")
	hypothesis := strict(map[string]any{"id": str(), "rank": map[string]any{"type": "integer", "minimum": 1}, "summary": str(), "bottleneck_ids": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": str()}, "target_case_ids": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": str()}, "mechanism": str(), "expected_effect": str(), "risk": str(), "knowledge_refs": map[string]any{"type": "array", "uniqueItems": true, "items": str()}}, "id", "rank", "summary", "bottleneck_ids", "target_case_ids", "mechanism", "expected_effect", "risk", "knowledge_refs")
	report := strict(map[string]any{
		"schema_version": map[string]any{"type": "integer", "const": 1},
		"subject": strict(map[string]any{
			"baseline_revision_id": str(), "best_sha": str(),
			"hardware": EnvironmentProfileSchema(),
			"software": EnvironmentProfileSchema(),
		}, "baseline_revision_id", "best_sha", "hardware", "software"),
		"coverage":     strict(map[string]any{"case_ids": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": str()}, "dispatch_paths": map[string]any{"type": "array", "uniqueItems": true, "items": str()}}, "case_ids", "dispatch_paths"),
		"artifacts":    map[string]any{"type": "array", "items": artifact()},
		"observations": map[string]any{"type": "array", "items": observation},
		"bottlenecks":  map[string]any{"type": "array", "items": bottleneck},
		"hypotheses":   map[string]any{"type": "array", "items": hypothesis},
		"limitations":  map[string]any{"type": "array", "items": limitation},
	}, "schema_version", "subject", "coverage", "artifacts", "observations", "bottlenecks", "hypotheses", "limitations")
	return report
}

// DiagnosisInputSchema is the complete v1 MCP contract for finish_diagnosis.
func DiagnosisInputSchema() map[string]any {
	report := DiagnosisReportSchema()
	input := strict(map[string]any{"idempotency_key": str(), "outcome": map[string]any{"type": "string", "enum": []string{"ready", "unavailable"}}, "report": report}, "idempotency_key", "outcome", "report")
	readyOutcome := map[string]any{
		"if": map[string]any{"properties": map[string]any{"outcome": map[string]any{"const": "ready"}}},
		"then": map[string]any{"properties": map[string]any{"report": map[string]any{"properties": map[string]any{
			"observations": map[string]any{"minItems": 1},
			"bottlenecks":  map[string]any{"minItems": 1},
			"hypotheses":   map[string]any{"minItems": 1},
		}}}},
		"else": map[string]any{"properties": map[string]any{"report": map[string]any{"properties": map[string]any{
			"limitations": map[string]any{"minItems": 1},
		}}}},
	}
	input["allOf"] = []any{readyOutcome}
	return input
}

// ExperimentSchema is the canonical v1 artifact contract for one experiment.
func ExperimentSchema() map[string]any {
	hypothesis := strict(map[string]any{"diagnosis_hypothesis_id": str(), "summary": str()})
	hypothesis["anyOf"] = []any{map[string]any{"required": []string{"diagnosis_hypothesis_id"}}, map[string]any{"required": []string{"summary"}}}
	change := strict(map[string]any{"summary": str(), "paths": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": str()}, "mechanism": str()}, "summary", "paths", "mechanism")
	correctness := strict(map[string]any{"benchmark_integrity": benchmarkintegrity.StrictEvidenceSchema()}, "benchmark_integrity")
	experiment := strict(map[string]any{
		"schema_version":         map[string]any{"type": "integer", "const": 1},
		"parent_checkpoint_sha":  str(),
		"hypothesis":             hypothesis,
		"change":                 change,
		"outcome":                map[string]any{"type": "string", "enum": []string{"kept", "rejected", "inconclusive"}},
		"checkpoint_sha":         str(),
		"correctness":            correctness,
		"benchmark_measurements": benchmarkintegrity.StrictMeasurementComparisonSchema(),
		"artifacts":              map[string]any{"type": "array", "minItems": 1, "items": artifact()},
		"summary":                str(),
	}, "schema_version", "parent_checkpoint_sha", "hypothesis", "change", "outcome", "artifacts", "summary")
	experiment["allOf"] = []any{map[string]any{
		"if":   map[string]any{"properties": map[string]any{"outcome": map[string]any{"const": "kept"}}, "required": []string{"outcome"}},
		"then": map[string]any{"required": []string{"checkpoint_sha", "correctness", "benchmark_measurements"}},
		"else": map[string]any{"not": map[string]any{"required": []string{"checkpoint_sha"}}},
	}}
	return experiment
}

// ExperimentInputSchema is the complete v1 MCP request contract.
func ExperimentInputSchema() map[string]any {
	return strict(map[string]any{"idempotency_key": str(), "experiment": ExperimentSchema()}, "idempotency_key", "experiment")
}

// ValidateDiagnosisInput and ValidateExperimentInput use the same strict
// property/required/enum contract exported to MCP. Runtime-specific identity,
// Git and artifact postconditions remain in the domain layer.
func ValidateDiagnosisInput(raw json.RawMessage) error { return validate(raw, DiagnosisInputSchema()) }
func ValidateDiagnosisReport(raw json.RawMessage) error {
	return validate(raw, DiagnosisReportSchema())
}
func ValidateExperiment(raw json.RawMessage) error {
	return validate(raw, ExperimentSchema())
}
func ValidateExperimentInput(raw json.RawMessage) error {
	return validate(raw, ExperimentInputSchema())
}

func validate(raw []byte, schema map[string]any) error {
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	encoded, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	schemaValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	if err := compiler.AddResource("kda.json", schemaValue); err != nil {
		return err
	}
	compiled, err := compiler.Compile("kda.json")
	if err != nil {
		return err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := compiled.Validate(value); err != nil {
		return fmt.Errorf("invalid JSON Schema contract: %w", err)
	}
	return nil
}
