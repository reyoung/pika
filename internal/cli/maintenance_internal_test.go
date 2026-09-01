package cli

import (
	"context"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/symphony"
)

func TestMaintenanceTargetsTerminalRequiresCompletedFrozenSessionIdentity(t *testing.T) {
	t.Run("completed frozen session", func(t *testing.T) {
		engine, target := maintenanceTargetFixture(t)
		if _, err := engine.Apply(context.Background(), symphony.SubmitBaselineDefinition{
			Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: target.WorkID, Definition: testcontract.Definition(),
		}); err != nil {
			t.Fatal(err)
		}
		terminal, err := (maintenanceDomain{engine: engine}).MaintenanceTargetsTerminal(context.Background(), []maintenance.Target{target})
		if err != nil || !terminal {
			t.Fatalf("completed frozen target: terminal=%v err=%v", terminal, err)
		}
	})

	t.Run("baseline submitted while draining", func(t *testing.T) {
		engine, target := maintenanceTargetFixture(t)
		if _, err := engine.Apply(context.Background(), symphony.RequestShutdown{
			Meta: symphony.CommandMeta{RequestID: "shutdown"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Apply(context.Background(), symphony.SubmitBaselineDefinition{
			Meta: symphony.CommandMeta{RequestID: "submit-draining"}, WorkID: target.WorkID, Definition: testcontract.Definition(),
		}); err != nil {
			t.Fatal(err)
		}
		terminal, err := (maintenanceDomain{engine: engine}).MaintenanceTargetsTerminal(context.Background(), []maintenance.Target{target})
		if err != nil || !terminal {
			t.Fatalf("draining baseline target: terminal=%v err=%v", terminal, err)
		}
	})

	t.Run("cancelled work", func(t *testing.T) {
		engine, target := maintenanceTargetFixture(t)
		if _, err := engine.Apply(context.Background(), symphony.CancelWork{
			Meta: symphony.CommandMeta{RequestID: "cancel"}, WorkID: target.WorkID,
		}); err != nil {
			t.Fatal(err)
		}
		if terminal, err := (maintenanceDomain{engine: engine}).MaintenanceTargetsTerminal(context.Background(), []maintenance.Target{target}); err == nil || terminal {
			t.Fatalf("cancelled target accepted: terminal=%v err=%v", terminal, err)
		}
	})

	t.Run("completed by non-role terminal command", func(t *testing.T) {
		engine, target := maintenanceVerificationTargetFixture(t)
		if _, err := engine.Apply(context.Background(), symphony.BackOff{
			Meta: symphony.CommandMeta{RequestID: "back-off"}, WorkID: target.WorkID, Message: "revise baseline",
		}); err != nil {
			t.Fatal(err)
		}
		if terminal, err := (maintenanceDomain{engine: engine}).MaintenanceTargetsTerminal(context.Background(), []maintenance.Target{target}); err == nil || terminal {
			t.Fatalf("back-off target accepted: terminal=%v err=%v", terminal, err)
		}
	})

	t.Run("replacement session", func(t *testing.T) {
		engine, target := maintenanceTargetFixture(t)
		if err := engine.MarkAgentSessionEnded(context.Background(), target.SessionID, symphony.AgentSessionLost); err != nil {
			t.Fatal(err)
		}
		if err := engine.EnsureAgentSession(context.Background(), symphony.AgentSession{
			ID: "replacement-session", WorkID: target.WorkID, Generation: target.WorkGeneration,
			Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "replacement", Status: symphony.AgentSessionRunning,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Apply(context.Background(), symphony.SubmitBaselineDefinition{
			Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: target.WorkID, Definition: testcontract.Definition(),
		}); err != nil {
			t.Fatal(err)
		}
		if terminal, err := (maintenanceDomain{engine: engine}).MaintenanceTargetsTerminal(context.Background(), []maintenance.Target{target}); err == nil || terminal {
			t.Fatalf("replacement target accepted: terminal=%v err=%v", terminal, err)
		}
	})

	t.Run("Herdr Agent pane drift", func(t *testing.T) {
		engine, target := maintenanceTargetFixture(t)
		if err := engine.ObservePaneMove(context.Background(), target.TerminalID, symphony.PaneBinding{
			WorkspaceID: target.WorkspaceID, TabID: target.TabID, PaneID: "drifted-pane", TerminalID: target.TerminalID,
		}); err != nil {
			t.Fatal(err)
		}
		if err := (maintenanceDomain{engine: engine}).ValidateMaintenanceTargets(context.Background(), []maintenance.Target{target}); err == nil {
			t.Fatal("drifted frozen Herdr Agent identity was accepted")
		}
	})

	t.Run("live Herdr Agent identity drift", func(t *testing.T) {
		engine, target := maintenanceTargetFixture(t)
		name, kind := target.AgentName, target.AgentKind
		domain := maintenanceDomain{
			engine: engine,
			herdrSnapshot: func(context.Context) (herdr.Snapshot, error) {
				return herdr.Snapshot{Agents: []herdr.Agent{{
					Name: &name, Agent: &kind, WorkspaceID: target.WorkspaceID, TabID: target.TabID,
					PaneID: "replacement-pane", TerminalID: target.TerminalID,
				}}}, nil
			},
		}
		if err := domain.ValidateMaintenanceTargets(context.Background(), []maintenance.Target{target}); err == nil {
			t.Fatal("drifted live Herdr Agent identity was accepted")
		}
	})

	t.Run("already retired frozen Agent may be absent during resume replay", func(t *testing.T) {
		engine, target := maintenanceTargetFixture(t)
		if err := engine.MarkAgentSessionEnded(context.Background(), target.SessionID, symphony.AgentSessionExited); err != nil {
			t.Fatal(err)
		}
		domain := maintenanceDomain{
			engine: engine,
			herdrSnapshot: func(context.Context) (herdr.Snapshot, error) {
				return herdr.Snapshot{}, nil
			},
		}
		if err := domain.ValidateMaintenanceTargets(context.Background(), []maintenance.Target{target}); err != nil {
			t.Fatalf("retired resume replay target rejected: %v", err)
		}
	})
}

func TestMaintenanceRecoveryValidatesExactDurableLiveAgentSet(t *testing.T) {
	ctx := context.Background()
	engine, frozen := maintenanceTargetFixture(t)
	frozenAgent := maintenanceHerdrAgent(frozen)
	status := maintenance.Status{
		State: maintenance.StateHolding, ResumeRequestID: "resume-1",
		Targets: []maintenance.Target{frozen},
	}
	domain := func(agents ...herdr.Agent) maintenanceDomain {
		return maintenanceDomain{
			engine: engine,
			herdrSnapshot: func(context.Context) (herdr.Snapshot, error) {
				return herdr.Snapshot{Agents: agents}, nil
			},
		}
	}
	if err := domain(frozenAgent).ValidateMaintenanceRecovery(ctx, status); err != nil {
		t.Fatalf("intent-persisted frozen Agent rejected: %v", err)
	}
	if err := domain().ValidateMaintenanceRecovery(ctx, status); err == nil {
		t.Fatal("intent-persisted missing frozen Agent was accepted")
	}
	drifted := frozenAgent
	drifted.PaneID = "drifted-pane"
	if err := domain(drifted).ValidateMaintenanceRecovery(ctx, status); err == nil {
		t.Fatal("intent-persisted drifted frozen Agent was accepted")
	}
	extraName, extraKind := "pika-extra", "codex"
	extra := herdr.Agent{
		Name: &extraName, Agent: &extraKind, WorkspaceID: frozen.WorkspaceID, TabID: frozen.TabID,
		PaneID: "extra-pane", TerminalID: "extra-terminal",
	}
	if err := domain(frozenAgent, extra).ValidateMaintenanceRecovery(ctx, status); err == nil {
		t.Fatal("intent-persisted extra live Agent was accepted")
	}

	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "complete-frozen"}, WorkID: frozen.WorkID, Definition: testcontract.Definition(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.MarkAgentSessionEnded(ctx, frozen.SessionID, symphony.AgentSessionExited); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	var successorWork symphony.WorkView
	for _, work := range view.Works {
		if work.Status == symphony.WorkPending {
			successorWork = work
		}
	}
	successor := symphony.AgentSession{
		ID: "successor-session", WorkID: successorWork.ID, Generation: successorWork.Generation,
		Role: successorWork.Role, AgentKind: "codex", AgentName: "pika-successor", Status: symphony.AgentSessionRunning,
	}
	if err := engine.EnsureAgentSession(ctx, successor); err != nil {
		t.Fatal(err)
	}
	successorBinding := symphony.PaneBinding{
		WorkspaceID: frozen.WorkspaceID, TabID: frozen.TabID,
		PaneID: "successor-pane", TerminalID: "successor-terminal",
	}
	if err := engine.BindPane(ctx, successor.ID, successorBinding); err != nil {
		t.Fatal(err)
	}
	successorTarget := maintenanceTarget(successor, successorBinding)
	successorAgent := maintenanceHerdrAgent(successorTarget)
	if err := domain(successorAgent).ValidateMaintenanceRecovery(ctx, status); err != nil {
		t.Fatalf("runtime-started successor Agent rejected: %v", err)
	}
	if err := domain().ValidateMaintenanceRecovery(ctx, status); err == nil {
		t.Fatal("runtime-started missing successor Agent was accepted")
	}
	if err := domain(successorAgent, extra).ValidateMaintenanceRecovery(ctx, status); err == nil {
		t.Fatal("runtime-started extra live Agent was accepted")
	}
}

func TestActiveMaintenanceTargetsRequireMatchingLiveHerdrAgent(t *testing.T) {
	ctx := context.Background()
	engine, target := maintenanceTargetFixture(t)
	domain := maintenanceDomain{
		engine: engine,
		herdrSnapshot: func(context.Context) (herdr.Snapshot, error) {
			return herdr.Snapshot{}, nil
		},
	}
	if _, err := domain.ActiveMaintenanceTargets(ctx); err == nil {
		t.Fatal("prepare accepted an active durable Session without its live Herdr Agent")
	}
	domain.herdrSnapshot = func(context.Context) (herdr.Snapshot, error) {
		return herdr.Snapshot{Agents: []herdr.Agent{maintenanceHerdrAgent(target)}}, nil
	}
	targets, err := domain.ActiveMaintenanceTargets(ctx)
	if err != nil || len(targets) != 1 || targets[0] != target {
		t.Fatalf("matching active target: targets=%+v err=%v", targets, err)
	}
}

func maintenanceHerdrAgent(target maintenance.Target) herdr.Agent {
	name, kind := target.AgentName, target.AgentKind
	return herdr.Agent{
		Name: &name, Agent: &kind, WorkspaceID: target.WorkspaceID, TabID: target.TabID,
		PaneID: target.PaneID, TerminalID: target.TerminalID,
	}
}

func maintenanceVerificationTargetFixture(t *testing.T) (*symphony.Engine, maintenance.Target) {
	t.Helper()
	engine, err := symphony.Open(context.Background(), t.TempDir()+"/pika.db", symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(context.Background(), symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(context.Background(), symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(context.Background(), symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition(),
	}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(context.Background(), symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	work := view.Works[len(view.Works)-1]
	session := symphony.AgentSession{
		ID: "frozen-verification", WorkID: work.ID, Generation: work.Generation,
		Role: work.Role, AgentKind: "codex", AgentName: "frozen", Status: symphony.AgentSessionRunning,
	}
	if err := engine.EnsureAgentSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding := symphony.PaneBinding{WorkspaceID: "workspace", TabID: "tab", PaneID: "verification-pane", TerminalID: "verification-terminal"}
	if err := engine.BindPane(context.Background(), session.ID, binding); err != nil {
		t.Fatal(err)
	}
	return engine, maintenanceTarget(session, binding)
}

func maintenanceTargetFixture(t *testing.T) (*symphony.Engine, maintenance.Target) {
	t.Helper()
	engine, err := symphony.Open(context.Background(), t.TempDir()+"/pika.db", symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(context.Background(), symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(context.Background(), symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	work := view.Works[0]
	session := symphony.AgentSession{
		ID: "frozen-session", WorkID: work.ID, Generation: work.Generation,
		Role: work.Role, AgentKind: "codex", AgentName: "frozen", Status: symphony.AgentSessionRunning,
	}
	if err := engine.EnsureAgentSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding := symphony.PaneBinding{WorkspaceID: "workspace", TabID: "tab", PaneID: "draft-pane", TerminalID: "draft-terminal"}
	if err := engine.BindPane(context.Background(), session.ID, binding); err != nil {
		t.Fatal(err)
	}
	return engine, maintenanceTarget(session, binding)
}

func maintenanceTarget(session symphony.AgentSession, binding symphony.PaneBinding) maintenance.Target {
	return maintenance.Target{
		WorkID: session.WorkID, SessionID: session.ID, WorkGeneration: session.Generation,
		AgentName: session.AgentName, AgentKind: session.AgentKind,
		WorkspaceID: binding.WorkspaceID, TabID: binding.TabID, PaneID: binding.PaneID, TerminalID: binding.TerminalID,
	}
}
