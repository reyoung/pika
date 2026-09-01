package kdacontract

import (
	"encoding/json"
	"testing"
)

func TestDiagnosisV1CanonicalFixtureAndStrictNegatives(t *testing.T) {
	valid := []byte(`{"idempotency_key":"diagnosis-1","outcome":"ready","report":{"schema_version":1,"subject":{"baseline_revision_id":"baseline","best_sha":"sha","hardware":{},"software":{}},"coverage":{"case_ids":["case-a"],"dispatch_paths":["kernel"]},"artifacts":[{"path":"report.json","kind":"profile"}],"observations":[{"id":"o","case_ids":["case-a"],"source_artifacts":["report.json"],"summary":"measured","metric":"time","value":1.5,"unit":"us"}],"bottlenecks":[{"id":"b","class":"tail","confidence":"high","observation_ids":["o"],"summary":"tail"}],"hypotheses":[{"id":"h","rank":1,"summary":"fuse","bottleneck_ids":["b"],"target_case_ids":["case-a"],"mechanism":"remove work","expected_effect":"lower","risk":"none","knowledge_refs":["KernelWiki/x"]}],"limitations":[]}}`)
	if err := ValidateDiagnosisInput(valid); err != nil {
		t.Fatalf("canonical Diagnosis fixture: %v", err)
	}
	for _, path := range [][]string{{"report", "coverage", "dispatch_paths"}, {"report", "observations", "metric"}, {"report", "observations", "value"}, {"report", "observations", "unit"}, {"report", "bottlenecks", "class"}, {"report", "hypotheses", "mechanism"}, {"report", "hypotheses", "expected_effect"}, {"report", "hypotheses", "risk"}, {"report", "hypotheses", "knowledge_refs"}} {
		var document map[string]any
		_ = json.Unmarshal(valid, &document)
		container := document[path[0]].(map[string]any)
		if values, ok := container[path[1]].([]any); ok {
			delete(values[0].(map[string]any), path[2])
		} else {
			delete(container[path[1]].(map[string]any), path[2])
		}
		encoded, _ := json.Marshal(document)
		if err := ValidateDiagnosisInput(encoded); err == nil {
			t.Fatalf("accepted Diagnosis missing required field %v", path)
		}
	}
	for _, replacement := range []string{`"summary":""`, `"case_ids":["case-a","case-a"]`, `"rank":0`} {
		invalid := []byte(replaceOnce(string(valid), replacement))
		if err := ValidateDiagnosisInput(invalid); err == nil {
			t.Fatalf("accepted invalid Diagnosis %s", replacement)
		}
	}
	var unknown map[string]any
	_ = json.Unmarshal(valid, &unknown)
	unknown["unexpected"] = true
	encoded, _ := json.Marshal(unknown)
	if err := ValidateDiagnosisInput(encoded); err == nil {
		t.Fatal("accepted unknown Diagnosis field")
	}
	if err := ValidateDiagnosisInput([]byte(stringReplace(string(valid), `"outcome":"ready"`, `"outcome":"unavailable"`))); err == nil {
		t.Fatal("accepted unavailable Diagnosis without a limitation")
	}
	if err := ValidateDiagnosisInput([]byte(stringReplace(string(valid), `"hardware":{}`, `"hardware":{"gpu":{"nested":"not-a-scalar"}}`))); err == nil {
		t.Fatal("accepted non-scalar hardware profile value")
	}
	if err := ValidateDiagnosisInput([]byte(stringReplace(string(valid), `"hardware":{},`, ``))); err == nil {
		t.Fatal("accepted Diagnosis subject without hardware")
	}
	if err := ValidateDiagnosisInput([]byte(stringReplace(string(valid), `,"software":{}`, ``))); err == nil {
		t.Fatal("accepted Diagnosis subject without software")
	}
	if err := ValidateDiagnosisReport([]byte(`{"schema_version":1,"subject":{"baseline_revision_id":"baseline","best_sha":"sha"},"coverage":{"case_ids":["case-a"]},"artifacts":[],"observations":[],"bottlenecks":[],"hypotheses":[],"limitations":[{"attempted_command":"ncu","failure_category":"missing","error_text":"unavailable"}]}`)); err == nil {
		t.Fatal("accepted Diagnosis report subject without environment objects")
	}
	var duplicatedArtifacts map[string]any
	_ = json.Unmarshal(valid, &duplicatedArtifacts)
	duplicatedArtifacts["artifacts"] = []any{map[string]any{"path": "report.json", "kind": "profile"}}
	encoded, _ = json.Marshal(duplicatedArtifacts)
	if err := ValidateDiagnosisInput(encoded); err == nil {
		t.Fatal("accepted duplicate top-level Diagnosis artifacts")
	}
}

