package workruntime_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
)

type realProviderSpec struct {
	name       string
	agents     map[string]configuration.Agent
	modelEnv   string
	keepEnv    string
	requestTag string
}

func TestRealCodexCompletesDisposableOptimization(t *testing.T) {
	if os.Getenv("PIKA_GO_REAL_CODEX_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_REAL_CODEX_INTEGRATION=1 to run the quota-consuming real Codex smoke")
	}
	runRealProviderOptimization(t, realProviderSpec{name: "codex", agents: configuration.DefaultAgents(), modelEnv: "PIKA_GO_REAL_CODEX_MODEL", keepEnv: "PIKA_GO_REAL_CODEX_KEEP", requestTag: "real-codex"})
}

func TestRealCursorCompletesDisposableOptimization(t *testing.T) {
	if os.Getenv("PIKA_GO_REAL_CURSOR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_REAL_CURSOR_INTEGRATION=1 to run the quota-consuming real Cursor matrix")
	}
	runRealProviderOptimization(t, realProviderSpec{name: "cursor", agents: matrixAgents("cursor", "cursor", "cursor", "cursor", "cursor"), modelEnv: "PIKA_GO_REAL_CURSOR_MODEL", keepEnv: "PIKA_GO_REAL_CURSOR_KEEP", requestTag: "real-cursor"})
}

func TestRealMixedProviderCompletesBothAlternatingMatrices(t *testing.T) {
	if os.Getenv("PIKA_GO_REAL_MIXED_PROVIDER_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_REAL_MIXED_PROVIDER_INTEGRATION=1 to run the quota-consuming mixed matrices")
	}
	for _, spec := range []realProviderSpec{
		{name: "cursor-codex-cursor-codex-cursor", agents: matrixAgents("cursor", "codex", "cursor", "codex", "cursor"), modelEnv: "PIKA_GO_REAL_MIXED_MODEL", keepEnv: "PIKA_GO_REAL_MIXED_KEEP", requestTag: "real-mixed-forward"},
		{name: "codex-cursor-codex-cursor-codex", agents: matrixAgents("codex", "cursor", "codex", "cursor", "codex"), modelEnv: "PIKA_GO_REAL_MIXED_MODEL", keepEnv: "PIKA_GO_REAL_MIXED_KEEP", requestTag: "real-mixed-reverse"},
	} {
		t.Run(spec.name, func(t *testing.T) { runRealProviderOptimization(t, spec) })
	}
}

