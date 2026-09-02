package contextbundle_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/contextbundle"
	"github.com/reyoung/pika-go/internal/skillsnapshot"
	"github.com/reyoung/pika-go/internal/symphony"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestMaterializeFreezesCompleteCrossProviderHistoryAndValidSchemas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	engine, err := symphony.Open(ctx, filepath.Join(root, "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	workID := view.Works[0].ID
	first := symphony.AgentSession{ID: "codex-session", WorkID: workID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "codex-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	largeOutput := strings.Repeat("full-output-", 2048)
	for _, event := range []json.RawMessage{
		json.RawMessage(`{"session_id":"native-codex","turn_id":"codex-turn","hook_event_name":"UserPromptSubmit","prompt":"measure everything"}`),
		json.RawMessage(`{"session_id":"native-codex","turn_id":"codex-turn","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"codex-tool","tool_input":{"command":"benchmark"},"tool_response":{"output":` + mustJSON(t, largeOutput) + `}}`),
		json.RawMessage(`{"session_id":"native-codex","turn_id":"codex-turn","hook_event_name":"Stop","last_assistant_message":"codex answer"}`),
	} {
		if err := engine.IngestProviderEvent(ctx, "codex", first.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.MarkAgentSessionEnded(ctx, first.ID, symphony.AgentSessionExited); err != nil {
		t.Fatal(err)
	}
	second := symphony.AgentSession{ID: "cursor-session", WorkID: workID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "cursor", AgentName: "cursor-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, second); err != nil {
		t.Fatal(err)
	}
	for _, event := range []json.RawMessage{
		json.RawMessage(`{"conversation_id":"native-cursor","generation_id":"cursor-turn","hook_event_name":"beforeSubmitPrompt","prompt":"continue"}`),
		json.RawMessage(`{"conversation_id":"native-cursor","generation_id":"cursor-turn","hook_event_name":"postToolUse","tool_name":"Shell","tool_use_id":"cursor-tool","tool_input":{"command":"run"},"tool_output":"{\"exit_code\":0}"}`),
		json.RawMessage(`{"conversation_id":"native-cursor","generation_id":"cursor-turn","hook_event_name":"afterShellExecution","command":"run","output":"complete shell output","duration":17}`),
		json.RawMessage(`{"conversation_id":"native-cursor","generation_id":"cursor-turn","hook_event_name":"stop","status":"completed"}`),
	} {
		if err := engine.IngestProviderEvent(ctx, "cursor", second.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.MarkAgentSessionEnded(ctx, second.ID, symphony.AgentSessionExited); err != nil {
		t.Fatal(err)
	}
	current := symphony.AgentSession{ID: "current-session", WorkID: workID, Generation: 2, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "current-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, current); err != nil {
		t.Fatal(err)
	}
	materializer := contextbundle.Materializer{Store: engine, Root: filepath.Join(root, "contexts")}
	bundle, err := materializer.Materialize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	contextBytes, err := os.ReadFile(bundle.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	messagesBytes, err := os.ReadFile(bundle.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.MessageRecords != 2 || !bytes.Contains(messagesBytes, []byte(largeOutput)) || !bytes.Contains(messagesBytes, []byte("complete shell output")) || bytes.Contains(messagesBytes, []byte(`"events"`)) {
		t.Fatalf("incomplete or raw history: records=%d bytes=%d", bundle.MessageRecords, len(messagesBytes))
	}
	contextSchemaBytes, messageSchemaBytes, err := contextbundle.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	contextSchema := compileSchema(t, "context-schema.json", contextSchemaBytes)
	messageSchema := compileSchema(t, "message-schema.json", messageSchemaBytes)
	if err := contextSchema.Validate(unmarshalJSON(t, contextBytes)); err != nil {
		t.Fatalf("context.json does not match embedded schema: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(messagesBytes), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("messages lines = %d", len(lines))
	}
	for _, line := range lines {
		if err := messageSchema.Validate(unmarshalJSON(t, line)); err != nil {
			t.Fatalf("messages.jsonl record does not match embedded schema: %v\n%s", err, line)
		}
	}

	beforeContext, beforeMessages := append([]byte(nil), contextBytes...), append([]byte(nil), messagesBytes...)
	if err := engine.IngestProviderEvent(ctx, "codex", current.ID, json.RawMessage(`{"session_id":"native-current","turn_id":"late-turn","hook_event_name":"UserPromptSubmit","prompt":"late"}`)); err != nil {
		t.Fatal(err)
	}
	retried, err := materializer.Materialize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	afterContext, _ := os.ReadFile(retried.ContextPath)
	afterMessages, _ := os.ReadFile(retried.MessagesPath)
	if !bytes.Equal(beforeContext, afterContext) || !bytes.Equal(beforeMessages, afterMessages) {
		t.Fatal("same Agent Session did not reuse byte-identical Context Bundle")
	}
}

func TestMaterializeRejectsTamperedFrozenFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	engine, err := symphony.Open(ctx, filepath.Join(root, "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	session := symphony.AgentSession{ID: "session", WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	materializer := contextbundle.Materializer{Store: engine, Root: filepath.Join(root, "contexts")}
	bundle, err := materializer.Materialize(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bundle.ContextPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle.ContextPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(ctx, session); err == nil || !strings.Contains(err.Error(), "digest does not match") {
		t.Fatalf("tampered Context Bundle was accepted: %v", err)
	}
}

func TestMaterializeFreezesRecentAttemptSummariesAndDetailedHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	session := symphony.AgentSession{ID: "session", WorkID: "current-work", Generation: 1, Role: symphony.RoleIteration, AgentKind: "codex", AgentName: "agent", Status: symphony.AgentSessionStarting}
	store := &staticContextStore{projection: symphony.ContextProjection{
		Session: session,
		View: symphony.View{Optimization: symphony.OptimizationView{ID: "optimization", Status: symphony.OptimizationOptimizing, Revision: 1,
			Repository: "/repo", IterationConcurrency: 1, MaxPendingAttempts: 1, IterationHistoryLimit: 2},
			IterationCaseSet: &symphony.IterationCaseSetView{Version: 1, CaseIDs: []string{"case-1"}}},
		TargetWork: symphony.RuntimeWork{Work: symphony.WorkView{ID: "current-work", Role: symphony.RoleIteration, AttemptID: "current", IterationRound: 2},
			OptimizationID: "optimization", OptimizationRepository: "/repo", Repository: "/attempt", IterationHistoryLimit: 2,
			IterationCaseSet: &symphony.IterationCaseSetView{Version: 1, CaseIDs: []string{"case-1"}}},
		GeneratorWork: symphony.WorkView{ID: "current-work", Role: symphony.RoleIteration, AttemptID: "current", IterationRound: 2},
		PreviousRound: &symphony.RoundHistoryProjection{
			Work:    symphony.WorkView{ID: "previous-work", Role: symphony.RoleIteration, Status: symphony.WorkCompleted, AttemptID: "current", IterationRound: 1},
			Round:   symphony.IterationRoundView{AttemptID: "current", Round: 1, Kind: "initial", BaseSHA: "base", Status: "candidate", IterationCaseSetVersion: 1},
			Journal: symphony.ConversationJournalView{Turns: []symphony.ConversationTurnView{{ID: "previous-turn", Provider: "codex", AgentSessionID: "previous-session", ProviderSessionID: "native-previous", ProviderTurnID: "previous", Status: "completed", StartedAt: "2026-01-01T00:00:00Z", AssistantMessage: "complete previous round"}}},
		},
		AttemptHistories: []symphony.AttemptHistoryProjection{
			{Attempt: symphony.AttemptView{ID: "rejected", Status: "rejected", BaseSHA: "base-1", Summary: "failed approach", FailureReason: "regression", HistoryLimit: 2},
				Journal: symphony.ConversationJournalView{Turns: []symphony.ConversationTurnView{{ID: "turn-1", Provider: "codex", AgentSessionID: "old-session", ProviderSessionID: "native", ProviderTurnID: "turn", Status: "completed", StartedAt: "2026-01-01T00:00:00Z", AssistantMessage: "details"}}}},
			{Attempt: symphony.AttemptView{ID: "accepted", Status: "accepted", BaseSHA: "base-2", CandidateSHA: "candidate", Summary: "working approach", HistoryLimit: 2}},
		},
	}}
	materializer := contextbundle.Materializer{Store: store, Root: filepath.Join(t.TempDir(), "contexts")}
	bundle, err := materializer.Materialize(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(bundle.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	var document contextbundle.Document
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	if document.Iteration == nil || document.Iteration.HistoryLimit != 2 || len(document.Iteration.RecentTerminalAttempts) != 2 {
		t.Fatalf("iteration context = %+v", document.Iteration)
	}
	if document.Iteration.RequiredCaseSet.Version != 1 || !slices.Equal(document.Iteration.RequiredCaseSet.CaseIDs, []string{"case-1"}) {
		t.Fatalf("required Iteration Case Snapshot = %+v", document.Iteration.RequiredCaseSet)
	}
	if document.Iteration.PreviousRound == nil || document.Iteration.PreviousRound.Messages.Records != 1 {
		t.Fatalf("previous Round context = %+v", document.Iteration.PreviousRound)
	}
	previousBytes, err := os.ReadFile(document.Iteration.PreviousRound.Messages.Path)
	if err != nil || !bytes.Contains(previousBytes, []byte("complete previous round")) {
		t.Fatalf("previous Round history is incomplete: err=%v bytes=%s", err, previousBytes)
	}
	for index, want := range []string{"rejected", "accepted"} {
		history := document.Iteration.RecentTerminalAttempts[index]
		if history.Attempt.ID != want {
			t.Fatalf("history[%d] = %s, want %s", index, history.Attempt.ID, want)
		}
		if _, err := os.Stat(history.Messages.Path); err != nil {
			t.Fatalf("history messages: %v", err)
		}
		if _, err := os.Stat(history.Summary.Path); err != nil {
			t.Fatalf("history summary: %v", err)
		}
	}
	summarySchemaBytes, err := contextbundle.SummarySchema()
	if err != nil {
		t.Fatal(err)
	}
	summarySchema := compileSchema(t, "summary-schema.json", summarySchemaBytes)
	summaryBytes, err := os.ReadFile(document.Iteration.RecentTerminalAttempts[0].Summary.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := summarySchema.Validate(unmarshalJSON(t, bytes.TrimSpace(summaryBytes))); err != nil {
		t.Fatalf("summary.jsonl does not match embedded schema: %v", err)
	}
	if document.Iteration.RecentTerminalAttempts[1].Messages.Records != 0 {
		t.Fatal("terminal Attempt with an empty journal did not retain a zero-record history file")
	}
	if bytes.Contains(contents, []byte(`"evidence_root"`)) || bytes.Contains(contents, []byte(`"experiments"`)) || bytes.Contains(contents, []byte(`"skill_snapshot"`)) {
		t.Fatal("frozen flow-v1 Context leaked a v4-only field")
	}
	contextSchemaBytes, _, err := contextbundle.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	if err := compileSchema(t, "legacy-context-schema.json", contextSchemaBytes).Validate(unmarshalJSON(t, contents)); err != nil {
		t.Fatalf("flow-v1 Iteration Context violates the frozen schema: %v", err)
	}
	tampered := document.Iteration.RecentTerminalAttempts[0].Summary.Path
	if err := os.Chmod(tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tampered, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(ctx, session); err == nil || !strings.Contains(err.Error(), "Attempt history digest") {
		t.Fatalf("tampered Attempt history was accepted: %v", err)
	}
}

func TestMaterializeFlowV2ArtifactFirstContextWithZeroHistory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	snapshot := prepareContextSkillSnapshot(t, root)
	session := symphony.AgentSession{ID: "v2-session", WorkID: "iteration-work", Generation: 1, Role: symphony.RoleIteration, AgentKind: "codex", AgentName: "agent", Status: symphony.AgentSessionStarting, ProviderCapabilities: json.RawMessage(`{"unmodeled":"must-not-inline"}`)}
	experiment := json.RawMessage(`{"schema_version":1,"parent_checkpoint_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","hypothesis":{"summary":"fuse"},"change":{"summary":"fuse","paths":["target.txt"],"mechanism":"fusion"},"outcome":"rejected","artifacts":[{"path":"experiments/raw-path.json","kind":"benchmark"}],"summary":"negative"}`)
	store := &staticContextStore{projection: symphony.ContextProjection{
		Session: session,
		View: symphony.View{Optimization: symphony.OptimizationView{ID: "optimization", Status: symphony.OptimizationOptimizing, Revision: 3, Repository: "/repo", FlowVersion: symphony.FlowVersion2, IterationHistoryLimit: 0},
			Baseline:         &symphony.BaselineView{ID: "baseline", Number: 1, Status: symphony.BaselineAccepted, Definition: json.RawMessage(`{"unmodeled":"must-not-inline"}`), VerificationEvidence: json.RawMessage(`{"unmodeled":"must-not-inline"}`)},
			IterationCaseSet: &symphony.IterationCaseSetView{Version: 1, CaseIDs: []string{"case-1"}},
			IterationRounds:  []symphony.IterationRoundView{{AttemptID: "attempt", Round: 1, Kind: "iteration", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "running", IterationCaseSetVersion: 1, Evidence: json.RawMessage(`{"unmodeled":"must-not-inline"}`)}},
			Integrations:     []symphony.IntegrationView{{Sequence: 1, FIFOPosition: 1, ID: "integration", AttemptID: "attempt", IterationRound: 1, Status: "rejected", CandidateSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ExpectedBestSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CandidateExperimentID: "experiment", RegressionCases: []symphony.RegressionCase{{CaseID: "case-1", Kind: "performance", Summary: "regressed", Evidence: json.RawMessage(`{"unmodeled":"must-not-inline"}`)}}}}},
		TargetWork: symphony.RuntimeWork{Work: symphony.WorkView{ID: "iteration-work", Role: symphony.RoleIteration, AttemptID: "attempt", IterationRound: 1},
			OptimizationID: "optimization", OptimizationRepository: "/repo", Repository: "/attempt", FlowVersion: symphony.FlowVersion2, BestSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", IterationHistoryLimit: 0,
			IterationCaseSet:     &symphony.IterationCaseSetView{Version: 1, CaseIDs: []string{"case-1"}},
			SkillSnapshot:        &symphony.SkillSnapshotView{SchemaVersion: snapshot.Input.SchemaVersion, SnapshotID: snapshot.Input.SnapshotID, RootPath: snapshot.Input.RootPath, ManifestSHA256: snapshot.Input.ManifestSHA256, Entries: snapshot.Input.Entries},
			Diagnosis:            &symphony.DiagnosisView{ID: "diagnosis", WorkID: "diagnosis-work", Status: symphony.DiagnosisReady, HypothesisCount: 1, Report: json.RawMessage(`{"schema_version":1,"subject":{"baseline_revision_id":"baseline","best_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","hardware":{},"software":{}},"coverage":{"case_ids":["case-1"],"dispatch_paths":["kernel"]},"artifacts":[{"path":"evidence.txt","kind":"fixture-evidence"}],"observations":[{"id":"o","case_ids":["case-1"],"metric":"latency","value":1,"unit":"ms","source_artifacts":["evidence.txt"],"summary":"measured"}],"bottlenecks":[{"id":"b","class":"launch-overhead","confidence":"low","observation_ids":["o"],"summary":"launch"}],"hypotheses":[{"id":"h","rank":1,"bottleneck_ids":["b"],"target_case_ids":["case-1"],"summary":"fuse","mechanism":"remove launches","expected_effect":"lower latency","risk":"correctness","knowledge_refs":[]}],"limitations":[]}`)},
			IterationExperiments: []symphony.IterationExperimentView{{ID: "experiment", AttemptID: "attempt", IterationRound: 1, Sequence: 1, Outcome: "rejected", ParentCheckpointSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ScopeBestSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ReceiptID: "receipt", ArtifactIDs: []string{"artifact-id"}, Experiment: experiment}}},
		GeneratorWork:        symphony.WorkView{ID: "iteration-work", Role: symphony.RoleIteration, AttemptID: "attempt", IterationRound: 1},
		KnowledgeExperiments: []symphony.IterationExperimentView{{ID: "experiment", AttemptID: "attempt", IterationRound: 1, Sequence: 1, Outcome: "rejected", ParentCheckpointSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ScopeBestSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ReceiptID: "receipt", ArtifactIDs: []string{"artifact-id"}, Experiment: experiment}},
		PreviousRound:        &symphony.RoundHistoryProjection{Work: symphony.WorkView{ID: "previous-work", Role: symphony.RoleIteration, AttemptID: "attempt", IterationRound: 1}, Round: symphony.IterationRoundView{AttemptID: "attempt", Round: 1, Kind: "iteration", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "completed", IterationCaseSetVersion: 1, Evidence: json.RawMessage(`{"unmodeled":"must-not-inline"}`)}},
	}}
	evidenceRoot := filepath.Join(root, "evidence")
	bundle, err := (contextbundle.Materializer{Store: store, Root: filepath.Join(root, "contexts"), EvidenceRoot: evidenceRoot}).Materialize(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.SchemaVersion != contextbundle.SchemaVersion {
		t.Fatalf("flow-v2 Context schema = %d", bundle.SchemaVersion)
	}
	contents, err := os.ReadFile(bundle.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	var document contextbundle.Document
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != 4 || document.TerminalOperation != "finish_iteration" || document.SkillSnapshot == nil || document.Diagnosis == nil || document.Diagnosis.Report == nil || document.Knowledge == nil || document.Iteration == nil || document.Iteration.Experiments == nil || len(document.Iteration.RecentTerminalAttempts) != 0 {
		t.Fatalf("artifact-first v4 Context = %+v", document)
	}
	wantEvidenceRoot := filepath.Join(evidenceRoot, "iterations", "iteration-work")
	if document.Iteration.EvidenceRoot != wantEvidenceRoot {
		t.Fatalf("Iteration evidence root = %q, want %q", document.Iteration.EvidenceRoot, wantEvidenceRoot)
	}
	if info, err := os.Stat(wantEvidenceRoot); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("protected Iteration evidence root mode = %v, err=%v", info, err)
	}
	if document.Work.SkillSnapshot != nil || document.Work.Diagnosis != nil || len(document.Work.IterationExperiments) != 0 {
		t.Fatalf("v4 context inlined an artifact payload in runtime work: %+v", document.Work)
	}
	if document.Diagnosis.ID != "diagnosis" || document.Diagnosis.WorkID != "diagnosis-work" || document.Diagnosis.Status != symphony.DiagnosisReady || document.Diagnosis.Hypotheses != 1 {
		t.Fatalf("v4 diagnosis identity/status projection = %+v", document.Diagnosis)
	}
	experimentLedger, err := os.ReadFile(document.Iteration.Experiments.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(experimentLedger, []byte(`"artifact_ids":["experiments/raw-path.json"]`)) {
		t.Fatalf("Experiment ledger treated an artifact path as a durable ID: %s", experimentLedger)
	}
	if !bytes.Contains(experimentLedger, []byte(`"artifact_ids":["artifact-id"]`)) {
		t.Fatalf("Experiment ledger omitted durable receipt artifact ID: %s", experimentLedger)
	}
	if !bytes.Contains(experimentLedger, []byte(`"scope_best_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`)) {
		t.Fatalf("Experiment ledger omitted persisted scope: %s", experimentLedger)
	}
	assertExperimentLedgerReceipt(t, document.Iteration.Experiments.Path, "receipt")

	integrationSession := session
	integrationSession.ID = "v2-integration-session"
	integrationSession.WorkID = "integration-work"
	integrationSession.Role = symphony.RoleIntegration
	integrationProjection := store.projection
	integrationProjection.Session = integrationSession
	integrationProjection.TargetWork.Work = symphony.WorkView{ID: "integration-work", Role: symphony.RoleIntegration, AttemptID: "attempt", IterationRound: 1, IntegrationID: "integration"}
	integrationProjection.GeneratorWork = integrationProjection.TargetWork.Work
	integrationStore := &staticContextStore{projection: integrationProjection}
	integrationBundle, err := (contextbundle.Materializer{Store: integrationStore, Root: filepath.Join(root, "integration-contexts"), EvidenceRoot: evidenceRoot}).Materialize(context.Background(), integrationSession)
	if err != nil {
		t.Fatal(err)
	}
	integrationContents, err := os.ReadFile(integrationBundle.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	var integrationDocument contextbundle.Document
	if err := json.Unmarshal(integrationContents, &integrationDocument); err != nil {
		t.Fatal(err)
	}
	if integrationDocument.Integration == nil || integrationDocument.Integration.CandidateExperimentID != "experiment" || integrationDocument.Integration.Experiments.Records != 1 {
		t.Fatalf("Integration Context omitted candidate Experiment ledger: %+v", integrationDocument.Integration)
	}
	assertExperimentLedgerReceipt(t, integrationDocument.Integration.Experiments.Path, "receipt")
	integrationSchemaBytes, _, err := contextbundle.SchemasForVersion(contextbundle.SchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	integrationSchema := compileSchema(t, "integration-context-v4.schema.json", integrationSchemaBytes)
	if err := integrationSchema.Validate(unmarshalJSON(t, integrationContents)); err != nil {
		t.Fatalf("Integration context schema validation: %v", err)
	}
	// The unavailable path retains factual hypotheses too.  It must derive the
	// count from the same strict artifact rather than treating unavailable as an
	// empty diagnosis.
	unavailable := session
	unavailable.ID = "v2-unavailable-session"
	unavailableProjection := store.projection
	unavailableProjection.Session = unavailable
	unavailableDiagnosis := *unavailableProjection.TargetWork.Diagnosis
	unavailableDiagnosis.Status = symphony.DiagnosisUnavailable
	unavailableProjection.TargetWork.Diagnosis = &unavailableDiagnosis
	unavailableStore := &staticContextStore{projection: unavailableProjection}
	unavailableBundle, err := (contextbundle.Materializer{Store: unavailableStore, Root: filepath.Join(root, "contexts"), EvidenceRoot: evidenceRoot}).Materialize(context.Background(), unavailable)
	if err != nil {
		t.Fatal(err)
	}
	unavailableContents, err := os.ReadFile(unavailableBundle.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	var unavailableDocument contextbundle.Document
	if err := json.Unmarshal(unavailableContents, &unavailableDocument); err != nil {
		t.Fatal(err)
	}
	if unavailableDocument.Diagnosis == nil || unavailableDocument.Diagnosis.Status != symphony.DiagnosisUnavailable || unavailableDocument.Diagnosis.Hypotheses != 1 {
		t.Fatalf("unavailable v4 Diagnosis count did not match artifact: %+v", unavailableDocument.Diagnosis)
	}
	contextSchemaBytes, _, err := contextbundle.SchemasForVersion(contextbundle.SchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	contextSchema := compileSchema(t, "context-v4.schema.json", contextSchemaBytes)
	if err := contextSchema.Validate(unmarshalJSON(t, contents)); err != nil {
		t.Fatalf("flow-v2 context schema validation: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(contents, &raw); err != nil {
		t.Fatal(err)
	}
	if _, found := raw["skill_snapshot"].(map[string]any)["manifest"].(map[string]any)["records"]; found {
		t.Fatal("skill manifest blob reference exposed JSONL records")
	}
	if _, found := raw["diagnosis"].(map[string]any)["report"].(map[string]any)["records"]; found {
		t.Fatal("Diagnosis report blob reference exposed JSONL records")
	}
	if _, found := raw["knowledge"].(map[string]any)["records"]; !found {
		t.Fatal("knowledge JSONL reference omitted records")
	}
	work := raw["work"].(map[string]any)
	if _, found := work["skill_snapshot"]; found {
		t.Fatal("v4 context.json inlined skill_snapshot instead of its manifest reference")
	}
	if _, found := work["diagnosis"]; found {
		t.Fatal("v4 context.json inlined diagnosis report instead of its artifact reference")
	}
	if _, found := work["iteration_experiments"]; found {
		t.Fatal("v4 context.json inlined Experiment ledger instead of its artifact reference")
	}
	if bytes.Contains(contents, []byte("must-not-inline")) {
		t.Fatal("v4 context.json retained an unmodeled raw JSON payload")
	}
	raw["unexpected"] = true
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("flow-v2 Context schema accepted an unknown top-level field")
	}
	raw = unmarshalJSON(t, contents).(map[string]any)
	raw["session"].(map[string]any)["unexpected"] = true
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("flow-v2 Context schema accepted an unknown nested session field")
	}
	raw = unmarshalJSON(t, contents).(map[string]any)
	raw["optimization"].(map[string]any)["flow_version"] = float64(1)
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("v4 Context schema accepted a flow-v1 optimization")
	}
	raw = unmarshalJSON(t, contents).(map[string]any)
	raw["work"].(map[string]any)["flow_version"] = float64(1)
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("v4 Context schema accepted a flow-v1 runtime work projection")
	}
	raw = unmarshalJSON(t, contents).(map[string]any)
	delete(raw["iteration_context"].(map[string]any), "evidence_root")
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("v4 Iteration Context schema accepted a missing evidence_root")
	}
	raw = unmarshalJSON(t, contents).(map[string]any)
	raw["skill_snapshot"].(map[string]any)["entries"] = nil
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("v4 Context schema accepted null skill snapshot entries")
	}
	raw = unmarshalJSON(t, contents).(map[string]any)
	raw["skill_snapshot"].(map[string]any)["entries"].([]any)[0].(map[string]any)["branch"] = "main"
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("v4 Context schema accepted a reordered or non-allowlisted skill provenance entry")
	}
	raw = unmarshalJSON(t, contents).(map[string]any)
	raw["skill_snapshot"].(map[string]any)["entries"].([]any)[0].(map[string]any)["path"] = "skills/elsewhere"
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("v4 Context schema accepted a non-canonical skill path")
	}
	raw = unmarshalJSON(t, contents).(map[string]any)
	raw["skill_snapshot"].(map[string]any)["snapshot_id"] = "short"
	if err := contextSchema.Validate(raw); err == nil {
		t.Fatal("v4 Context schema accepted a non-SHA-256 snapshot_id")
	}
	drifted := store.projection
	drifted.Session.ID = "v2-drifted-session"
	drifted.TargetWork.SkillSnapshot = &symphony.SkillSnapshotView{}
	*drifted.TargetWork.SkillSnapshot = *store.projection.TargetWork.SkillSnapshot
	drifted.TargetWork.SkillSnapshot.Entries = append([]symphony.SkillSnapshotEntry{}, store.projection.TargetWork.SkillSnapshot.Entries...)
	drifted.TargetWork.SkillSnapshot.Entries[1].ContentSHA256 = strings.Repeat("3", 64)
	driftedSession := session
	driftedSession.ID = drifted.Session.ID
	if _, err := (contextbundle.Materializer{Store: &staticContextStore{projection: drifted}, Root: filepath.Join(root, "drifted-contexts"), EvidenceRoot: evidenceRoot}).Materialize(context.Background(), driftedSession); err == nil || !strings.Contains(err.Error(), "frozen skill") {
		t.Fatalf("v4 Context accepted skill entries that drifted from the manifest: %v", err)
	}
}

func TestMaterializeFlowV3BenchmarkContextAndSchema(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	snapshot := prepareContextSkillSnapshot(t, root)
	cycle := symphony.ExperimentCycleView{
		ID: "cycle", AttemptID: "attempt", IterationRound: 1, Sequence: 1,
		CheckpointSHA: strings.Repeat("a", 40), BaselineDefinitionSHA: strings.Repeat("b", 64),
		CaseSnapshotSHA: strings.Repeat("c", 64), Status: "benchmark_pending", BenchmarkWorkID: "benchmark-work",
	}
	session := symphony.AgentSession{ID: "v3-benchmark-session", WorkID: "benchmark-work", Generation: 1, Role: symphony.RoleBenchmark, AgentKind: "codex", AgentName: "benchmark-agent", Status: symphony.AgentSessionStarting}
	work := symphony.RuntimeWork{
		Work:       symphony.WorkView{ID: "benchmark-work", BaselineRevisionID: "baseline", Role: symphony.RoleBenchmark, Status: symphony.WorkPending, Generation: 1, AttemptID: "attempt", IterationRound: 1, ExperimentCycleID: cycle.ID},
		Repository: "/benchmark", OptimizationRepository: "/repo", OptimizationID: "optimization", OptimizationStatus: symphony.OptimizationOptimizing,
		OptimizationRevision: 4, BaselineNumber: 1, BaselineStatus: symphony.BaselineAccepted, BaselineDefinitionSHA256: strings.Repeat("b", 64),
		BaseSHA: strings.Repeat("a", 40), BestSHA: strings.Repeat("a", 40), FlowVersion: symphony.FlowVersion3,
		CurrentCheckpointSHA: strings.Repeat("a", 40), IterationCaseSet: &symphony.IterationCaseSetView{Version: 1, CaseIDs: []string{"case-1"}},
		SkillSnapshot:   &symphony.SkillSnapshotView{SchemaVersion: snapshot.Input.SchemaVersion, SnapshotID: snapshot.Input.SnapshotID, RootPath: snapshot.Input.RootPath, ManifestSHA256: snapshot.Input.ManifestSHA256, Entries: snapshot.Input.Entries},
		ExperimentCycle: &cycle,
	}
	store := &staticContextStore{projection: symphony.ContextProjection{
		Session: session,
		View: symphony.View{
			Optimization: symphony.OptimizationView{ID: "optimization", Status: symphony.OptimizationOptimizing, Revision: 4, Repository: "/repo", FlowVersion: symphony.FlowVersion3},
			Baseline:     &symphony.BaselineView{ID: "baseline", Number: 1, Status: symphony.BaselineAccepted}, Best: &symphony.BestView{ID: "best", CommitSHA: strings.Repeat("a", 40)},
			IterationCaseSet: &symphony.IterationCaseSetView{Version: 1, CaseIDs: []string{"case-1"}}, ExperimentCycles: []symphony.ExperimentCycleView{cycle},
		},
		TargetWork: work, GeneratorWork: work.Work,
	}}
	evidenceRoot := filepath.Join(root, "evidence")
	bundle, err := (contextbundle.Materializer{Store: store, Root: filepath.Join(root, "contexts"), EvidenceRoot: evidenceRoot}).Materialize(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.SchemaVersion != contextbundle.FlowV3SchemaVersion {
		t.Fatalf("flow-v3 Context schema = %d", bundle.SchemaVersion)
	}
	contents, err := os.ReadFile(bundle.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	contextSchemaBytes, messageSchemaBytes, err := contextbundle.SchemasForVersion(contextbundle.FlowV3SchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := compileSchema(t, "context-v5.schema.json", contextSchemaBytes).Validate(unmarshalJSON(t, contents)); err != nil {
		t.Fatalf("flow-v3 Context schema validation: %v\n%s", err, contents)
	}
	_ = compileSchema(t, "message-v5.schema.json", messageSchemaBytes)
	summarySchema, err := contextbundle.SummarySchemaForVersion(contextbundle.FlowV3SchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	_ = compileSchema(t, "summary-v5.schema.json", summarySchema)
	var document contextbundle.Document
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	wantEvidenceRoot := filepath.Join(evidenceRoot, "benchmarks", "benchmark-work")
	if document.Benchmark == nil || document.Benchmark.EvidenceRoot != wantEvidenceRoot || document.ExperimentCycle == nil || document.ExperimentCycle.ID != cycle.ID || !slices.Equal(document.AllowedTerminals, []string{"finish_iteration_benchmark"}) {
		t.Fatalf("flow-v3 Benchmark Context omitted gate identity: %+v", document)
	}
}

type contextSkillGit struct{ remotes map[string]string }

func (g contextSkillGit) Run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	mapped := append([]string{}, arguments...)
	if !(len(arguments) >= 3 && arguments[0] == "remote" && arguments[1] == "set-url") {
		for index, value := range mapped {
			if replacement, found := g.remotes[value]; found {
				mapped[index] = replacement
			}
		}
	}
	command := exec.CommandContext(ctx, "git", mapped...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w: %s", mapped, err, output)
	}
	if len(arguments) == 3 && arguments[0] == "remote" && arguments[1] == "get-url" && arguments[2] == "origin" {
		for canonical, local := range g.remotes {
			if string(output) == local+"\n" {
				return []byte(canonical + "\n"), nil
			}
		}
	}
	return output, nil
}

func prepareContextSkillSnapshot(t *testing.T, workspace string) skillsnapshot.Snapshot {
	t.Helper()
	remotes := map[string]string{
		symphony.KernelWikiRepository:     createContextSkillRepository(t, "master", "KernelWiki"),
		symphony.NCUReportSkillRepository: createContextSkillRepository(t, "main", "ncu-report-skill"),
	}
	snapshot, err := (skillsnapshot.Manager{Git: contextSkillGit{remotes: remotes}}).Prepare(context.Background(), skillsnapshot.Request{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("prepare Context skill snapshot: %v", err)
	}
	return snapshot
}

func createContextSkillRepository(t *testing.T, branch, name string) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), name)
	for _, arguments := range [][]string{{"init", "--quiet", "--initial-branch=" + branch, repository}, {"-C", repository, "config", "user.name", "Pika Test"}, {"-C", repository, "config", "user.email", "pika@example.invalid"}} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Context fixture\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"-C", repository, "add", "SKILL.md"}, {"-C", repository, "commit", "--quiet", "-m", "fixture"}} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	return repository
}

func assertExperimentLedgerReceipt(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ReceiptID string `json:"receipt_id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(contents), &envelope); err != nil {
		t.Fatalf("decode Experiment ledger: %v", err)
	}
	if envelope.ReceiptID != want {
		t.Fatalf("Experiment ledger receipt_id = %q, want %q: %s", envelope.ReceiptID, want, contents)
	}
}

func sha256Hex(contents []byte) string {
	value := sha256.Sum256(contents)
	return hex.EncodeToString(value[:])
}

type staticContextStore struct {
	projection symphony.ContextProjection
	snapshot   symphony.ContextSnapshot
	frozen     bool
}

func (s *staticContextStore) ContextProjection(context.Context, string) (symphony.ContextProjection, error) {
	return s.projection, nil
}

func (s *staticContextStore) ReadContextSnapshot(context.Context, string) (symphony.ContextSnapshot, bool, error) {
	return s.snapshot, s.frozen, nil
}

func (s *staticContextStore) FreezeContextSnapshot(_ context.Context, snapshot symphony.ContextSnapshot) (symphony.ContextSnapshot, error) {
	if !s.frozen {
		s.snapshot, s.frozen = snapshot, true
	}
	return s.snapshot, nil
}

func compileSchema(t *testing.T, name string, contents []byte) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(name, unmarshalJSON(t, contents)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func unmarshalJSON(t *testing.T, contents []byte) any {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