func TestExperimentV1CanonicalFixtureAndOutcomeNegatives(t *testing.T) {
	valid := []byte(`{"idempotency_key":"experiment-1","experiment":{"schema_version":1,"parent_checkpoint_sha":"parent","hypothesis":{"diagnosis_hypothesis_id":"hypothesis-1","summary":"claim"},"change":{"summary":"change","paths":["candidate.py"],"mechanism":"remove work"},"outcome":"kept","checkpoint_sha":"checkpoint","correctness":{"benchmark_integrity":{"schema_version":1,"cases":[{"case_id":"case-a","warmup_invocations":0,"measured_invocations":1,"input_restores":1,"checked_invocations":1,"mismatches":0,"nonfinite":0,"canonical_input_mutations":0,"tolerance_passed":true}]}},"benchmark_measurements":{"schema_version":1,"comparisons":[{"case_id":"case-a","reference":{"latency":2},"candidate":{"latency":1}}]},"artifacts":[{"path":"experiments/evidence.json","kind":"benchmark"}],"summary":"kept"}}`)
	if err := ValidateExperimentInput(valid); err != nil {
		t.Fatalf("canonical Experiment fixture: %v", err)
	}
	var envelope struct {
		Experiment json.RawMessage `json:"experiment"`
	}
	if err := json.Unmarshal(valid, &envelope); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExperiment(envelope.Experiment)
	if err != nil || parsed.Hypothesis.DiagnosisHypothesisID != "hypothesis-1" || parsed.Correctness == nil || len(parsed.Artifacts) != 1 {
		t.Fatalf("canonical Experiment DTO = %+v, %v", parsed, err)
	}
	rejected, err := json.Marshal(Experiment{
		SchemaVersion: 1, ParentCheckpointSHA: "parent", Hypothesis: ExperimentHypothesis{Summary: "negative"},
		Change:  ExperimentChange{Summary: "change", Paths: []string{"candidate.py"}, Mechanism: "try"},
		Outcome: "rejected", Artifacts: []ArtifactRef{{Path: "experiments/evidence.json", Kind: "benchmark"}}, Summary: "no gain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseExperiment(rejected); err != nil || parsed.Correctness != nil || parsed.CheckpointSHA != "" {
		t.Fatalf("negative Experiment DTO = %+v, %v; JSON=%s", parsed, err, rejected)
	}
	for _, replacement := range [][2]string{{`"reference":{"latency":2}`, `"reference":{"latency":0}`}, {`"paths":["candidate.py"]`, `"paths":["candidate.py","candidate.py"]`}, {`"outcome":"kept"`, `"outcome":"rejected"`}} {
		if err := ValidateExperimentInput([]byte(stringReplace(string(valid), replacement[0], replacement[1]))); err == nil {
			t.Fatalf("accepted invalid Experiment %q", replacement[1])
		}
	}
	var unknown map[string]any
	_ = json.Unmarshal(valid, &unknown)
	unknown["unexpected"] = true
	encoded, _ := json.Marshal(unknown)
	if err := ValidateExperimentInput(encoded); err == nil {
		t.Fatal("accepted unknown Experiment input field")
	}
	if err := ValidateExperimentInput([]byte(stringReplace(string(valid), `"summary":"change"`, `"summary":""`))); err == nil {
		t.Fatal("accepted empty Experiment change summary")
	}
	if err := ValidateExperimentInput([]byte(stringReplace(string(valid), `"hypothesis":{"diagnosis_hypothesis_id":"hypothesis-1","summary":"claim"}`, `"hypothesis":{}`))); err == nil {
		t.Fatal("accepted Experiment hypothesis without identity or summary")
	}
	var missingNestedArtifacts map[string]any
	_ = json.Unmarshal(valid, &missingNestedArtifacts)
	missingNestedArtifacts["experiment"].(map[string]any)["artifacts"] = []any{}
	encoded, _ = json.Marshal(missingNestedArtifacts)
	if err := ValidateExperimentInput(encoded); err == nil {
		t.Fatal("accepted Experiment without an evidence artifact")
	}
	var duplicatedArtifacts map[string]any
	_ = json.Unmarshal(valid, &duplicatedArtifacts)
	duplicatedArtifacts["artifacts"] = []any{map[string]any{"path": "experiments/evidence.json", "kind": "benchmark"}}
	encoded, _ = json.Marshal(duplicatedArtifacts)
	if err := ValidateExperimentInput(encoded); err == nil {
		t.Fatal("accepted duplicate top-level Experiment artifacts")
	}
}

func replaceOnce(valid string, replacement string) string {
	if replacement == `"summary":""` {
		return stringReplace(valid, `"summary":"measured"`, replacement)
	}
	if replacement == `"case_ids":["case-a","case-a"]` {
		return stringReplace(valid, `"case_ids":["case-a"]`, replacement)
	}
	return stringReplace(valid, `"rank":1`, replacement)
}
func stringReplace(value, old, replacement string) string {
	for i := 0; i+len(old) <= len(value); i++ {
		if value[i:i+len(old)] == old {
			return value[:i] + replacement + value[i+len(old):]
		}
	}
	return value
}
