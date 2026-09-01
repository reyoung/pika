package symphony

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestOperatorStatusDoesNotLeakEvidenceOrProviderPayloads(t *testing.T) {
	t.Parallel()
	const secret = "raw-sensitive-payload"
	view := View{
		Optimization:         OptimizationView{ID: "optimization", Status: OptimizationOptimizing, Repository: secret, FlowVersion: FlowVersion2},
		Scheduler:            SchedulerView{Status: SchedulerRunning, Latest: &SchedulerControlCycleView{Actions: []SchedulerControlActionView{{Message: secret, Error: secret}}}},
		Baselines:            []BaselineView{{ID: "baseline", Status: BaselineAccepted, Definition: json.RawMessage(`{"secret":"` + secret + `"}`), VerificationEvidence: json.RawMessage(`{"secret":"` + secret + `"}`)}},
		Bests:                []BestView{{ID: "best", CommitSHA: "sha", Evidence: json.RawMessage(`{"secret":"` + secret + `"}`)}},
		Integrations:         []IntegrationView{{ID: "integration", Validation: json.RawMessage(`{"secret":"` + secret + `"}`), Result: json.RawMessage(`{"secret":"` + secret + `"}`), RegressionCases: []RegressionCase{{Evidence: json.RawMessage(`{"secret":"` + secret + `"}`)}}}},
		IterationRounds:      []IterationRoundView{{AttemptID: "attempt", Round: 1, Evidence: json.RawMessage(`{"secret":"` + secret + `"}`)}},
		Diagnoses:            []DiagnosisView{{ID: "diagnosis", Status: DiagnosisReady, Report: json.RawMessage(`{"secret":"` + secret + `"}`)}},
		IterationExperiments: []IterationExperimentView{{ID: "experiment", Outcome: "rejected", ScopeBestSHA: "scope", ReceiptID: "receipt", Experiment: json.RawMessage(`{"secret":"` + secret + `"}`), DerivedComparisons: []BenchmarkComparisonView{{}}, ArtifactIDs: []string{"artifact"}}},
		AgentSessions:        []AgentSession{{ID: "session", ProviderCapabilities: json.RawMessage(`{"secret":"` + secret + `"}`)}},
		FollowUps:            []FollowUpView{{ID: "follow-up", Message: secret}},
		Attempts:             []AttemptView{{ID: "attempt", Summary: secret, FailureReason: secret}},
		BackOffs:             []BackOffView{{ID: "back-off", Message: secret}},
		SkillSnapshot: &SkillSnapshotView{
			SchemaVersion: 1, SnapshotID: "snapshot-id", RootPath: secret, ManifestSHA256: secret,
			Entries: []SkillSnapshotEntry{{Name: "KernelWiki", CommitSHA: "commit", Repository: secret, RelativePath: secret, ContentSHA256: secret}},
		},
	}
	encoded, err := json.Marshal(ProjectOperatorStatus(view))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(secret)) {
		t.Fatalf("operator status leaked raw payload: %s", encoded)
	}
	for _, forbidden := range [][]byte{
		[]byte(`"definition"`), []byte(`"verification_evidence"`), []byte(`"report"`),
		[]byte(`"experiment":`), []byte(`"validation"`), []byte(`"result"`),
		[]byte(`"provider_capabilities"`), []byte(`"message"`),
		[]byte(`"summary"`), []byte(`"failure_reason"`), []byte(`"root_path"`),
		[]byte(`"content_sha256"`), []byte(`"relative_path"`), []byte(`"repository"`), []byte(`"path"`),
	} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("operator status contains forbidden field %s: %s", forbidden, encoded)
		}
	}
	if !bytes.Contains(encoded, []byte(`"hypothesis_count"`)) || !bytes.Contains(encoded, []byte(`"measurement_count"`)) {
		t.Fatalf("operator status omitted KDA counts: %s", encoded)
	}
	for _, count := range [][]byte{[]byte(`"measurement_count":1`), []byte(`"artifact_count":1`), []byte(`"regression_case_count":1`)} {
		if !bytes.Contains(encoded, count) {
			t.Fatalf("operator status did not retain count %s: %s", count, encoded)
		}
	}
}
