package workruntime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/activation"
	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/instructions"
	"github.com/reyoung/pika-go/internal/outbox"
	"github.com/reyoung/pika-go/internal/provider"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
	"github.com/reyoung/pika-go/internal/workruntime"
)

type matrixRuntime struct {
	starts  []workruntime.StartSpec
	active  []workruntime.Observation
	prompts []struct{ pane, message string }
	closed  []string
}

func (r *matrixRuntime) Snapshot(context.Context) (workruntime.Snapshot, error) {
	return workruntime.Snapshot{Sessions: append([]workruntime.Observation(nil), r.active...)}, nil
}

func (r *matrixRuntime) Start(_ context.Context, spec workruntime.StartSpec) (workruntime.Observation, error) {
	r.starts = append(r.starts, spec)
	index := len(r.starts)
	observation := workruntime.Observation{
		AgentName: spec.AgentName, AgentKind: spec.AgentKind,
		WorkspaceID: "workspace", TabID: fmt.Sprintf("tab-%d", index), PaneID: fmt.Sprintf("pane-%d", index),
		TerminalID: fmt.Sprintf("terminal-%d", index), Status: "idle",
	}
	r.active = append(r.active, observation)
	return observation, nil
}

func (r *matrixRuntime) Prompt(_ context.Context, pane, message string) error {
	r.prompts = append(r.prompts, struct{ pane, message string }{pane, message})
	return nil
}

func (r *matrixRuntime) Close(_ context.Context, pane string) error {
	r.closed = append(r.closed, pane)
	for index, observation := range r.active {
		if observation.PaneID == pane {
			r.active = append(r.active[:index], r.active[index+1:]...)
			break
		}
	}
	return nil
}

func TestMixedProviderRoleMatricesPreserveLifecycleAndIsolation(t *testing.T) {
	matrices := []struct {
		name           string
		agents         map[string]configuration.Agent
		iterationKinds []string
	}{
		{name: "cursor-codex-cursor-codex-cursor", agents: matrixAgents("cursor", "codex", "cursor", "codex", "cursor")},
		{name: "codex-cursor-codex-cursor-codex", agents: matrixAgents("codex", "cursor", "codex", "cursor", "codex")},
		{name: "independent-codex-cursor-iteration-slots", agents: matrixAgents("codex", "codex", "codex", "codex", "codex"), iterationKinds: []string{"codex", "cursor"}},
	}
	for _, matrix := range matrices {
		t.Run(matrix.name, func(t *testing.T) {
			iterationAgents := make([]configuration.Agent, 2)
			for index := range iterationAgents {
				iterationAgents[index] = matrix.agents["iteration"]
			}
			for index, kind := range matrix.iterationKinds {
				iterationAgents[index].Kind = kind
				iterationAgents[index].Args = nil
				if kind == "cursor" {
					iterationAgents[index].Args = []string{"--force", "--approve-mcps"}
				}
			}
			runMixedProviderMatrix(t, matrix.agents, iterationAgents)
		})
	}
}