// runRealProviderOptimization is deliberately opt-in: it starts real model
// sessions and consumes quota. Fake-Agent matrices remain deterministic CI.
func runRealProviderOptimization(t *testing.T, spec realProviderSpec) {
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	codexBinary := "/bin/echo"
	if hasAgentKind(spec.agents, "codex") {
		codexBinary, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}
	cursorBinary := ""
	if hasAgentKind(spec.agents, "cursor") {
		cursorBinary, err = exec.LookPath("cursor-agent")
		if err != nil {
			t.Fatal(err)
		}
	}
	pikaBinary := os.Getenv("PIKA_GO_BIN")
	if pikaBinary == "" || !filepath.IsAbs(pikaBinary) {
		t.Fatal("PIKA_GO_BIN must be the absolute path to the pika-go test binary")
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(codexHome) {
		t.Fatalf("CODEX_HOME must be absolute: %s", codexHome)
	}

	root, err := os.MkdirTemp("/tmp", "pika-go-real-"+spec.name+"-")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv(spec.keepEnv) == "1" {
		t.Logf("preserving real-provider fixture at %s", root)
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	repository := filepath.Join(root, "repository")
	initializeRealCodexFixture(t, repository)
	followUpHoldPath := createRealCodexHold(t, root, "verification")
	iterationHoldPath := createRealCodexHold(t, root, "iteration")
	integrationHoldPath := createRealCodexHold(t, root, "integration")
	if hasAgentKind(spec.agents, "codex") {
		restoreTrust := trustRealCodexFixture(t, codexHome, repository)
		t.Cleanup(restoreTrust)
	}

	herdrConfig, herdrState := configureRealCodexHerdr(t, root)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig),
		"XDG_STATE_HOME":  herdrState,
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopServer()
		_ = server.Wait()
	})
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)

	testCtx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(testCtx, "workspace.create", map[string]any{
		"cwd": repository, "label": "pika-real-" + spec.name, "focus": false,
	}, &created); err != nil {
		t.Fatalf("create isolated Herdr workspace: %v; output=%s", err, serverOutput.String())
	}
	t.Cleanup(func() {
		_ = client.Call(context.Background(), "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
	})
	initPane, err := herdr.NewRuntime(client).SplitPane(testCtx, created.RootPane.PaneID, "down", repository)
	if err != nil {
		t.Fatal(err)
	}

	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot := filepath.Join(root, "pika-state")
	configRoot := filepath.Join(root, "pika-config")
	model := os.Getenv(spec.modelEnv)
	if model == "" {
		model = "gpt-5.6-luna"
	}
	profileRollback := configureRealProviderInstance(t, testCtx, repository, stateRoot, configRoot, codexHome, pikaBinary, codexBinary, model, spec.agents)
	t.Cleanup(func() {
		if err := profileRollback(); err != nil {
			t.Errorf("restore pre-smoke Codex profile: %v", err)
		}
	})

	daemon := startRealCodexDaemon(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, codexHome, codexBinary, cursorBinary)
	t.Cleanup(func() {
		if daemon != nil {
			daemon.killAndWait()
		}
	})
	if _, err := control.Init(testCtx, pikaSocket, protocol.InitRequest{
		Mutation:     protocol.Mutation{RequestID: spec.requestTag + "-init"},
		Repository:   repository,
		CallerPaneID: initPane.PaneID,
	}); err != nil {
		t.Fatalf("initialize disposable Optimization: %v", err)
	}
	followUpTarget := exerciseRealCodexFollowUp(t, testCtx, stateRoot, client, pikaSocket, daemon, &serverOutput)
	crashedOutput := crashRealCodexDaemon(t, daemon)
	daemon = nil
	daemon = startRealCodexDaemon(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, codexHome, codexBinary, cursorBinary)
	waitForRealCodexReplacement(t, testCtx, stateRoot, followUpTarget, client, pikaSocket, daemon, &serverOutput)
	releaseRealCodexHold(t, followUpHoldPath)
	t.Logf("real Codex Verification recovered through a fresh Session after daemon crash; crashed daemon output bytes=%d", len(crashedOutput))

	exerciseRealCodexCancellation(t, testCtx, stateRoot, iterationHoldPath, client, pikaSocket, daemon, &serverOutput)
	exerciseRealCodexIntegrationBackOff(t, testCtx, stateRoot, integrationHoldPath, client, pikaSocket, daemon, &serverOutput)

	view := waitForRealCodexBest(t, testCtx, client, pikaSocket, daemon, &serverOutput)
	if view.Baseline == nil || view.Baseline.Status != symphony.BaselineAccepted {
		t.Fatalf("real Codex did not accept Baseline: %+v", view.Baseline)
	}
	acceptedIntegrations := 0
	for _, integration := range view.Integrations {
		if integration.Status == "accepted" {
			acceptedIntegrations++
		}
	}
	if acceptedIntegrations == 0 || view.Best == nil || view.Best.Sequence < 1 {
		t.Fatalf("real Codex did not advance Best: best=%+v integrations=%+v", view.Best, view.Integrations)
	}
	if !hasCancelledAttempt(view) || !hasIntegrationBackOff(view, "real release-matrix back-off") {
		t.Fatalf("real Codex release matrix omitted cancellation or Back-off: attempts=%+v rounds=%+v", view.Attempts, view.IterationRounds)
	}
	if view.Storage.ProviderEventBytes == 0 || view.Storage.ToolPayloadBytes == 0 {
		t.Fatalf("Codex hooks did not persist observable journal payloads: %+v", view.Storage)
	}
	if output, err := exec.Command("git", "-C", repository, "rev-parse", "refs/heads/pika/best").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != view.Best.CommitSHA {
		t.Fatalf("Git/SQLite Best mismatch: git=%q err=%v db=%+v", output, err, view.Best)
	}

	if _, err := control.Shutdown(testCtx, pikaSocket, protocol.ShutdownRequest{Mutation: protocol.Mutation{RequestID: spec.requestTag + "-shutdown"}}); err != nil {
		t.Fatalf("request graceful shutdown: %v", err)
	}
	cancelPendingRealCodexWork(t, testCtx, pikaSocket)
	select {
	case err := <-daemon.done:
		if err != nil {
			t.Fatalf("daemon did not drain cleanly: %v; output=%s", err, daemon.output.String())
		}
		daemon = nil
	case <-time.After(90 * time.Second):
		t.Fatalf("daemon did not finish draining: %s", diagnoseRealCodex(testCtx, client, pikaSocket, daemon, &serverOutput))
	}

	engine, err := symphony.Open(testCtx, filepath.Join(stateRoot, "instances", "real-codex", "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatalf("reopen final database: %v", err)
	}
	defer engine.Close()
	toolEvents := 0
	providerSessionIDs := map[string]struct{}{}
	for _, work := range view.Works {
		journal, err := engine.ConversationJournal(testCtx, work.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range journal.Events {
			if event.ProviderSessionID != "" {
				providerSessionIDs[event.ProviderSessionID] = struct{}{}
			}
		}
		toolEvents += len(journal.Tools)
	}
	if len(providerSessionIDs) < 8 || toolEvents == 0 {
		t.Fatalf("incomplete real-Codex journal: provider_sessions=%d tool_events=%d", len(providerSessionIDs), toolEvents)
	}
	t.Logf("real Codex release matrix advanced Best to sequence %d with %d provider sessions and %d observable tool events", view.Best.Sequence, len(providerSessionIDs), toolEvents)
}

func configureRealCodexHerdr(t *testing.T, root string) (string, string) {
	t.Helper()
	configHome := filepath.Join(root, "herdr-config-home")
	configDir := filepath.Join(configHome, "herdr")
	stateHome := filepath.Join(root, "herdr-state")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[update]\nversion_check = false\nmanifest_check = false\n\n[session]\nresume_agents_on_restore = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_CONFIG_PATH", configPath)
	return configDir, stateHome
}

func hasAgentKind(agents map[string]configuration.Agent, kind string) bool {
	for _, agent := range agents {
		if agent.Kind == kind {
			return true
		}
	}
	return false
}

func configureRealProviderInstance(t *testing.T, ctx context.Context, repository, stateRoot, configRoot, codexHome, pikaBinary, codexBinary, model string, agents map[string]configuration.Agent) func() error {
	t.Helper()
	configuredAgents := make(map[string]configuration.Agent, len(agents))
	for role, agent := range agents {
		agent.Model = model
		agent.ReasoningEffort = "medium"
		if agent.Kind == "cursor" {
			agent.Args = []string{"--force", "--approve-mcps", "--trust"}
		}
		configuredAgents[role] = agent
	}
	configured, err := configuration.RenderConfiguration(repository, configuredAgents)
	if err != nil {
		t.Fatal(err)
	}
	configured = strings.Replace(configured, "iteration_concurrency = 4", "iteration_concurrency = 2", 1)
	configured = strings.Replace(configured, "max_pending_attempts = 8", "max_pending_attempts = 2", 1)
	configured = strings.Replace(configured, "pane_idle_timeout = \"5m\"", "pane_idle_timeout = \"10m\"", 1)
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: stateRoot, InstanceID: "real-codex",
		CodexHome: codexHome, CursorMCPPath: realCursorMCPPath(t), CursorHooksPath: realCursorHooksPath(t), PikaExecutable: pikaBinary, CodexExecutable: codexBinary, ConfigurationTOML: &configured,
	}
	rollback, err := initializer.Prepare(ctx, repository)
	if err != nil {
		t.Fatalf("prepare real-provider instance: %v", err)
	}
	fixtureRoot := filepath.Dir(repository)
	gateScript := "#!/bin/sh\n" +
		"set -eu\n" +
		"case \"${1:-}\" in verification|iteration|integration) ;; *) echo 'usage: pika-real-gate <verification|iteration|integration>' >&2; exit 2 ;; esac\n" +
		"hold=" + shellTestQuote(fixtureRoot) + "-\"$1\"-hold\n" +
		"ready=" + shellTestQuote(fixtureRoot) + "-\"$1\"-ready\n" +
		"touch \"$ready\"\n" +
		"while [ -e \"$hold\" ]; do sleep 0.1; done\n"
	gatePath := filepath.Join(stateRoot, "instances", "real-codex", "runtime", "bin", "pika-real-gate")
	if err := os.MkdirAll(filepath.Dir(gatePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gatePath, []byte(gateScript), 0o700); err != nil {
		t.Fatal(err)
	}
	instructionRoot := filepath.Join(configRoot, "instances", "real-codex", "instructions")
	roleInstructions := map[string]string{
		"baseline-verify.md": "At the beginning of this Session, immediately after fully reading the Context Bundle and before any other tool or shell work, run `pika-real-gate verification` and wait until it exits successfully. This is a test-instance synchronization requirement.\n",
		"iteration.md":       "At the beginning of this Session, immediately after fully reading the Context Bundle and before any other tool or shell work, run `pika-real-gate iteration` and wait until it exits successfully. This is a test-instance synchronization requirement.\n",
		"integration.md":     "At the beginning of this Session, immediately after fully reading the Context Bundle and before any other tool or shell work, run `pika-real-gate integration` and wait until it exits successfully. This is a test-instance synchronization requirement.\n",
	}
	for name, instruction := range roleInstructions {
		if err := os.WriteFile(filepath.Join(instructionRoot, name), []byte(instruction), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return rollback
}

func realCursorMCPPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".cursor", "mcp.json")
}

func realCursorHooksPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".cursor", "hooks.json")
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func startRealCodexDaemon(t *testing.T, pikaBinary, socketPath, stateRoot, configRoot, herdrSocket, symphonyPane, codexHome, codexBinary, cursorBinary string) *daemonProcess {
	t.Helper()
	process := &daemonProcess{done: make(chan error, 1)}
	process.command = exec.Command(pikaBinary, "daemon",
		"--socket", socketPath,
		"--state-dir", stateRoot,
		"--config-dir", configRoot,
		"--instance", "real-codex",
	)
	process.command.Env = replacedEnvironment(map[string]string{
		"HERDR_SOCKET_PATH":            herdrSocket,
		"HERDR_CONFIG_PATH":            filepath.Join(filepath.Dir(herdrSocket), "config.toml"),
		"HERDR_PANE_ID":                symphonyPane,
		"HERDR_ENV":                    "1",
		"CODEX_HOME":                   codexHome,
		"PIKA_GO_CODEX_EXECUTABLE":     codexBinary,
		"PIKA_GO_CURSOR_EXECUTABLE":    cursorBinary,
		"PIKA_CODEX_BYPASS_HOOK_TRUST": "1",
		"PIKA_GO_FOLLOWUP_INACTIVITY":  "500ms",
	})
	process.command.Stdout, process.command.Stderr = &process.output, &process.output
	if err := process.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { process.done <- process.command.Wait() }()
	waitForPikaProcessHealth(t, socketPath, process)
	return process
}

type realCodexSessionObservation struct {
	Work              symphony.WorkView
	Session           symphony.AgentSession
	ProviderSessionID string
}

func exerciseRealCodexFollowUp(t *testing.T, ctx context.Context, stateRoot string, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) realCodexSessionObservation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var target symphony.WorkView
	for time.Now().Before(deadline) {
		view, err := control.Status(ctx, socketPath)
		if err == nil {
			for _, work := range view.Works {
				if work.Role == symphony.RoleBaselineVerification && work.Status == symphony.WorkPending {
					target = work
					break
				}
			}
		}
		if target.ID != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if target.ID == "" {
		t.Fatalf("real Codex Verification did not start before Follow-up smoke: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
	}

	engine, err := symphony.Open(ctx, filepath.Join(stateRoot, "instances", "real-codex", "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	var targetSession symphony.AgentSession
	providerSessionID := ""
	for time.Now().Before(deadline) {
		var found bool
		targetSession, _, found, err = engine.CurrentAgentSession(ctx, target.ID)
		if err == nil && found && targetSession.Status == symphony.AgentSessionRunning {
			journal, journalErr := engine.ConversationJournal(ctx, target.ID)
			if journalErr == nil && journal.ProviderSessionID != "" {
				providerSessionID = journal.ProviderSessionID
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if targetSession.ID == "" || providerSessionID == "" {
		t.Fatalf("real Codex Verification provider Session did not bind before Follow-up smoke: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
	}
	readyPath := filepath.Dir(stateRoot) + "-verification-ready"
	readyDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(readyDeadline) {
		if _, statErr := os.Stat(readyPath); statErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("real Codex Verification did not become quiescent before Follow-up smoke: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
	}
	hook := matrixStopEvent(targetSession.AgentKind, providerSessionID, "pika-real-follow-up-stopped-turn")
	if err := control.IngestProviderEvent(ctx, socketPath, targetSession.AgentKind, protocol.ProviderEventRequest{AgentSessionID: targetSession.ID, Event: hook}); err != nil {
		t.Fatalf("inject stopped-turn observation for real Follow-up smoke: %v", err)
	}

	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		view, statusErr := control.Status(ctx, socketPath)
		if statusErr == nil {
			for _, followUp := range view.FollowUps {
				if followUp.TargetWorkID == target.ID && followUp.Status == "delivered" && followUp.Message != "" {
					for _, work := range view.Works {
						if work.ID == target.ID && work.Status != symphony.WorkPending {
							t.Fatalf("real Follow-up completed target Work: %+v", work)
						}
					}
					t.Logf("real Codex Follow-up request %s delivered to pending Verification Work %s", followUp.ID, target.ID)
					return realCodexSessionObservation{Work: target, Session: targetSession, ProviderSessionID: providerSessionID}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("real Codex Follow-up was not delivered: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
	return realCodexSessionObservation{}
}

func crashRealCodexDaemon(t *testing.T, process *daemonProcess) string {
	t.Helper()
	if process == nil || process.command == nil || process.command.Process == nil {
		t.Fatal("real Codex daemon process is unavailable for crash injection")
	}
	if err := process.command.Process.Kill(); err != nil {
		t.Fatalf("crash real Codex daemon: %v", err)
	}
	select {
	case <-process.done:
		return process.output.String()
	case <-time.After(10 * time.Second):
		t.Fatal("crashed real Codex daemon did not exit")
	}
	return ""
}

func waitForRealCodexReplacement(t *testing.T, ctx context.Context, stateRoot string, previous realCodexSessionObservation, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) realCodexSessionObservation {
	t.Helper()
	engine, err := symphony.Open(ctx, filepath.Join(stateRoot, "instances", "real-codex", "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		view, statusErr := control.Status(ctx, socketPath)
		if statusErr == nil {
			for _, work := range view.Works {
				if work.ID != previous.Work.ID {
					continue
				}
				history, historyErr := engine.AgentSessionHistory(ctx, work.ID)
				if historyErr != nil || len(history) < 2 {
					continue
				}
				journal, journalErr := engine.ConversationJournal(ctx, work.ID)
				if journalErr != nil {
					continue
				}
				providerIDs := map[string]struct{}{}
				for _, event := range journal.Events {
					if event.ProviderSessionID != "" {
						providerIDs[event.ProviderSessionID] = struct{}{}
					}
				}
				latest := history[len(history)-1]
				if history[0].ID == previous.Session.ID && history[0].Status == symphony.AgentSessionLost && latest.ID != previous.Session.ID && latest.Generation == previous.Session.Generation && len(providerIDs) >= 2 {
					for providerID := range providerIDs {
						if providerID != previous.ProviderSessionID {
							return realCodexSessionObservation{Work: work, Session: latest, ProviderSessionID: providerID}
						}
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("real Codex daemon recovery did not create a fresh provider Session: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
	return realCodexSessionObservation{}
}

func exerciseRealCodexCancellation(t *testing.T, ctx context.Context, stateRoot, holdPath string, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) {
	t.Helper()
	sessions := waitForRealCodexRoleSessions(t, ctx, stateRoot, symphony.RoleIteration, 2, client, socketPath, daemon, serverOutput)
	if sessions[0].Work.AttemptID == "" || sessions[0].Work.AttemptID == sessions[1].Work.AttemptID {
		t.Fatalf("parallel real Iteration Sessions do not belong to distinct Attempts: %+v", sessions)
	}
	if _, err := control.CancelWork(ctx, socketPath, sessions[0].Work.ID, protocol.CancelWorkRequest{Mutation: protocol.Mutation{RequestID: "real-release-cancel-" + sessions[0].Work.ID}}); err != nil {
		t.Fatalf("cancel one real Iteration sibling: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		view, err := control.Status(ctx, socketPath)
		if err == nil {
			cancelled, siblingPending, attemptCancelled := false, false, false
			for _, work := range view.Works {
				cancelled = cancelled || work.ID == sessions[0].Work.ID && work.Status == symphony.WorkCancelled
				siblingPending = siblingPending || work.ID == sessions[1].Work.ID && work.Status == symphony.WorkPending
			}
			for _, attempt := range view.Attempts {
				attemptCancelled = attemptCancelled || attempt.ID == sessions[0].Work.AttemptID && attempt.Status == "cancelled"
			}
			if cancelled && siblingPending && attemptCancelled {
				releaseRealCodexHold(t, holdPath)
				t.Logf("cancelled real Iteration Work %s while sibling %s remained pending", sessions[0].Work.ID, sessions[1].Work.ID)
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real Iteration cancellation did not preserve its sibling: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
}

func exerciseRealCodexIntegrationBackOff(t *testing.T, ctx context.Context, stateRoot, holdPath string, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) {
	t.Helper()
	target := waitForRealCodexRoleSessions(t, ctx, stateRoot, symphony.RoleIntegration, 1, client, socketPath, daemon, serverOutput)[0]
	const message = "real release-matrix back-off"
	if _, err := control.BackOff(ctx, socketPath, protocol.BackOffRequest{
		Mutation: protocol.Mutation{RequestID: "real-release-back-off-" + target.Work.ID}, WorkID: target.Work.ID, Message: message,
	}); err != nil {
		t.Fatalf("Back-off real Integration: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		view, err := control.Status(ctx, socketPath)
		if err == nil {
			backedOff, successor := false, false
			for _, integration := range view.Integrations {
				backedOff = backedOff || integration.ID == target.Work.IntegrationID && integration.Status == "backed_off"
			}
			for _, work := range view.Works {
				if work.Role == symphony.RoleIteration && work.AttemptID == target.Work.AttemptID && work.IterationRound == target.Work.IterationRound+1 && work.Status == symphony.WorkPending {
					successor = true
				}
			}
			if backedOff && successor && hasIntegrationBackOff(view, message) {
				releaseRealCodexHold(t, holdPath)
				t.Logf("backed off real Integration Work %s to a fresh Iteration round", target.Work.ID)
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real Integration Back-off did not create its successor Iteration: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
}

func waitForRealCodexRoleSessions(t *testing.T, ctx context.Context, stateRoot string, role symphony.WorkRole, count int, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) []realCodexSessionObservation {
	t.Helper()
	engine, err := symphony.Open(ctx, filepath.Join(stateRoot, "instances", "real-codex", "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		view, statusErr := control.Status(ctx, socketPath)
		if statusErr == nil {
			observations := make([]realCodexSessionObservation, 0, count)
			for _, work := range view.Works {
				if work.Role != role || work.Status != symphony.WorkPending {
					continue
				}
				session, _, found, currentErr := engine.CurrentAgentSession(ctx, work.ID)
				if currentErr != nil || !found || session.Status != symphony.AgentSessionRunning {
					continue
				}
				journal, journalErr := engine.ConversationJournal(ctx, work.ID)
				if journalErr != nil {
					continue
				}
				providerSessionID := ""
				for _, event := range journal.Events {
					if event.ProviderSessionID != "" {
						providerSessionID = event.ProviderSessionID
					}
				}
				if providerSessionID != "" {
					observations = append(observations, realCodexSessionObservation{Work: work, Session: session, ProviderSessionID: providerSessionID})
				}
			}
			if len(observations) >= count {
				return observations[:count]
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("real Codex did not start %d %s provider Sessions: %s", count, role, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
	return nil
}

func createRealCodexHold(t *testing.T, root, kind string) string {
	t.Helper()
	path := root + "-" + kind + "-hold"
	if err := os.WriteFile(path, []byte("hold "+kind+" for the real release matrix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(path)
		_ = os.Remove(root + "-" + kind + "-ready")
	})
	return path
}

func releaseRealCodexHold(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("release real Codex hold %s: %v", filepath.Base(path), err)
	}
}

func hasCancelledAttempt(view symphony.View) bool {
	for _, attempt := range view.Attempts {
		if attempt.Status == "cancelled" {
			return true
		}
	}
	return false
}

func hasIntegrationBackOff(view symphony.View, message string) bool {
	for _, round := range view.IterationRounds {
		if round.Kind == "user_back_off" && round.BackOffMessage == message {
			return true
		}
	}
	return false
}

func trustRealCodexFixture(t *testing.T, codexHome, repository string) func() {
	t.Helper()
	configPath := filepath.Join(codexHome, "config.toml")
	before, err := os.ReadFile(configPath)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf("\n[projects.%q]\ntrust_level = \"trusted\"\n", resolved)
	if strings.Contains(string(before), "[projects."+fmt.Sprintf("%q", resolved)+"]") {
		return func() {}
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(append([]byte(nil), before...), []byte(entry)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if existed {
			if err := os.WriteFile(configPath, before, 0o600); err != nil {
				t.Errorf("restore Codex config after real smoke: %v", err)
			}
			return
		}
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove real-smoke Codex config: %v", err)
		}
	}
}

func waitForRealCodexBest(t *testing.T, ctx context.Context, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) symphony.View {
	t.Helper()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastMilestone := ""
	for {
		view, err := control.Status(ctx, socketPath)
		if err == nil {
			baseline := "none"
			if view.Baseline != nil {
				baseline = fmt.Sprintf("%d/%s", view.Baseline.Number, view.Baseline.Status)
			}
			best := "none"
			if view.Best != nil {
				best = fmt.Sprintf("%d/%s", view.Best.Sequence, view.Best.CommitSHA)
			}
			milestone := fmt.Sprintf("status=%s baseline=%s works=%d integrations=%d best=%s", view.Optimization.Status, baseline, len(view.Works), len(view.Integrations), best)
			if milestone != lastMilestone {
				t.Log(milestone)
				lastMilestone = milestone
			}
			if view.Best != nil && view.Best.Sequence >= 1 {
				return view
			}
			if view.Baseline != nil && view.Baseline.Number > 3 {
				t.Fatalf("disposable fixture exceeded three Baseline revisions: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
			}
		}
		select {
		case err := <-daemon.done:
			t.Fatalf("daemon exited before Best advanced: %v; %s", err, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
		case <-ctx.Done():
			t.Fatalf("real Codex smoke timed out: %s", diagnoseRealCodex(context.Background(), client, socketPath, daemon, serverOutput))
		case <-ticker.C:
		}
	}
}

func cancelPendingRealCodexWork(t *testing.T, ctx context.Context, socketPath string) {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		view, err := control.Status(ctx, socketPath)
		if err != nil {
			return
		}
		var pending []string
		for _, work := range view.Works {
			if work.Status == symphony.WorkPending && (work.Role == symphony.RoleIteration || work.Role == symphony.RoleIntegration) {
				pending = append(pending, work.ID)
			}
		}
		if len(pending) == 0 {
			return
		}
		for _, workID := range pending {
			_, _ = control.CancelWork(ctx, socketPath, workID, protocol.CancelWorkRequest{Mutation: protocol.Mutation{RequestID: "real-codex-cancel-" + workID}})
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("pending real-Codex Work did not settle during drain")
}

func diagnoseRealCodex(ctx context.Context, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) string {
	var report strings.Builder
	if view, err := control.Status(ctx, socketPath); err == nil {
		_, _ = fmt.Fprintf(&report, "view=%+v\n", view)
	} else {
		_, _ = fmt.Fprintf(&report, "status_error=%v\n", err)
	}
	if snapshot, err := client.Snapshot(ctx); err == nil {
		_, _ = fmt.Fprintf(&report, "snapshot=%+v\n", snapshot)
		for _, pane := range snapshot.Panes {
			var read struct {
				Text string `json:"text"`
			}
			_ = client.Call(ctx, "pane.read", map[string]any{"pane_id": pane.PaneID, "source": "recent-unwrapped", "lines": 160, "format": "text"}, &read)
			_, _ = fmt.Fprintf(&report, "pane %s:\n%s\n", pane.PaneID, read.Text)
		}
	}
	if daemon != nil {
		_, _ = fmt.Fprintf(&report, "daemon=%s\n", daemon.output.String())
	}
	_, _ = fmt.Fprintf(&report, "herdr=%s", serverOutput.String())
	return report.String()
}

func initializeRealCodexFixture(t *testing.T, repository string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main", repository},
		{"-C", repository, "config", "user.name", "Pika Real Codex Smoke"},
		{"-C", repository, "config", "user.email", "pika-smoke@example.invalid"},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	files := map[string]string{
		".gitignore": "__pycache__/\n*.py[cod]\n",
		"README.md": `# Disposable Pika optimization fixture

This repository is a fully specified, non-interactive smoke fixture. Do not ask the user for missing requirements.

Optimize candidate.sum_to_n. The development baseline is the initial Git commit. The correctness oracle is the closed-form expression in harness.py; Target and Candidate must never call that oracle. The full case set is exactly small (n=1000, weight 1) and large (n=10000, weight 4); large is critical.

Canonical commands:

    python3 harness.py correctness --cases small large
    python3 harness.py benchmark --cases small large

Correctness is exact integer equality for both cases. The primary metric is logical_operations, lower is better, reported independently for every case by the real Candidate implementation. There are no warmups or stochastic repeats because the metric is deterministic. Baseline Verification must establish the Development Baseline values (1000 and 10000 operations) and verify the protocol; it must not require the Development Baseline to improve over itself. Candidate acceptance during Iteration and Integration requires correctness plus at least 90% fewer logical operations than those baseline values on every case, with no guard regression. The stop condition is a Best implementation reporting at most one logical operation per case.

The intended safe optimization is the arithmetic-series closed form. harness.py, its oracle, the cases, and the protocol are frozen after Baseline acceptance. Baseline Draft may add and commit a JSON Definition. Iteration must commit the Candidate and leave its worktree clean. Integration must use Pika's two-phase Git Intent. Preserve command output as evidence.
`,
		"candidate.py": `def sum_to_n(n):
    total = 0
    for value in range(1, n + 1):
        total += value
    return total, n
`,
		"harness.py": `import argparse
import json

import candidate

CASES = {"small": 1000, "large": 10000}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["correctness", "benchmark"])
    parser.add_argument("--cases", nargs="+", required=True)
    args = parser.parse_args()
    if not args.cases or any(name not in CASES for name in args.cases):
        raise SystemExit("cases must be a non-empty subset of small, large")
    rows = []
    for name in args.cases:
        n = CASES[name]
        actual, operations = candidate.sum_to_n(n)
        expected = n * (n + 1) // 2
        if actual != expected:
            print(json.dumps({"case": name, "actual": actual, "expected": expected, "ok": False}))
            raise SystemExit(1)
        if args.mode == "correctness":
            rows.append({"case": name, "ok": True, "actual": actual})
        else:
            rows.append({"case": name, "metric": "logical_operations", "unit": "operations", "direction": "lower", "value": operations})
    print(json.dumps({"mode": args.mode, "results": rows}, sort_keys=True))


if __name__ == "__main__":
    main()
`,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"-C", repository, "add", "."}, {"-C", repository, "commit", "-m", "add deterministic optimization fixture"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	for _, args := range [][]string{{"correctness", "--cases", "small", "large"}, {"benchmark", "--cases", "small", "large"}} {
		if output, err := exec.Command("python3", append([]string{filepath.Join(repository, "harness.py")}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("fixture %v: %v: %s", args, err, output)
		}
	}
}
