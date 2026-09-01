package symphony

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestBuildKnowledgeProjectionPromotionSurvivesSliceGrowth(t *testing.T) {
	t.Parallel()
	report := json.RawMessage(`{"schema_version":1,"subject":{"baseline_revision_id":"baseline","best_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","hardware":{},"software":{}},"coverage":{"case_ids":["case-1"],"dispatch_paths":["kernel"]},"artifacts":[{"path":"diag.json","kind":"profile"}],"observations":[{"id":"o1","case_ids":["case-1"],"metric":"latency","value":1,"unit":"ms","source_artifacts":["diag.json"],"summary":"obs"}],"bottlenecks":[{"id":"b1","class":"launch","confidence":"high","observation_ids":["o1"],"summary":"b"}],"hypotheses":[{"id":"h1","rank":1,"bottleneck_ids":["b1"],"target_case_ids":["case-1"],"summary":"h","mechanism":"fuse","expected_effect":"lower latency","risk":"correctness","knowledge_refs":[]}],"limitations":[]}`)
	work := RuntimeWork{BestSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Diagnosis: &DiagnosisView{ID: "diagnosis", WorkID: "diagnosis-work", Report: report}}
	experiments := make([]IterationExperimentView, 0, 256)
	for i := 0; i < 128; i++ {
		payload := fmt.Sprintf(`{"summary":"exp-%d","hypothesis":{"diagnosis_hypothesis_id":"h1"}}`, i)
		experiments = append(experiments, IterationExperimentView{
			ID:           fmt.Sprintf("exp-%d", i),
			Outcome:      "kept",
			ScopeBestSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Experiment:   json.RawMessage(payload),
		})
	}
	view := View{Integrations: []IntegrationView{{ID: "integration-1", Status: "accepted", CandidateExperimentID: "exp-127"}}}
	records, err := BuildKnowledgeProjection(work, view, experiments, []EvidenceArtifact{{ID: "artifact-diag", WorkID: "diagnosis-work", RelativePath: "diag.json"}})
	if err != nil {
		t.Fatal(err)
	}
	var hypothesis *KnowledgeRecord
	for index := range records {
		if records[index].KnowledgeID == "diagnosis:diagnosis:hypothesis:h1" {
			hypothesis = &records[index]
			break
		}
	}
	if hypothesis == nil || hypothesis.Status != "verified" || hypothesis.Source.IntegrationID != "integration-1" {
		t.Fatalf("hypothesis promotion failed after slice growth: %+v", hypothesis)
	}
}