func runMixedProviderMatrix(t *testing.T, agents map[string]configuration.Agent, iterationAgents []configuration.Agent) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	instructionRoot := filepath.Join(root, "instructions")
	if err := instructions.Install(instructionRoot); err != nil {
		t.Fatal(err)
	}
	pikaExecutable := writeMatrixExecutable(t, root, "pika-go")
	cursorExecutable := writeMatrixExecutable(t, root, "cursor-real")
	registry, err := provider.NewRegistry(provider.NewCodexAdapter(), provider.NewCursorAdapter(provider.CursorOptions{
		Executable: cursorExecutable, RuntimeRoot: runtimeRoot, InstanceBin: filepath.Join(runtimeRoot, "bin"), PikaExecutable: pikaExecutable,
	}))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, filepath.Join(root, "pika.db"), symphony.Options{
		Now: func() time.Time { return now }, FollowUpInactivity: time.Minute, Providers: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
		IterationConcurrency: int64(len(iterationAgents)), MaxPendingAttempts: 4,
	}); err != nil {
		t.Fatal(err)
	}
	configContents, err := configuration.RenderConfiguration("/repo", agents, iterationAgents...)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContents), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &matrixRuntime{}
	preparer := activation.Preparer{
		Store: engine, InstructionRoot: instructionRoot, ContextsRoot: filepath.Join(root, "contexts"), SocketPath: "/tmp/pika.sock", AgentConfigPath: configPath, Providers: registry,
	}
	sink := workruntime.Sink{
		Store: engine, Runtime: runtime, AgentConfigPath: configPath, Providers: registry,
		ProviderRuntimeRoot: runtimeRoot, Preparer: preparer,
	}
	dispatcher := outbox.Dispatcher{Store: engine, Sink: sink}
	dispatch := func() {
		t.Helper()
		if err := dispatcher.DispatchPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	dispatch()
	assertLastStartKind(t, runtime, agents["baseline"].Kind)
	assertLaunchIsolation(t, runtime.starts)

	view, _ := engine.Inspect(ctx, symphony.Status{})
	baseline := matrixPendingWork(t, view, symphony.RoleBaselineDraft)
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "baseline"}, WorkID: baseline.ID, Definition: testcontract.Definition(),
	}); err != nil {
		t.Fatal(err)
	}
	dispatch()
	if promptPath := runtime.starts[0].Environment["PIKA_CURSOR_SYSTEM_PROMPT_PATH"]; promptPath != "" {
		sessionDir := filepath.Dir(promptPath)
		if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
			t.Fatalf("completed Cursor Baseline state remains: %s err=%v", sessionDir, err)
		}
	}
	assertLastStartKind(t, runtime, agents["baseline_verify"].Kind)
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verification := matrixPendingWork(t, view, symphony.RoleBaselineVerification)
	targetSession, targetBinding, found, err := engine.CurrentAgentSession(ctx, verification.ID)
	if err != nil || !found {
		t.Fatalf("verification Session found=%v err=%v", found, err)
	}
	oppositeKind := "codex"
	if targetSession.AgentKind == "codex" {
		oppositeKind = "cursor"
	}
	if err := engine.IngestProviderEvent(ctx, oppositeKind, targetSession.ID, matrixStopEvent(oppositeKind, "collision", "turn")); err == nil {
		t.Fatal("provider-route mismatch was accepted")
	}
	if err := engine.IngestProviderEvent(ctx, targetSession.AgentKind, targetSession.ID, matrixStopEvent(targetSession.AgentKind, "target-native", "target-turn")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote Follow-up=%v err=%v", promoted, err)
	}
	dispatch()
	assertLastStartKind(t, runtime, agents["follow_up"].Kind)
	view, _ = engine.Inspect(ctx, symphony.Status{})
	followUp := matrixPendingWork(t, view, symphony.RoleFollowUp)
	followUpSession, _, found, err := engine.CurrentAgentSession(ctx, followUp.ID)
	if err != nil || !found {
		t.Fatalf("Follow-up Session found=%v err=%v", found, err)
	}
	grant := startForSession(t, runtime.starts, followUpSession.ID).Environment["PIKA_MCP_GRANT"]
	if grant == "" {
		t.Fatal("Follow-up launch has no Session-scoped MCP grant")
	}
	if _, err := (toolapp.Application{Store: engine}).Invoke(ctx, grant, toolapp.Call{
		Name: "submit_followup_message", Arguments: json.RawMessage(`{"idempotency_key":"follow-up","message":"continue with evidence"}`),
	}); err != nil {
		t.Fatal(err)
	}
	dispatch()
	delivered := runtime.prompts[len(runtime.prompts)-1]
	if delivered.pane != targetBinding.PaneID || delivered.message != "continue with evidence" {
		t.Fatalf("cross-provider Follow-up delivery = %+v target=%+v", runtime.prompts, targetBinding)
	}

	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{
		Meta: symphony.CommandMeta{RequestID: "verify"}, WorkID: verification.ID,
		Decision: symphony.VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: "baseline-sha",
	}); err != nil {
		t.Fatal(err)
	}
	dispatch()
	view, _ = engine.Inspect(ctx, symphony.Status{})
	iterations := matrixPendingWorks(t, view, symphony.RoleIteration)
	if len(iterations) != 2 {
		t.Fatalf("Iteration Work count=%d", len(iterations))
	}
	for _, iteration := range iterations {
		session, _, found, err := engine.CurrentAgentSession(ctx, iteration.ID)
		runtimeWork, workErr := engine.RuntimeWork(ctx, iteration.ID)
		if workErr != nil {
			t.Fatal(workErr)
		}
		wantAgent := iterationAgents[runtimeWork.IterationSlotIndex]
		if err != nil || !found || session.AgentKind != wantAgent.Kind {
			t.Fatalf("Iteration Session=%+v found=%v err=%v", session, found, err)
		}
		start := startForSession(t, runtime.starts, session.ID)
		if start.Environment["PIKA_AGENT_MODEL"] != wantAgent.Model {
			t.Fatalf("Iteration slot %d model=%q want=%q", runtimeWork.IterationSlotIndex, start.Environment["PIKA_AGENT_MODEL"], wantAgent.Model)
		}
		wantTabLabel := fmt.Sprintf("Iteration R%d · %.8s", iteration.IterationRound, iteration.AttemptID)
		if !start.DedicatedTab || start.TabLabel != wantTabLabel {
			t.Fatalf("Iteration launch does not request a dedicated tab: %+v", start)
		}
	}
	siblingSession, _, _, _ := engine.CurrentAgentSession(ctx, iterations[1].ID)
	if _, err := engine.Apply(ctx, symphony.FinishIteration{
		Meta: symphony.CommandMeta{RequestID: "candidate"}, WorkID: iterations[0].ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: "candidate-sha", Summary: "faster", Evidence: testcontract.Evidence(),
	}); err != nil {
		t.Fatal(err)
	}
	dispatch()
	assertLastStartKind(t, runtime, agents["integration"].Kind)
	view, _ = engine.Inspect(ctx, symphony.Status{})
	integration := matrixPendingWork(t, view, symphony.RoleIntegration)
	if _, err := engine.Apply(ctx, symphony.BackOff{
		Meta: symphony.CommandMeta{RequestID: "back-off"}, WorkID: integration.ID, Message: "try another layout",
	}); err != nil {
		t.Fatal(err)
	}
	dispatch()
	view, _ = engine.Inspect(ctx, symphony.Status{})
	backedOff := matrixPendingWorkForAttempt(t, view, symphony.RoleIteration, integration.AttemptID)
	backedOffRuntime, err := engine.RuntimeWork(ctx, backedOff.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertLastStartKind(t, runtime, iterationAgents[backedOffRuntime.IterationSlotIndex].Kind)

	currentSibling, _, found, err := engine.CurrentAgentSession(ctx, iterations[1].ID)
	if err != nil || !found || currentSibling.ID != siblingSession.ID {
		t.Fatalf("Back-off changed sibling Session: before=%+v after=%+v found=%v err=%v", siblingSession, currentSibling, found, err)
	}
	recoveryEffect, err := engine.ReplaceLostAgentSession(ctx, currentSibling.ID)
	if err != nil || recoveryEffect == "" {
		t.Fatalf("replace Session effect=%q err=%v", recoveryEffect, err)
	}
	dispatch()
	recovered, _, found, err := engine.CurrentAgentSession(ctx, iterations[1].ID)
	siblingRuntime, runtimeErr := engine.RuntimeWork(ctx, iterations[1].ID)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if err != nil || !found || recovered.ID == currentSibling.ID || recovered.AgentKind != iterationAgents[siblingRuntime.IterationSlotIndex].Kind {
		t.Fatalf("fresh configured-provider recovery=%+v found=%v err=%v", recovered, found, err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	var cancellationTarget symphony.WorkView
	for _, pending := range matrixPendingWorks(t, view, symphony.RoleIteration) {
		if pending.ID != iterations[1].ID {
			cancellationTarget = pending
			break
		}
	}
	if cancellationTarget.ID == "" {
		t.Fatalf("cancellation sibling not found: %+v", view.Works)
	}
	if _, err := engine.Apply(ctx, symphony.CancelWork{Meta: symphony.CommandMeta{RequestID: "cancel-sibling"}, WorkID: cancellationTarget.ID}); err != nil {
		t.Fatal(err)
	}
	dispatch()
	stillRecovered, _, found, err := engine.CurrentAgentSession(ctx, iterations[1].ID)
	if err != nil || !found || stillRecovered.ID != recovered.ID {
		t.Fatalf("sibling cancellation changed recovered Session: before=%+v after=%+v found=%v err=%v", recovered, stillRecovered, found, err)
	}

	closedBeforeShutdown := len(runtime.closed)
	if _, err := engine.Apply(ctx, symphony.RequestShutdown{Meta: symphony.CommandMeta{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	dispatch()
	if len(runtime.closed) != closedBeforeShutdown {
		t.Fatalf("graceful shutdown closed active Agents: before=%d after=%d", closedBeforeShutdown, len(runtime.closed))
	}
	assertLaunchIsolation(t, runtime.starts)
}

func matrixPendingWorkForAttempt(t *testing.T, view symphony.View, role symphony.WorkRole, attemptID string) symphony.WorkView {
	t.Helper()
	for _, work := range matrixPendingWorks(t, view, role) {
		if work.AttemptID == attemptID {
			return work
		}
	}
	t.Fatalf("pending %s Work for Attempt %s not found", role, attemptID)
	return symphony.WorkView{}
}

func matrixAgents(baseline, verification, iteration, integration, followUp string) map[string]configuration.Agent {
	agents := configuration.DefaultAgents()
	for role, kind := range map[string]string{
		"baseline": baseline, "baseline_verify": verification, "iteration": iteration, "integration": integration, "follow_up": followUp,
	} {
		agent := agents[role]
		agent.Kind = kind
		if kind == "cursor" {
			agent.Args = []string{"--force", "--approve-mcps"}
			if agent.ReasoningEffort == "ultra" {
				agent.ReasoningEffort = "max"
			}
		}
		agents[role] = agent
	}
	return agents
}

func writeMatrixExecutable(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func matrixPendingWork(t *testing.T, view symphony.View, role symphony.WorkRole) symphony.WorkView {
	t.Helper()
	works := matrixPendingWorks(t, view, role)
	if len(works) == 0 {
		t.Fatalf("pending %s Work not found: %+v", role, view.Works)
	}
	return works[0]
}

func matrixPendingWorks(_ *testing.T, view symphony.View, role symphony.WorkRole) []symphony.WorkView {
	var works []symphony.WorkView
	for _, work := range view.Works {
		if work.Role == role && work.Status == symphony.WorkPending {
			works = append(works, work)
		}
	}
	return works
}

func assertLastStartKind(t *testing.T, runtime *matrixRuntime, want string) {
	t.Helper()
	if len(runtime.starts) == 0 || runtime.starts[len(runtime.starts)-1].AgentKind != want {
		t.Fatalf("last start=%+v want kind=%s", runtime.starts, want)
	}
}

func assertLaunchIsolation(t *testing.T, starts []workruntime.StartSpec) {
	t.Helper()
	grants := map[string]bool{}
	for _, start := range starts {
		grant := start.Environment["PIKA_MCP_GRANT"]
		if grant == "" || grants[grant] {
			t.Fatalf("missing or reused Session MCP grant in starts=%+v", starts)
		}
		grants[grant] = true
		switch start.AgentKind {
		case "cursor":
			if start.Environment["PIKA_CURSOR_SYSTEM_PROMPT_PATH"] == "" || start.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"] != "" {
				t.Fatalf("Cursor launch layers leaked: %+v", start.Environment)
			}
		case "codex":
			if start.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"] == "" || start.Environment["PIKA_CURSOR_SYSTEM_PROMPT_PATH"] != "" {
				t.Fatalf("Codex launch layers leaked: %+v", start.Environment)
			}
		default:
			t.Fatalf("unexpected provider %q", start.AgentKind)
		}
	}
}

func startForSession(t *testing.T, starts []workruntime.StartSpec, sessionID string) workruntime.StartSpec {
	t.Helper()
	for _, start := range starts {
		if start.Environment["PIKA_SESSION_ID"] == sessionID {
			return start
		}
	}
	t.Fatalf("start for Session %s not found", sessionID)
	return workruntime.StartSpec{}
}

func matrixStopEvent(kind, sessionID, turnID string) json.RawMessage {
	if kind == "cursor" {
		return json.RawMessage(fmt.Sprintf(`{"conversation_id":%q,"generation_id":%q,"hook_event_name":"stop","status":"completed"}`, sessionID, turnID))
	}
	return json.RawMessage(fmt.Sprintf(`{"session_id":%q,"turn_id":%q,"hook_event_name":"Stop"}`, sessionID, turnID))
}