func TestBuildKnowledgeProjectionUsesPersistedScopesAndArtifactIDs(t *testing.T) {
	t.Parallel()
	report := json.RawMessage(`{"schema_version":1,"subject":{"baseline_revision_id":"baseline-1","best_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","hardware":{"gpu":"h100"},"software":{"driver":"550"}},"coverage":{"case_ids":["case-1","case-2"],"dispatch_paths":["kernel"]},"artifacts":[{"path":"diag.json","kind":"profile"}],"observations":[{"id":"o1","case_ids":["case-1"],"metric":"latency","value":1,"unit":"ms","source_artifacts":["diag.json"],"summary":"obs"}],"bottlenecks":[{"id":"b1","class":"launch","confidence":"high","observation_ids":["o1"],"summary":"b"}],"hypotheses":[{"id":"h1","rank":1,"bottleneck_ids":["b1"],"target_case_ids":["case-1"],"summary":"h","mechanism":"fuse work","expected_effect":"lower latency","risk":"correctness","knowledge_refs":[]}],"limitations":[]}`)
	work := RuntimeWork{BestSHA: "ffffffffffffffffffffffffffffffffffffffff", Diagnosis: &DiagnosisView{ID: "diagnosis", WorkID: "diagnosis-work", Report: report}}
	experiment := IterationExperimentView{
		ID:           "exp-1",
		Outcome:      "kept",
		ScopeBestSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ArtifactIDs:  []string{"exp-artifact"},
		DerivedComparisons: []BenchmarkComparisonView{
			{ExperimentID: "exp-1", CaseID: "case-1", MetricID: "latency", Speedup: 1.2, Regression: false, RegressionFraction: 0},
		},
		Experiment: json.RawMessage(`{"summary":"kept change","hypothesis":{"diagnosis_hypothesis_id":"h1"}}`),
	}
	noMeasurements := IterationExperimentView{
		ID: "exp-2", Outcome: "rejected", ScopeBestSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Experiment: json.RawMessage(`{"summary":"negative change","hypothesis":{"diagnosis_hypothesis_id":"h1"}}`),
	}
	records, err := BuildKnowledgeProjection(work, View{}, []IterationExperimentView{experiment, noMeasurements}, []EvidenceArtifact{
		{ID: "diag-artifact", WorkID: "diagnosis-work", RelativePath: "diag.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var observation, hypothesis, exp, negative *KnowledgeRecord
	for index := range records {
		switch records[index].KnowledgeID {
		case "diagnosis:diagnosis:observation:o1":
			observation = &records[index]
		case "diagnosis:diagnosis:hypothesis:h1":
			hypothesis = &records[index]
		case "experiment:exp-1":
			exp = &records[index]
		case "experiment:exp-2":
			negative = &records[index]
		}
	}
	if observation == nil || len(observation.EvidenceArtifactIDs) != 1 || observation.EvidenceArtifactIDs[0] != "diag-artifact" {
		t.Fatalf("observation artifact IDs not resolved: %+v", observation)
	}
	if hypothesis == nil || hypothesis.Scope.BestSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || hypothesis.Scope.BaselineRevisionID != "baseline-1" || hypothesis.HypothesisMechanism != "fuse work" {
		t.Fatalf("diagnosis scope/mechanism incorrect: %+v", hypothesis)
	}
	if exp == nil || exp.Scope.BestSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		exp.Scope.BaselineRevisionID != "baseline-1" || len(exp.Scope.CaseIDs) != 1 || exp.Scope.CaseIDs[0] != "case-1" ||
		exp.Scope.Hardware["gpu"] != "h100" || exp.Scope.Software["driver"] != "550" ||
		exp.Source.DiagnosisID != "diagnosis" || exp.Source.ExperimentID != "exp-1" ||
		exp.Experiment == nil || len(exp.Experiment.ArtifactIDs) != 1 ||
		exp.Experiment.ArtifactIDs[0] != "exp-artifact" || len(exp.Experiment.DerivedComparisons) != 1 {
		t.Fatalf("experiment scope/evidence incorrect: %+v", exp)
	}
	if negative == nil || negative.Status != "observed-negative" ||
		negative.Scope.BestSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
		negative.Scope.BaselineRevisionID != "baseline-1" ||
		len(negative.Scope.CaseIDs) != 1 || negative.Scope.CaseIDs[0] != "case-1" ||
		negative.Scope.Hardware["gpu"] != "h100" || negative.Scope.Software["driver"] != "550" {
		t.Fatalf("no-measurements Experiment did not inherit frozen scope: %+v", negative)
	}
}

func TestKnowledgeProjectionRetainsEmptyEnvironmentAndRejectsMissingObjects(t *testing.T) {
	t.Parallel()
	emptyEnv := json.RawMessage(`{"schema_version":1,"subject":{"baseline_revision_id":"baseline-1","best_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","hardware":{},"software":{}},"coverage":{"case_ids":["case-1"],"dispatch_paths":["kernel"]},"artifacts":[{"path":"diag.json","kind":"profile"}],"observations":[{"id":"o1","case_ids":["case-1"],"metric":"latency","value":1,"unit":"ms","source_artifacts":["diag.json"],"summary":"obs"}],"bottlenecks":[{"id":"b1","class":"launch","confidence":"high","observation_ids":["o1"],"summary":"b"}],"hypotheses":[{"id":"h1","rank":1,"bottleneck_ids":["b1"],"target_case_ids":["case-1"],"summary":"h","mechanism":"fuse","expected_effect":"lower latency","risk":"correctness","knowledge_refs":[]}],"limitations":[]}`)
	work := RuntimeWork{Diagnosis: &DiagnosisView{ID: "diagnosis", WorkID: "diagnosis-work", Report: emptyEnv}}
	records, err := BuildKnowledgeProjection(work, View{}, nil, []EvidenceArtifact{{ID: "diag-artifact", WorkID: "diagnosis-work", RelativePath: "diag.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("expected knowledge records")
	}
	encoded, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateKnowledgeRecordJSON(encoded); err != nil {
		t.Fatalf("empty environment KnowledgeRecord: %v %s", err, encoded)
	}
	if !bytes.Contains(encoded, []byte(`"hardware":{}`)) || !bytes.Contains(encoded, []byte(`"software":{}`)) {
		t.Fatalf("empty environment objects were omitted: %s", encoded)
	}
	for name, report := range map[string]json.RawMessage{
		"missing hardware": json.RawMessage(`{"subject":{"baseline_revision_id":"baseline-1","best_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","software":{}},"coverage":{"case_ids":["case-1"]},"observations":[{"id":"o1","case_ids":["case-1"],"source_artifacts":["diag.json"],"summary":"obs"}],"bottlenecks":[],"hypotheses":[]}`),
		"missing software": json.RawMessage(`{"subject":{"baseline_revision_id":"baseline-1","best_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","hardware":{}},"coverage":{"case_ids":["case-1"]},"observations":[{"id":"o1","case_ids":["case-1"],"source_artifacts":["diag.json"],"summary":"obs"}],"bottlenecks":[],"hypotheses":[]}`),
	} {
		_, err := BuildKnowledgeProjection(RuntimeWork{Diagnosis: &DiagnosisView{ID: "diagnosis", WorkID: "diagnosis-work", Report: report}}, View{}, nil, nil)
		if err == nil {
			t.Fatalf("accepted Diagnosis environment %s", name)
		}
	}
}
