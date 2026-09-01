package workruntime_test

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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/kdacontract"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
	workruntime "github.com/reyoung/pika-go/internal/workruntime"
)

const realProviderStallBudget = 2 * time.Minute

type realProviderSpec struct {
	name        string
	agents      map[string]configuration.Agent
	modelEnv    string
	keepEnv     string
	requestTag  string
	simple      bool
	promptSmoke bool
}

func TestRealCodexHotReloadCompletesSimpleOptimization(t *testing.T) {
	if os.Getenv("PIKA_GO_REAL_CODEX_HOT_RELOAD_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_REAL_CODEX_HOT_RELOAD_INTEGRATION=1 to run the quota-consuming Codex hot-reload smoke")
	}
	runRealProviderOptimization(t, realProviderSpec{name: "codex-hot-reload", agents: configuration.DefaultAgents(), modelEnv: "PIKA_GO_REAL_CODEX_MODEL", keepEnv: "PIKA_GO_REAL_CODEX_KEEP", requestTag: "real-codex-hot", simple: true})
}

func TestRealCursorHotReloadCompletesSimpleOptimization(t *testing.T) {
	if os.Getenv("PIKA_GO_REAL_CURSOR_HOT_RELOAD_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_REAL_CURSOR_HOT_RELOAD_INTEGRATION=1 to run the quota-consuming Cursor hot-reload smoke")
	}
	runRealProviderOptimization(t, realProviderSpec{name: "cursor-hot-reload", agents: matrixAgents("cursor", "cursor", "cursor", "cursor", "cursor"), modelEnv: "PIKA_GO_REAL_CURSOR_MODEL", keepEnv: "PIKA_GO_REAL_CURSOR_KEEP", requestTag: "real-cursor-hot", simple: true})
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

// TestRealCursorPromptDeliverySmoke is intentionally narrower than the
// credentialed release matrix. It proves that a real Cursor pane accepts the
// initial operator prompt and becomes observably active before spending quota
// on the multi-phase lifecycle matrix.
func TestRealCursorPromptDeliverySmoke(t *testing.T) {
	if os.Getenv("PIKA_GO_REAL_CURSOR_PROMPT_SMOKE") != "1" {
		t.Skip("set PIKA_GO_REAL_CURSOR_PROMPT_SMOKE=1 to run the focused real Cursor prompt smoke")
	}
	runRealProviderOptimization(t, realProviderSpec{name: "cursor-prompt", agents: matrixAgents("cursor", "cursor", "cursor", "cursor", "cursor"), modelEnv: "PIKA_GO_REAL_CURSOR_MODEL", keepEnv: "PIKA_GO_REAL_CURSOR_KEEP", requestTag: "real-cursor-prompt", promptSmoke: true})
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
	if hasAgentKind(spec.agents, "codex") {
		codexHome = isolateRealCodexHome(t, root, codexHome)
	}
	repository := filepath.Join(root, "repository")
	initializeRealCodexFixture(t, repository)
	var workspace *optimizationworkspace.Workspace
	herdrCWD := repository
	stateRoot := filepath.Join(root, "pika-state")
	configRoot := filepath.Join(root, "pika-config")
	if spec.simple {
		createdWorkspace, createErr := optimizationworkspace.Create(context.Background(), filepath.Join(root, "workspace"), repository)
		if createErr != nil {
			t.Fatalf("create real-provider Optimization Workspace: %v", createErr)
		}
		workspace = &createdWorkspace
		herdrCWD = workspace.Root
		stateRoot, configRoot = workspace.Root, workspace.Root
	}
	followUpHoldPath := createRealCodexHold(t, root, "verification")
	iterationHoldPath := createRealCodexHold(t, root, "iteration")
	integrationHoldPath := createRealCodexHold(t, root, "integration")
	if hasAgentKind(spec.agents, "codex") {
		restoreTrust := trustRealCodexFixture(t, codexHome, repository)
		t.Cleanup(restoreTrust)
		if workspace != nil {
			restoreWorkspaceTrust := trustRealCodexFixture(t, codexHome, workspace.BaseRepository)
			t.Cleanup(restoreWorkspaceTrust)
		}
	}

	herdrConfig, herdrState := configureRealCodexHerdr(t, root)
	if hasAgentKind(spec.agents, "cursor") {
		linkRealCursorAuthentication(t, filepath.Dir(herdrConfig))
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig),
		"XDG_STATE_HOME":  herdrState,
		"CODEX_HOME":      codexHome,
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

	testCtx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(testCtx, "workspace.create", map[string]any{
		"cwd": herdrCWD, "label": "pika-real-" + spec.name, "focus": false,
	}, &created); err != nil {
		t.Fatalf("create isolated Herdr workspace: %v; output=%s", err, serverOutput.String())
	}
	t.Cleanup(func() {
		_ = client.Call(context.Background(), "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
	})
	initPane, err := herdr.NewRuntime(client).SplitPane(testCtx, created.RootPane.PaneID, "down", herdrCWD)
	if err != nil {
		t.Fatal(err)
	}

	pikaSocket := filepath.Join(root, "pika.sock")
	model := os.Getenv(spec.modelEnv)
	if model == "" {
		model = "gpt-5.6-luna"
	}
	profileRollback := configureRealProviderInstance(t, testCtx, workspace, repository, stateRoot, configRoot, codexHome, pikaBinary, codexBinary, model, spec.agents, spec.simple)
	t.Cleanup(func() {
		if err := profileRollback(); err != nil {
			t.Errorf("restore pre-smoke Codex profile: %v", err)
		}
	})

	daemon := startRealCodexDaemon(t, pikaBinary, pikaSocket, workspace, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, codexHome, codexBinary, cursorBinary)
	assertRealDaemonExecutableDigest(t, daemon, pikaBinary)
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
	promptInitialRealProviderBaseline(t, testCtx, stateRoot, client, pikaSocket, daemon, &serverOutput)
	if spec.promptSmoke {
		exerciseRealSchedulerPauseResume(t, testCtx, client, pikaSocket, stateRoot, spec.requestTag)
		t.Logf("focused real %s prompt smoke observed an active Agent and completed pause/resume", spec.name)
		return
	}
	if !spec.simple {
		exerciseRealSchedulerPauseResume(t, testCtx, client, pikaSocket, stateRoot, spec.requestTag)
	}
	if spec.simple {
		followUpTarget := waitForRealProviderVerificationHold(t, testCtx, stateRoot, client, pikaSocket, daemon, &serverOutput)
		hotBinary := os.Getenv("PIKA_GO_HOT_BIN")
		if hotBinary == "" || !filepath.IsAbs(hotBinary) {
			t.Fatal("PIKA_GO_HOT_BIN must be the absolute path to the hot-update candidate")
		}
		exerciseRealProviderHotReload(t, testCtx, *workspace, hotBinary, followUpTarget, client, pikaSocket, daemon, &serverOutput)
		releaseRealCodexHold(t, followUpHoldPath)
		waitForRealProviderFollowUp(t, testCtx, stateRoot, followUpTarget, client, pikaSocket, daemon, &serverOutput)
		releaseRealCodexHold(t, iterationHoldPath)
		releaseRealCodexHold(t, integrationHoldPath)
		view := waitForRealCodexBest(t, testCtx, client, pikaSocket, daemon, &serverOutput)
		if view.Baseline == nil || view.Baseline.Status != symphony.BaselineAccepted || view.Best == nil || view.Best.Sequence < 1 {
			t.Fatalf("simple real-provider flow did not accept Baseline and advance Best: baseline=%+v best=%+v", view.Baseline, view.Best)
		}
		if view.Storage.ProviderEventBytes == 0 || view.Storage.ToolPayloadBytes == 0 {
			t.Fatalf("simple real-provider hooks did not persist journal payloads: %+v", view.Storage)
		}
		if output, err := exec.Command("git", "-C", repository, "rev-parse", realProviderBestRef(workspace)).CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != view.Best.CommitSHA {
			t.Fatalf("simple Git/SQLite Best mismatch: git=%q err=%v db=%+v", output, err, view.Best)
		}
		if _, err := control.Shutdown(testCtx, pikaSocket, protocol.ShutdownRequest{Mutation: protocol.Mutation{RequestID: spec.requestTag + "-shutdown"}}); err != nil {
			t.Fatalf("request simple graceful shutdown: %v", err)
		}
		cancelPendingRealCodexWork(t, testCtx, pikaSocket)
		select {
		case err := <-daemon.done:
			if err != nil {
				t.Fatalf("simple daemon did not drain cleanly: %v; output=%s", err, daemon.output.String())
			}
			daemon = nil
		case <-time.After(90 * time.Second):
			t.Fatalf("simple daemon did not finish draining: %s", diagnoseRealCodex(testCtx, client, pikaSocket, daemon, &serverOutput))
		}
		assertRealProviderJournal(t, testCtx, workspace.DatabasePath, view, 4)
		t.Logf("simple real %s hot-reload flow advanced Best to sequence %d", spec.name, view.Best.Sequence)
		return
	}
	// Exercise a natural Follow-up while Verification is still live, then begin
	// the bounded Diagnosis-session window only after Verification completes.
	verificationTarget := exerciseRealCodexFollowUp(t, testCtx, stateRoot, followUpHoldPath, client, pikaSocket, daemon, &serverOutput)
	t.Logf("real %s Verification Work %s completed a natural Follow-up; starting the bounded Diagnosis phase", verificationTarget.Session.AgentKind, verificationTarget.Work.ID)

	// Crash a real active Diagnosis session, after Baseline acceptance has
	// already projected it.  This is the flow-v2 recovery point required by the
	// release contract.  Crashing Verification instead made a valid rejected
	// baseline consume the Diagnosis budget before its work even existed.
	diagnosisTarget := waitForRealCodexRoleSessions(t, testCtx, stateRoot, symphony.RoleDiagnosis, 1, client, pikaSocket, daemon, &serverOutput)[0]
	crashedOutput := crashRealCodexDaemon(t, daemon)
	daemon = nil
	daemon = startRealCodexDaemon(t, pikaBinary, pikaSocket, workspace, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, codexHome, codexBinary, cursorBinary)
	waitForRealCodexReplacement(t, testCtx, stateRoot, diagnosisTarget, client, pikaSocket, daemon, &serverOutput)
	t.Logf("real provider Diagnosis recovered through a fresh Session after daemon crash; crashed daemon output bytes=%d", len(crashedOutput))

	waitForRealDiagnosisComplete(t, testCtx, client, pikaSocket, daemon, &serverOutput)
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
	durableView := inspectRealProviderView(t, testCtx, realProviderDatabasePath(stateRoot))
	if !hasCancelledAttempt(durableView) || !hasIntegrationBackOff(durableView, "real release-matrix back-off") {
		t.Fatalf("real Codex release matrix omitted cancellation or Back-off: attempts=%+v rounds=%+v", durableView.Attempts, durableView.IterationRounds)
	}
	if view.Storage.ProviderEventBytes == 0 || view.Storage.ToolPayloadBytes == 0 {
		t.Fatalf("Codex hooks did not persist observable journal payloads: %+v", view.Storage)
	}
	if output, err := exec.Command("git", "-C", repository, "rev-parse", realProviderBestRef(workspace)).CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != view.Best.CommitSHA {
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
	assertRealFlowV2Contract(t, testCtx, realProviderDatabasePath(stateRoot))

	engine, err := symphony.Open(testCtx, realProviderDatabasePath(stateRoot), symphony.Options{})
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

func assertRealDaemonExecutableDigest(t *testing.T, daemon *daemonProcess, expectedPath string) {
	t.Helper()
	if daemon == nil || daemon.command == nil || daemon.command.Process == nil {
		t.Fatal("real daemon process is unavailable for executable digest verification")
	}
	executable, err := os.Readlink(filepath.Join("/proc", fmt.Sprintf("%d", daemon.command.Process.Pid), "exe"))
	if err != nil {
		t.Fatalf("read real daemon /proc executable: %v", err)
	}
	expected, err := executableSHA256(expectedPath)
	if err != nil {
		t.Fatalf("digest requested PIKA_GO_BIN: %v", err)
	}
	actual, err := executableSHA256(executable)
	if err != nil {
		t.Fatalf("digest real daemon executable %s: %v", executable, err)
	}
	if actual != expected {
		t.Fatalf("real daemon executable digest=%s want PIKA_GO_BIN digest=%s (exe=%s)", actual, expected, executable)
	}
	t.Logf("real daemon pid=%d executable sha256=%s", daemon.command.Process.Pid, actual)
}

func executableSHA256(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:]), nil
}

func promptInitialRealProviderBaseline(t *testing.T, ctx context.Context, stateRoot string, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		view, err := control.Status(ctx, socketPath)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for index := len(view.AgentSessions) - 1; index >= 0; index-- {
			session := view.AgentSessions[index]
			if session.Role != symphony.RoleBaselineDraft || session.Status != symphony.AgentSessionRunning {
				continue
			}
			snapshot, snapshotErr := client.Snapshot(ctx)
			if snapshotErr != nil {
				break
			}
			for _, agent := range snapshot.Agents {
				if agent.Name == nil || *agent.Name != session.AgentName || agent.PaneID == "" {
					continue
				}
				runtime := workruntime.NewHerdrRuntime(client, agent.PaneID)
				message := "请读取仓库 README.md，并按其中给出的 target、case set、correctness oracle、benchmark protocol 和 stop condition 完成 Baseline Definition。"
				before := int64(0)
				if session.AgentKind == "cursor" {
					before = latestRealProviderEventSequence(t, ctx, realProviderDatabasePath(stateRoot), session.ID)
				}
				if err := runtime.PromptForProvider(ctx, agent.PaneID, message, session.AgentKind); err != nil {
					if session.AgentKind != "cursor" || !waitForRealCursorPromptHook(t, ctx, realProviderDatabasePath(stateRoot), session.ID, before, message, 10*time.Second) {
						t.Fatalf("send operator Baseline configuration: %v; %s", err, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
					}
					t.Logf("real Cursor operator prompt confirmed by a new matching durable beforeSubmitPrompt after Herdr missed the active transition")
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("first Baseline Agent did not become ready for operator input: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
}

func latestRealProviderEventSequence(t *testing.T, ctx context.Context, databasePath, sessionID string) int64 {
	t.Helper()
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("open real-provider journal before prompt: %v", err)
	}
	defer engine.Close()
	sequence, err := engine.LatestProviderEventSequence(ctx, sessionID)
	if err != nil {
		t.Fatalf("read real-provider event high-watermark before prompt: %v", err)
	}
	return sequence
}

func waitForRealCursorPromptHook(t *testing.T, ctx context.Context, databasePath, sessionID string, after int64, message string, timeout time.Duration) bool {
	t.Helper()
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("open real Cursor prompt journal: %v", err)
	}
	defer engine.Close()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		observed, err := engine.PromptSubmissionObservedAfter(ctx, sessionID, after, message)
		if err != nil {
			t.Fatalf("read normalized real Cursor prompt submission: %v", err)
		}
		if observed {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func exerciseRealSchedulerPauseResume(t *testing.T, ctx context.Context, client *herdr.Client, socketPath, stateRoot, requestTag string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var before symphony.OperatorStatus
	var lastSnapshot herdr.Snapshot
	var targetID string
	var targetName string
	var targetKind string
	for time.Now().Before(deadline) {
		view, err := control.Status(ctx, socketPath)
		if err == nil {
			before = view
			for index := len(view.AgentSessions) - 1; index >= 0; index-- {
				session := view.AgentSessions[index]
				if session.Status != symphony.AgentSessionStarting && session.Status != symphony.AgentSessionRunning {
					continue
				}
				snapshot, snapshotErr := client.Snapshot(ctx)
				if snapshotErr != nil {
					break
				}
				lastSnapshot = snapshot
				for _, agent := range snapshot.Agents {
					if agent.Name != nil && *agent.Name == session.AgentName && (agent.AgentStatus == "working" || agent.AgentStatus == "blocked") {
						targetID = session.ID
						targetName = session.AgentName
						targetKind = session.AgentKind
						break
					}
				}
				if targetID != "" {
					break
				}
			}
			if targetID != "" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if targetID == "" {
		t.Fatalf("real provider did not expose an active runtime Agent before Scheduler pause: sessions=%+v snapshot_agents=%+v snapshot_panes=%+v", before.AgentSessions, lastSnapshot.Agents, lastSnapshot.Panes)
	}
	// Active UI state can occur during Cursor startup or while its delayed
	// bracketed-paste Enter is still queued. Require a session-scoped submitted
	// prompt and a completed/failed provider tool event, then re-observe active
	// Herdr state. These are a sequencing barrier for the interrupt, not a
	// substitute for the working|blocked lifecycle assertion.
	waitForRealProviderExecutionBarrier(t, ctx, realProviderDatabasePath(stateRoot), targetID, targetKind, 30*time.Second)
	waitForRealProviderActiveStatus(t, ctx, client, targetName, 15*time.Second, "after durable provider execution barrier")
	pause, err := control.PauseScheduler(ctx, socketPath, protocol.SchedulerControlRequest{Mutation: protocol.Mutation{RequestID: requestTag + "-pause"}})
	if err != nil {
		t.Fatalf("pause real provider: %v", err)
	}
	if pause.Control.HasDeliveryFailure() || len(pause.Control.Actions) != 1 || pause.Control.Actions[0].Status != symphony.SchedulerActionSent || pause.Control.Actions[0].AgentSessionID != targetID {
		t.Fatalf("real pause cycle=%+v", pause.Control)
	}
	waitForRealProviderAgentStatus(t, ctx, client, targetName, "idle", 15*time.Second, "after Scheduler pause")
	resume, err := control.ResumeScheduler(ctx, socketPath, protocol.SchedulerControlRequest{Mutation: protocol.Mutation{RequestID: requestTag + "-resume"}})
	if err != nil {
		t.Fatalf("resume real provider: %v", err)
	}
	if resume.Control.HasDeliveryFailure() || len(resume.Control.Actions) != 1 {
		t.Fatalf("real resume cycle=%+v target=%s", resume.Control, targetID)
	}
	resumeAction := resume.Control.Actions[0]
	if (resumeAction.Status != symphony.SchedulerActionSent && resumeAction.Status != symphony.SchedulerActionObserved) ||
		resumeAction.AgentSessionID != targetID || resumeAction.Message != "继续" {
		t.Fatalf("real resume cycle=%+v target=%s", resume.Control, targetID)
	}
	waitForRealProviderActiveStatus(t, ctx, client, targetName, 15*time.Second, "after Scheduler resume")
}

func providerPromptHook(providerKind string) string {
	if providerKind == "cursor" {
		return "beforeSubmitPrompt"
	}
	return "UserPromptSubmit"
}

func waitForRealProviderExecutionBarrier(t *testing.T, ctx context.Context, databasePath, agentSessionID, providerKind string, timeout time.Duration) {
	t.Helper()
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("open real provider journal while waiting for execution barrier: %v", err)
	}
	defer engine.Close()
	promptHook := providerPromptHook(providerKind)
	deadline := time.Now().Add(timeout)
	var last []symphony.ProviderEventView
	for time.Now().Before(deadline) {
		events, err := engine.ProviderEvents(ctx, agentSessionID)
		if err == nil {
			last = events
			hasPrompt, hasTool := false, false
			for _, event := range events {
				if event.HookEventName == promptHook && event.ProviderSessionID != "" {
					hasPrompt = true
				}
				if isRealProviderToolCompletion(providerKind, event) {
					hasTool = true
				}
			}
			if hasPrompt && hasTool {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real provider Agent Session %s did not reach submitted-prompt plus tool-execution barrier before Scheduler pause: events=%+v", agentSessionID, last)
}

func isRealProviderToolCompletion(providerKind string, event symphony.ProviderEventView) bool {
	if providerKind == "cursor" {
		if event.HookEventName != "postToolUse" && event.HookEventName != "postToolUseFailure" {
			return false
		}
	} else if event.HookEventName != "PostToolUse" && event.HookEventName != "PostToolUseFailure" {
		return false
	}
	var raw struct {
		ToolName string `json:"tool_name"`
	}
	return json.Unmarshal(event.Raw, &raw) == nil && raw.ToolName != ""
}

func waitForRealProviderAgentStatus(t *testing.T, ctx context.Context, client *herdr.Client, agentName, want string, timeout time.Duration, phase string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last herdr.Agent
	for time.Now().Before(deadline) {
		snapshot, err := client.Snapshot(ctx)
		if err == nil {
			for _, agent := range snapshot.Agents {
				if agent.Name != nil && *agent.Name == agentName {
					last = agent
					if agent.AgentStatus == want {
						return
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real provider Agent %s did not become %s %s: last=%+v", agentName, want, phase, last)
}

func waitForRealProviderActiveStatus(t *testing.T, ctx context.Context, client *herdr.Client, agentName string, timeout time.Duration, phase string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last herdr.Agent
	for time.Now().Before(deadline) {
		snapshot, err := client.Snapshot(ctx)
		if err == nil {
			for _, agent := range snapshot.Agents {
				if agent.Name != nil && *agent.Name == agentName {
					last = agent
					if agent.AgentStatus == "working" || agent.AgentStatus == "blocked" {
						return
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real provider Agent %s did not become working or blocked %s: last=%+v", agentName, phase, last)
}

func isolateRealCodexHome(t *testing.T, root, sourceHome string) string {
	t.Helper()
	isolated := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(isolated, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceAuth := filepath.Join(sourceHome, "auth.json")
	if info, err := os.Stat(sourceAuth); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("real Codex authentication file is unavailable: %v", err)
	}
	if err := os.Symlink(sourceAuth, filepath.Join(isolated, "auth.json")); err != nil {
		t.Fatal(err)
	}
	// Provider catalog membership is intentionally checked before the daemon
	// starts. The credentialed fixture runs with an isolated CODEX_HOME, so
	// expose the CLI's real, account-scoped catalog there as read-only input.
	// This is not a test catalog: without the link the daemon cannot prove the
	// requested real model is launchable.
	sourceCatalog := filepath.Join(sourceHome, "models_cache.json")
	if info, err := os.Stat(sourceCatalog); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("real Codex model catalog is unavailable: %v", err)
	}
	if err := os.Symlink(sourceCatalog, filepath.Join(isolated, "models_cache.json")); err != nil {
		t.Fatal(err)
	}
	return isolated
}

func realProviderBestRef(workspace *optimizationworkspace.Workspace) string {
	if workspace == nil {
		return "refs/heads/pika/best"
	}
	return "refs/heads/" + workspace.BestBranch()
}

func TestRealProviderBestRefUsesWorkspaceNamespace(t *testing.T) {
	workspace := optimizationworkspace.Workspace{Identity: optimizationworkspace.Identity{ID: "0123456789abcdef0123456789abcdef"}}
	want := "refs/heads/" + workspace.BestBranch()
	if got := realProviderBestRef(&workspace); got != want {
		t.Fatalf("real-provider Best ref = %q, want %q", got, want)
	}
}

func TestRealProviderVerificationInstructionsKeepWorkPendingForFollowUp(t *testing.T) {
	instructions := realProviderRoleInstructions()["baseline-verify.md"]
	if !strings.Contains(instructions, "do not call `finish_baseline_verification` during this first turn") {
		t.Fatalf("Verification instructions do not preserve pending Work for a natural Follow-up: %q", instructions)
	}
	if !strings.Contains(instructions, "messages.records is greater than zero") {
		t.Fatalf("Verification recovery instructions force a duplicate Follow-up: %q", instructions)
	}
}

func assertRealProviderJournal(t *testing.T, ctx context.Context, databasePath string, view symphony.OperatorStatus, minimumProviderSessions int) {
	t.Helper()
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("reopen final real-provider database: %v", err)
	}
	defer engine.Close()
	toolEvents := 0
	providerSessionIDs := map[string]struct{}{}
	for _, work := range view.Works {
		journal, err := engine.ConversationJournal(ctx, work.ID)
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
	if len(providerSessionIDs) < minimumProviderSessions || toolEvents == 0 {
		t.Fatalf("incomplete real-provider journal: provider_sessions=%d want_at_least=%d tool_events=%d", len(providerSessionIDs), minimumProviderSessions, toolEvents)
	}
	t.Logf("real-provider journal contains %d provider sessions and %d observable tool events", len(providerSessionIDs), toolEvents)
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

func configureRealProviderInstance(t *testing.T, ctx context.Context, workspace *optimizationworkspace.Workspace, repository, stateRoot, configRoot, codexHome, pikaBinary, codexBinary, model string, agents map[string]configuration.Agent, simple bool) func() error {
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
	iterationCount := 2
	if simple {
		iterationCount = 1
	}
	iterationAgents := make([]configuration.Agent, iterationCount)
	for index := range iterationAgents {
		iterationAgents[index] = configuredAgents["iteration"]
	}
	configured, err := configuration.RenderConfiguration(repository, configuredAgents, iterationAgents...)
	if err != nil {
		t.Fatal(err)
	}
	configured = strings.Replace(configured, "max_pending_attempts = 8", "max_pending_attempts = 2", 1)
	configured = strings.Replace(configured, "pane_idle_timeout = \"5m\"", "pane_idle_timeout = \"10m\"", 1)
	if simple {
		configured = strings.Replace(configured, "max_pending_attempts = 2", "max_pending_attempts = 1", 1)
	}
	initializer := configuration.Initializer{
		Workspace: workspace, ConfigRoot: configRoot, StateRoot: stateRoot, InstanceID: "real-codex",
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
	instructionRoot := filepath.Join(configRoot, "instances", "real-codex", "instructions")
	if workspace != nil {
		gatePath = filepath.Join(workspace.RuntimeRoot, "bin", "pika-real-gate")
		instructionRoot = workspace.InstructionsRoot
	}
	if err := os.MkdirAll(filepath.Dir(gatePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gatePath, []byte(gateScript), 0o700); err != nil {
		t.Fatal(err)
	}
	roleInstructions := realProviderRoleInstructions()
	for name, instruction := range roleInstructions {
		if err := os.WriteFile(filepath.Join(instructionRoot, name), []byte(instruction), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return rollback
}

func realProviderRoleInstructions() map[string]string {
	roleInstructions := map[string]string{
		"baseline.md":        "This is a strict flow-v2 release fixture. The Baseline Definition's benchmark_integrity must use exactly case_ids [\"small\",\"large\"], benchmark_repeats=1, warmup_invocations_per_repeat=0, measured_invocations_per_repeat=1, canonical_inputs_candidate_visible=false, and every other named benchmark_integrity boolean=true. The fixture's CPU execution domain is its device domain: do not describe input restoration, device validation, compact host transfer, or timing boundaries as not applicable. The evidence for each case must report warmup_invocations=0, measured_invocations=1, input_restores=1, checked_invocations=1, mismatches=0, nonfinite=0, canonical_input_mutations=0, tolerance_passed=true. If the Definition records the initial development baseline Git SHA, execute `git rev-list --max-parents=0 HEAD` and copy its exact 40-hex output: never guess, transpose, or invent it. Preserve this protocol exactly; it is the release gate, not an optional accelerator claim.\n",
		"baseline-verify.md": "At the beginning of this Session, immediately after fully reading the Context Bundle and before any other tool or shell work, run `pika-real-gate verification` and wait until it exits successfully. This is a test-instance synchronization requirement.\nAfter the gate exits, establish the Development Baseline. When the Context Bundle reports messages.records is zero, do not call `finish_baseline_verification` during this first turn: end the turn with the Work still pending and wait for Pika's Follow-up, then finish only after that Follow-up arrives. When messages.records is greater than zero, this is a recovered Session with prior Work history; do not deliberately request another Follow-up and proceed to the terminal operation when the evidence is ready.\n",
		"iteration.md":       "At the beginning of this Session, immediately after fully reading the Context Bundle and before any other tool or shell work, run `pika-real-gate iteration` and wait until it exits successfully. This is a test-instance synchronization requirement.\n",
		"integration.md":     "At the beginning of this Session, immediately after fully reading the Context Bundle and before any other tool or shell work, run `pika-real-gate integration` and wait until it exits successfully. This is a test-instance synchronization requirement.\n",
	}
	return roleInstructions
}

func realCursorMCPPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".cursor", "mcp.json")
}

// linkRealCursorAuthentication deliberately carries only the pre-existing
// credential into the disposable XDG config root. Herdr requires that root to
// avoid colliding with a developer's running server; without this link Cursor
// enters its interactive login UI and never receives the test prompt.
func linkRealCursorAuthentication(t *testing.T, destinationConfigHome string) {
	t.Helper()
	sourceConfigHome := os.Getenv("XDG_CONFIG_HOME")
	if sourceConfigHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		sourceConfigHome = filepath.Join(home, ".config")
	}
	source := filepath.Join(sourceConfigHome, "cursor", "auth.json")
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("real Cursor authentication file is unavailable at %s: %v", source, err)
	}
	destinationDir := filepath.Join(destinationConfigHome, "cursor")
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, filepath.Join(destinationDir, "auth.json")); err != nil {
		t.Fatal(err)
	}
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

func startRealCodexDaemon(t *testing.T, pikaBinary, socketPath string, workspace *optimizationworkspace.Workspace, stateRoot, configRoot, herdrSocket, symphonyPane, codexHome, codexBinary, cursorBinary string) *daemonProcess {
	t.Helper()
	process := &daemonProcess{done: make(chan error, 1), socketPath: socketPath}
	arguments := []string{"daemon", "--socket", socketPath}
	if workspace != nil {
		arguments = append(arguments, "--workspace", workspace.Root)
	} else {
		arguments = append(arguments, "--state-dir", stateRoot, "--config-dir", configRoot, "--instance", "real-codex")
	}
	process.command = exec.Command(pikaBinary, arguments...)
	process.command.Env = replacedEnvironment(map[string]string{
		"HERDR_SOCKET_PATH":           herdrSocket,
		"HERDR_CONFIG_PATH":           filepath.Join(filepath.Dir(herdrSocket), "config.toml"),
		"HERDR_PANE_ID":               symphonyPane,
		"HERDR_ENV":                   "1",
		"CODEX_HOME":                  codexHome,
		"PIKA_GO_CODEX_EXECUTABLE":    codexBinary,
		"PIKA_GO_CURSOR_EXECUTABLE":   cursorBinary,
		"PIKA_GO_FOLLOWUP_INACTIVITY": "500ms",
	})
	process.command.Stdout, process.command.Stderr = &process.output, &process.output
	if err := process.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { process.done <- process.command.Wait() }()
	// Cursor's bounded preflight permits up to 15 seconds for its version and
	// 45 seconds for authentication. The credentialed gate must not declare the
	// daemon unhealthy before those production probe budgets can expire.
	waitForPikaProcessHealthWithin(t, socketPath, process, 70*time.Second)
	return process
}

func realProviderDatabasePath(workspaceRoot string) string {
	if _, err := os.Stat(filepath.Join(workspaceRoot, optimizationworkspace.ManifestName)); err == nil {
		return filepath.Join(workspaceRoot, "pika.db")
	}
	return filepath.Join(workspaceRoot, "instances", "real-codex", "pika.db")
}

func exerciseRealProviderHotReload(t *testing.T, ctx context.Context, workspace optimizationworkspace.Workspace, hotBinary string, target realCodexSessionObservation, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) {
	t.Helper()
	beforeHealth, err := control.Health(ctx, socketPath)
	if err != nil {
		t.Fatalf("read daemon health before hot reload: %v", err)
	}
	engine, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, beforeBinding, found, err := engine.CurrentAgentSession(ctx, target.Work.ID)
	if err != nil || !found || beforeSession.ID != target.Session.ID {
		_ = engine.Close()
		t.Fatalf("read active Session before hot reload: session=%+v binding=%+v found=%v err=%v", beforeSession, beforeBinding, found, err)
	}
	_ = engine.Close()

	candidate, err := daemonupdate.Stage(ctx, workspace.Root, hotBinary)
	if err != nil {
		t.Fatalf("stage real-provider hot-update candidate: %v", err)
	}
	accepted, err := control.Update(ctx, socketPath, candidate)
	if err != nil {
		t.Fatalf("request real-provider hot reload: %v; %s", err, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
	}
	// Update() returns only after the candidate reports PREPARED. From that
	// point, the successful path has one bounded old-generation quiesce and two
	// ActivationDeadline phases (READY and health stabilization). This is a
	// protocol-derived watchdog, not extra time for an unhealthy provider.
	deadline := time.Now().Add(2*daemonupdate.ActivationDeadline + 10*time.Second)
	for {
		status, readErr := daemonupdate.ReadStatus(workspace.Root)
		if readErr != nil {
			t.Fatalf("read accepted hot-reload status: %v", readErr)
		}
		if status.ID != accepted.ID {
			t.Fatalf("hot-reload status changed identity: got %s, want %s", status.ID, accepted.ID)
		}
		if status.State == daemonupdate.StateCommitted {
			break
		}
		if status.State == daemonupdate.StateRollingBack || status.State == daemonupdate.StateRolledBack || status.State == daemonupdate.StateFailed {
			t.Fatalf("real-provider hot reload ended in %s: %s; %s", status.State, status.Failure, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
		}
		if time.Now().After(deadline) {
			t.Fatalf("real-provider hot reload did not commit: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
		}
		time.Sleep(100 * time.Millisecond)
	}
	afterHealth, err := control.Health(ctx, socketPath)
	if err != nil {
		t.Fatalf("read daemon health after hot reload: %v", err)
	}
	if beforeHealth.PID == afterHealth.PID || afterHealth.BinaryDigest != candidate.Digest {
		t.Fatalf("daemon generation did not change: before=%+v after=%+v candidate=%s", beforeHealth, afterHealth, candidate.Digest)
	}
	engine, err = symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	afterSession, afterBinding, found, err := engine.CurrentAgentSession(ctx, target.Work.ID)
	_ = engine.Close()
	if err != nil || !found || !reflect.DeepEqual(afterSession, beforeSession) || afterBinding != beforeBinding {
		t.Fatalf("active Session changed across hot reload: before=%+v/%+v after=%+v/%+v found=%v err=%v", beforeSession, beforeBinding, afterSession, afterBinding, found, err)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot Herdr after hot reload: %v", err)
	}
	paneStillLive := false
	for _, pane := range snapshot.Panes {
		if pane.PaneID == beforeBinding.PaneID && pane.TerminalID == beforeBinding.TerminalID {
			paneStillLive = true
			break
		}
	}
	if !paneStillLive {
		t.Fatalf("active Agent pane disappeared across hot reload: binding=%+v snapshot=%+v", beforeBinding, snapshot.Panes)
	}
	t.Logf("hot-reloaded daemon %d -> %d while preserving %s Session %s and pane %s", beforeHealth.PID, afterHealth.PID, beforeSession.AgentKind, beforeSession.ID, beforeBinding.PaneID)
}

type realCodexSessionObservation struct {
	Work              symphony.WorkView
	Session           symphony.AgentSession
	ProviderSessionID string
}

type realProviderProgressWatch struct {
	stallBudget     time.Duration
	lastFingerprint string
	lastProgress    time.Time
}

func newRealProviderProgressWatch(now time.Time) *realProviderProgressWatch {
	return &realProviderProgressWatch{stallBudget: realProviderStallBudget, lastProgress: now}
}

func (watch *realProviderProgressWatch) Observe(now time.Time, view symphony.OperatorStatus, snapshot herdr.Snapshot) error {
	active := make(map[string]symphony.OperatorAgentSession)
	for _, session := range view.AgentSessions {
		if session.Status == symphony.AgentSessionStarting || session.Status == symphony.AgentSessionRunning {
			active[session.AgentName] = session
		}
	}
	for _, agent := range snapshot.Agents {
		if agent.Name == nil || agent.AgentStatus != "blocked" {
			continue
		}
		if session, found := active[*agent.Name]; found {
			return fmt.Errorf("active %s Session %s for Work %s is blocked in Herdr", session.Role, session.ID, session.WorkID)
		}
	}

	type agentProgress struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		InteractiveReady bool   `json:"interactive_ready"`
		PaneID           string `json:"pane_id"`
	}
	type paneProgress struct {
		PaneID      string `json:"pane_id"`
		AgentStatus string `json:"agent_status"`
		Revision    uint64 `json:"revision"`
	}
	agents := make([]agentProgress, 0, len(snapshot.Agents))
	for _, agent := range snapshot.Agents {
		name := ""
		if agent.Name != nil {
			name = *agent.Name
		}
		agents = append(agents, agentProgress{Name: name, Status: agent.AgentStatus, InteractiveReady: agent.InteractiveReady, PaneID: agent.PaneID})
	}
	panes := make([]paneProgress, 0, len(snapshot.Panes))
	for _, pane := range snapshot.Panes {
		panes = append(panes, paneProgress{PaneID: pane.PaneID, AgentStatus: pane.AgentStatus, Revision: pane.Revision})
	}
	fingerprint, err := json.Marshal(struct {
		Optimization       symphony.OperatorOptimization   `json:"optimization"`
		Baseline           *symphony.OperatorBaseline      `json:"baseline"`
		Works              []symphony.WorkView             `json:"works"`
		Sessions           []symphony.OperatorAgentSession `json:"sessions"`
		FollowUps          []symphony.OperatorFollowUp     `json:"follow_ups"`
		DomainEventCount   int64                           `json:"domain_event_count"`
		PendingEffectCount int64                           `json:"pending_effect_count"`
		ProviderEventBytes int64                           `json:"provider_event_bytes"`
		ToolPayloadBytes   int64                           `json:"tool_payload_bytes"`
		Agents             []agentProgress                 `json:"agents"`
		Panes              []paneProgress                  `json:"panes"`
	}{
		Optimization: view.Optimization, Baseline: view.Baseline, Works: view.Works, Sessions: view.AgentSessions,
		FollowUps: view.FollowUps, DomainEventCount: view.DomainEventCount, PendingEffectCount: view.PendingEffectCount,
		ProviderEventBytes: view.Storage.ProviderEventBytes, ToolPayloadBytes: view.Storage.ToolPayloadBytes,
		Agents: agents, Panes: panes,
	})
	if err != nil {
		return fmt.Errorf("encode real-provider progress: %w", err)
	}
	if string(fingerprint) != watch.lastFingerprint {
		watch.lastFingerprint = string(fingerprint)
		watch.lastProgress = now
		return nil
	}
	if now.Sub(watch.lastProgress) >= watch.stallBudget {
		return fmt.Errorf("real-provider workflow made no observable progress for %s", watch.stallBudget)
	}
	return nil
}

func TestRealProviderProgressWatchRejectsBlockedActiveSession(t *testing.T) {
	name := "pika-baseline-draft"
	view := symphony.OperatorStatus{AgentSessions: []symphony.OperatorAgentSession{{
		ID: "session-1", WorkID: "work-1", Role: symphony.RoleBaselineDraft, AgentName: name, Status: symphony.AgentSessionRunning,
	}}}
	snapshot := herdr.Snapshot{Agents: []herdr.Agent{{Name: &name, AgentStatus: "blocked"}}}
	if err := newRealProviderProgressWatch(time.Unix(0, 0)).Observe(time.Unix(0, 0), view, snapshot); err == nil || !strings.Contains(err.Error(), "is blocked") {
		t.Fatalf("blocked active Session error = %v, want immediate blocked failure", err)
	}
}

func TestRealProviderProgressWatchResetsOnlyForObservableProgress(t *testing.T) {
	started := time.Unix(0, 0)
	watch := newRealProviderProgressWatch(started)
	view := symphony.OperatorStatus{DomainEventCount: 1}
	if err := watch.Observe(started, view, herdr.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if err := watch.Observe(started.Add(realProviderStallBudget-time.Second), view, herdr.Snapshot{}); err != nil {
		t.Fatalf("healthy stall budget failed early: %v", err)
	}
	view.DomainEventCount++
	progressed := started.Add(realProviderStallBudget - time.Second)
	if err := watch.Observe(progressed, view, herdr.Snapshot{}); err != nil {
		t.Fatalf("observable progress did not reset stall budget: %v", err)
	}
	if err := watch.Observe(progressed.Add(realProviderStallBudget), view, herdr.Snapshot{}); err == nil || !strings.Contains(err.Error(), "no observable progress") {
		t.Fatalf("stalled workflow error = %v, want no-progress failure", err)
	}
}

func exerciseRealCodexFollowUp(t *testing.T, ctx context.Context, stateRoot, holdPath string, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) realCodexSessionObservation {
	t.Helper()
	target := waitForRealProviderVerificationHold(t, ctx, stateRoot, client, socketPath, daemon, serverOutput)
	releaseRealCodexHold(t, holdPath)
	return waitForRealProviderFollowUp(t, ctx, stateRoot, target, client, socketPath, daemon, serverOutput)
}

func waitForRealProviderVerificationHold(t *testing.T, ctx context.Context, stateRoot string, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) realCodexSessionObservation {
	t.Helper()
	engine, err := symphony.Open(ctx, realProviderDatabasePath(stateRoot), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	watch := newRealProviderProgressWatch(time.Now())
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var target symphony.WorkView
	var targetSession symphony.AgentSession
	providerSessionID := ""
	readyPath := filepath.Dir(stateRoot) + "-verification-ready"
	for {
		view, statusErr := control.Status(ctx, socketPath)
		if statusErr != nil {
			t.Fatalf("observe real-provider status before Follow-up smoke: %v", statusErr)
		}
		snapshot, snapshotErr := client.Snapshot(ctx)
		if snapshotErr != nil {
			t.Fatalf("observe Herdr status before Follow-up smoke: %v", snapshotErr)
		}
		if progressErr := watch.Observe(time.Now(), view, snapshot); progressErr != nil {
			t.Fatalf("real-provider workflow failed before Follow-up smoke: %v; %s", progressErr, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
		}
		for _, work := range view.Works {
			if work.Role == symphony.RoleBaselineVerification && work.Status == symphony.WorkPending {
				target = work
				break
			}
		}
		if target.ID != "" {
			var found bool
			targetSession, _, found, err = engine.CurrentAgentSession(ctx, target.ID)
			if err != nil {
				t.Fatalf("read real-provider Verification Session: %v", err)
			}
			if found && targetSession.Status == symphony.AgentSessionRunning {
				journal, journalErr := engine.ConversationJournal(ctx, target.ID)
				if journalErr != nil {
					t.Fatalf("read real-provider Verification journal: %v", journalErr)
				}
				providerSessionID = journal.ProviderSessionID
			}
		}
		_, readyErr := os.Stat(readyPath)
		if targetSession.ID != "" && providerSessionID != "" && readyErr == nil {
			break
		}
		if readyErr != nil && !os.IsNotExist(readyErr) {
			t.Fatalf("read real-provider Verification gate: %v", readyErr)
		}
		select {
		case err := <-daemon.done:
			t.Fatalf("real-provider daemon exited before Follow-up smoke: %v; output=%s", err, daemon.output.String())
		case <-ctx.Done():
			t.Fatalf("real-provider workflow context ended before Follow-up smoke: %v; %s", ctx.Err(), diagnoseRealCodex(context.Background(), client, socketPath, daemon, serverOutput))
		case <-ticker.C:
		}
	}
	return realCodexSessionObservation{Work: target, Session: targetSession, ProviderSessionID: providerSessionID}
}

func waitForRealProviderFollowUp(t *testing.T, ctx context.Context, stateRoot string, target realCodexSessionObservation, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) realCodexSessionObservation {
	t.Helper()
	watch := newRealProviderProgressWatch(time.Now())
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		view, statusErr := control.Status(ctx, socketPath)
		if statusErr != nil {
			t.Fatalf("observe real-provider Follow-up status: %v", statusErr)
		}
		snapshot, snapshotErr := client.Snapshot(ctx)
		if snapshotErr != nil {
			t.Fatalf("observe Herdr Follow-up status: %v", snapshotErr)
		}
		if progressErr := watch.Observe(time.Now(), view, snapshot); progressErr != nil {
			t.Fatalf("real-provider Follow-up failed: %v; %s", progressErr, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
		}
		for _, followUp := range view.FollowUps {
			if followUp.TargetWorkID == target.Work.ID && followUp.Status == "delivered" {
				for _, work := range view.Works {
					if work.ID == target.Work.ID && work.Status != symphony.WorkPending {
						t.Fatalf("real Follow-up completed target Work: %+v", work)
					}
				}
				if followUp.TargetAgentSessionID != target.Session.ID {
					t.Fatalf("real Follow-up changed target Session: got %s, want %s", followUp.TargetAgentSessionID, target.Session.ID)
				}
				durable := inspectRealProviderView(t, ctx, realProviderDatabasePath(stateRoot))
				var durableFollowUp symphony.FollowUpView
				for _, candidate := range durable.FollowUps {
					if candidate.ID == followUp.ID {
						durableFollowUp = candidate
						break
					}
				}
				if durableFollowUp.Message == "" || durableFollowUp.TargetProviderTurnID == "" || durableFollowUp.TargetProviderTurnID == "pika-real-follow-up-stopped-turn" {
					t.Fatalf("real Follow-up was not durably armed by a natural provider Stop: %+v", durableFollowUp)
				}
				t.Logf("real %s Follow-up request %s delivered to Verification Work %s", target.Session.AgentKind, followUp.ID, target.Work.ID)
				return target
			}
		}
		select {
		case err := <-daemon.done:
			t.Fatalf("real-provider daemon exited during Follow-up smoke: %v; output=%s", err, daemon.output.String())
		case <-ctx.Done():
			t.Fatalf("real-provider Follow-up context ended: %v; %s", ctx.Err(), diagnoseRealCodex(context.Background(), client, socketPath, daemon, serverOutput))
		case <-ticker.C:
		}
	}
}

func crashRealCodexDaemon(t *testing.T, process *daemonProcess) string {
	t.Helper()
	if process == nil || process.command == nil || process.command.Process == nil {
		t.Fatal("real Codex daemon process is unavailable for crash injection")
	}
	pid := process.command.Process.Pid
	if health, err := control.Health(context.Background(), process.socketPath); err == nil && health.PID > 0 {
		pid = health.PID
	}
	owner, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find real Codex daemon %d: %v", pid, err)
	}
	if err := owner.Kill(); err != nil {
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
	engine, err := symphony.Open(ctx, realProviderDatabasePath(stateRoot), symphony.Options{})
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

func waitForRealDiagnosisComplete(t *testing.T, ctx context.Context, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		view, err := control.Status(ctx, socketPath)
		if err == nil && len(view.Diagnoses) == 1 {
			diagnosis := view.Diagnoses[0]
			if diagnosis.Status == symphony.DiagnosisReady || diagnosis.Status == symphony.DiagnosisUnavailable {
				// Flow v2 explicitly permits a factual profiler/hardware failure to
				// complete Diagnosis as unavailable.  Treating it as a test failure
				// would contradict the contract and would make this CPU fixture
				// fabricate profiler evidence merely to reach Iteration.
				t.Logf("real %s Diagnosis %s completed as %s with %d hypotheses", diagnosis.WorkID, diagnosis.ID, diagnosis.Status, diagnosis.HypothesisCount)
				return
			}
			if diagnosis.Status == symphony.DiagnosisCancelled {
				t.Fatalf("real Diagnosis reached terminal non-ready state: %+v; %s", diagnosis, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
			}
		}
		select {
		case err := <-daemon.done:
			t.Fatalf("daemon exited while waiting for real Diagnosis: %v; output=%s", err, daemon.output.String())
		case <-ctx.Done():
			t.Fatalf("real Diagnosis context ended: %v; %s", ctx.Err(), diagnoseRealCodex(context.Background(), client, socketPath, daemon, serverOutput))
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("real Diagnosis did not complete within its release budget: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
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
	// An Integration session cannot start until the surviving Iteration has
	// terminally projected its Candidate.  That is a separate real-provider
	// phase, so do not spend the session-start budget while the Iteration is
	// still making observable tool progress.
	waitForRealCodexIntegrationProjection(t, ctx, client, socketPath, daemon, serverOutput)
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
			if backedOff && successor && durableIntegrationBackOff(ctx, realProviderDatabasePath(stateRoot), message) {
				releaseRealCodexHold(t, holdPath)
				t.Logf("backed off real Integration Work %s to a fresh Iteration round", target.Work.ID)
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real Integration Back-off did not create its successor Iteration: %s", diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
}

func waitForRealCodexIntegrationProjection(t *testing.T, ctx context.Context, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) {
	t.Helper()
	watch := newRealProviderProgressWatch(time.Now())
	for {
		view, statusErr := control.Status(ctx, socketPath)
		snapshot, snapshotErr := client.Snapshot(ctx)
		if statusErr == nil && snapshotErr == nil {
			if progressErr := watch.Observe(time.Now(), view, snapshot); progressErr != nil {
				t.Fatalf("real Iteration made no healthy progress before Integration projection: %v; %s", progressErr, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
			}
			for _, work := range view.Works {
				if work.Role == symphony.RoleIntegration && work.Status == symphony.WorkPending {
					return
				}
			}
		}
		select {
		case err := <-daemon.done:
			t.Fatalf("daemon exited before Integration projection: %v; output=%s", err, daemon.output.String())
		case <-ctx.Done():
			t.Fatalf("real Integration projection timed out: %s", diagnoseRealCodex(context.Background(), client, socketPath, daemon, serverOutput))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func waitForRealCodexRoleSessions(t *testing.T, ctx context.Context, stateRoot string, role symphony.WorkRole, count int, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) []realCodexSessionObservation {
	t.Helper()
	engine, err := symphony.Open(ctx, realProviderDatabasePath(stateRoot), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	watch := newRealProviderProgressWatch(time.Now())
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		view, statusErr := control.Status(ctx, socketPath)
		if statusErr != nil {
			t.Fatalf("observe real-provider status while waiting for %s Sessions: %v", role, statusErr)
		}
		snapshot, snapshotErr := client.Snapshot(ctx)
		if snapshotErr != nil {
			t.Fatalf("observe Herdr status while waiting for %s Sessions: %v", role, snapshotErr)
		}
		if progressErr := watch.Observe(time.Now(), view, snapshot); progressErr != nil {
			t.Fatalf("real-provider workflow failed while waiting for %s Sessions: %v; %s", role, progressErr, diagnoseRealCodex(ctx, client, socketPath, daemon, serverOutput))
		}
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
		select {
		case err := <-daemon.done:
			t.Fatalf("real-provider daemon exited while waiting for %s Sessions: %v; output=%s", role, err, daemon.output.String())
		case <-ctx.Done():
			t.Fatalf("real-provider context ended while waiting for %d %s Sessions: %v; %s", count, role, ctx.Err(), diagnoseRealCodex(context.Background(), client, socketPath, daemon, serverOutput))
		case <-ticker.C:
		}
	}
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

func inspectRealProviderView(t *testing.T, ctx context.Context, databasePath string) symphony.View {
	t.Helper()
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("open durable real-provider status: %v", err)
	}
	defer engine.Close()
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect durable real-provider status: %v", err)
	}
	return view
}

func durableIntegrationBackOff(ctx context.Context, databasePath, message string) bool {
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		return false
	}
	defer engine.Close()
	view, err := engine.Inspect(ctx, symphony.Status{})
	return err == nil && hasIntegrationBackOff(view, message)
}

// assertRealFlowV2Contract is deliberately run against the persisted daemon
// database, not a fabricated View. It keeps the credentialed Codex, Cursor,
// and mirror-provider gates honest about the immutable data that crosses
// provider boundaries.
func assertRealFlowV2Contract(t *testing.T, ctx context.Context, databasePath string) {
	t.Helper()
	view := inspectRealProviderView(t, ctx, databasePath)
	if view.Optimization.FlowVersion != symphony.FlowVersion2 || view.SkillSnapshot == nil || len(view.SkillSnapshot.Entries) != 2 {
		t.Fatalf("real provider did not retain flow-v2 frozen skills: flow=%d snapshot=%+v", view.Optimization.FlowVersion, view.SkillSnapshot)
	}
	if len(view.Diagnoses) != 1 || (view.Diagnoses[0].Status != symphony.DiagnosisReady && view.Diagnoses[0].Status != symphony.DiagnosisUnavailable) {
		t.Fatalf("real provider did not complete Diagnosis: %+v", view.Diagnoses)
	}
	if view.Diagnoses[0].Status == symphony.DiagnosisReady && view.Diagnoses[0].HypothesisCount == 0 {
		t.Fatalf("ready real Diagnosis has no hypotheses: %+v", view.Diagnoses[0])
	}
	kept := map[string]symphony.IterationExperimentView{}
	ids := map[string]bool{}
	for _, experiment := range view.IterationExperiments {
		if experiment.ID == "" || ids[experiment.ID] || experiment.ParentCheckpointSHA == "" {
			t.Fatalf("invalid real Experiment ledger: %+v", view.IterationExperiments)
		}
		ids[experiment.ID] = true
		if experiment.Outcome == "kept" {
			if experiment.CheckpointSHA == "" {
				t.Fatalf("kept real Experiment has no checkpoint: %+v", experiment)
			}
			kept[experiment.CheckpointSHA] = experiment
		}
	}
	if len(kept) == 0 {
		t.Fatalf("real provider never recorded a kept Experiment: %+v", view.IterationExperiments)
	}
	accepted := false
	for _, integration := range view.Integrations {
		if integration.Status == "accepted" {
			accepted = true
			if _, found := kept[integration.CandidateSHA]; !found {
				t.Fatalf("accepted Integration %s has no matching kept Experiment: %+v", integration.ID, view.IterationExperiments)
			}
		}
	}
	if !accepted {
		t.Fatal("real provider recorded no accepted Integration")
	}
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("open real flow-v2 database for Context verification: %v", err)
	}
	defer engine.Close()
	contextRoles := map[symphony.WorkRole]bool{}
	workStatuses := map[string]symphony.WorkStatus{}
	for _, work := range view.Works {
		workStatuses[work.ID] = work.Status
	}
	for _, session := range view.AgentSessions {
		snapshot, found, err := engine.ReadContextSnapshot(ctx, session.ID)
		if err != nil {
			t.Fatalf("real %s Context snapshot = %+v found=%v err=%v", session.Role, snapshot, found, err)
		}
		events, err := engine.ProviderEvents(ctx, session.ID)
		if err != nil {
			t.Fatalf("read real %s provider events for Context verification: %v", session.Role, err)
		}
		binding, bound, err := engine.AgentSessionBinding(ctx, session.ID)
		if err != nil {
			t.Fatalf("read real %s activation binding for Context verification: %v", session.Role, err)
		}
		if !found {
			// EnsureAgentSession intentionally precedes activation so a scheduler
			// cancellation can leave a never-launched starting record. Once any
			// durable pane binding or provider event exists, however, Context must
			// already be frozen.
			if bound || len(events) != 0 {
				t.Fatalf("real %s Session reached the provider without a Context v4 snapshot: bound=%v binding=%+v events=%d", session.Role, bound, binding, len(events))
			}
			continue
		}
		if snapshot.SchemaVersion != 4 {
			t.Fatalf("real %s Context snapshot schema=%d, want 4", session.Role, snapshot.SchemaVersion)
		}
		if !bound && len(events) != 0 {
			t.Fatalf("real %s Session journal has no durable activation binding: binding=%+v events=%d", session.Role, binding, len(events))
		}
		if bound && len(events) == 0 {
			if workStatuses[session.WorkID] != symphony.WorkCancelled {
				t.Fatalf("real %s Session activation has no provider journal outside a cancelled drain: binding=%+v work_status=%s", session.Role, binding, workStatuses[session.WorkID])
			}
		}
		if bound && len(events) != 0 {
			contextRoles[session.Role] = true
		}
		// ContextRelativePath is relative to the dedicated contexts root, not
		// the SQLite directory. Keeping this explicit makes the credentialed
		// assertion verify the same artifact layout Activation publishes.
		contents, err := os.ReadFile(filepath.Join(filepath.Dir(databasePath), "contexts", snapshot.ContextRelativePath))
		if err != nil {
			t.Fatalf("read real %s Context file: %v", session.Role, err)
		}
		digest := sha256.Sum256(contents)
		if hex.EncodeToString(digest[:]) != snapshot.ContextSHA256 {
			t.Fatalf("real %s Context digest differs from persisted snapshot", session.Role)
		}
		var document struct {
			SkillSnapshot *struct {
				SnapshotID string `json:"snapshot_id"`
				Entries    []struct {
					Name string `json:"name"`
					Path string `json:"path"`
				} `json:"entries"`
				Manifest struct {
					Path   string `json:"path"`
					SHA256 string `json:"sha256"`
				} `json:"manifest"`
			} `json:"skill_snapshot"`
			Diagnosis *struct {
				ID         string `json:"id"`
				Status     string `json:"status"`
				Hypotheses int64  `json:"hypothesis_count"`
				Report     *struct {
					Path string `json:"path"`
				} `json:"report"`
			} `json:"diagnosis"`
		}
		if err := json.Unmarshal(contents, &document); err != nil || document.SkillSnapshot == nil || document.SkillSnapshot.SnapshotID != view.SkillSnapshot.SnapshotID || len(document.SkillSnapshot.Entries) != 2 || document.SkillSnapshot.Manifest.Path == "" || len(document.SkillSnapshot.Manifest.SHA256) != 64 {
			t.Fatalf("real %s Context does not reference the optimization's frozen skill snapshot: snapshot=%+v err=%v", session.Role, document.SkillSnapshot, err)
		}
		if document.SkillSnapshot.Entries[0].Name != "KernelWiki" || document.SkillSnapshot.Entries[0].Path != "skills/KernelWiki" || document.SkillSnapshot.Entries[1].Name != "ncu-report-skill" || document.SkillSnapshot.Entries[1].Path != "skills/ncu-report-skill" {
			t.Fatalf("real %s Context skill provenance is not the canonical path wire contract: %+v", session.Role, document.SkillSnapshot.Entries)
		}
		if document.Diagnosis != nil && (document.Diagnosis.ID == "" || document.Diagnosis.Status == "") {
			t.Fatalf("real %s Context has incomplete Diagnosis identity: %+v", session.Role, document.Diagnosis)
		}
		if document.Diagnosis != nil && document.Diagnosis.Report != nil {
			report, err := os.ReadFile(document.Diagnosis.Report.Path)
			if err != nil {
				t.Fatalf("read real %s canonical Diagnosis artifact: %v", session.Role, err)
			}
			if err := kdacontract.ValidateDiagnosisReport(report); err != nil {
				t.Fatalf("real %s Diagnosis artifact violates canonical schema: %v", session.Role, err)
			}
			var canonical struct {
				Hypotheses []json.RawMessage `json:"hypotheses"`
			}
			if err := json.Unmarshal(report, &canonical); err != nil {
				t.Fatalf("parse real %s canonical Diagnosis artifact: %v", session.Role, err)
			}
			if document.Diagnosis.Hypotheses != int64(len(canonical.Hypotheses)) {
				t.Fatalf("real %s Context Diagnosis hypothesis_count=%d differs from canonical artifact count=%d", session.Role, document.Diagnosis.Hypotheses, len(canonical.Hypotheses))
			}
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(contents, &wire); err != nil {
			t.Fatal(err)
		}
		var work map[string]json.RawMessage
		if err := json.Unmarshal(wire["work"], &work); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"skill_snapshot", "diagnosis", "iteration_experiments"} {
			if _, found := work[forbidden]; found {
				t.Fatalf("real %s context.json inlined %s instead of an artifact reference", session.Role, forbidden)
			}
		}
	}
	for _, role := range []symphony.WorkRole{symphony.RoleBaselineDraft, symphony.RoleBaselineVerification, symphony.RoleDiagnosis, symphony.RoleIteration, symphony.RoleIntegration} {
		if !contextRoles[role] {
			t.Fatalf("real provider matrix has no Context v4 snapshot for required role %s", role)
		}
	}
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

func waitForRealCodexBest(t *testing.T, ctx context.Context, client *herdr.Client, socketPath string, daemon *daemonProcess, serverOutput *bytes.Buffer) symphony.OperatorStatus {
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
		if encoded, encodeErr := json.MarshalIndent(view, "", "  "); encodeErr == nil {
			_, _ = fmt.Fprintf(&report, "view=%s\n", encoded)
		} else {
			_, _ = fmt.Fprintf(&report, "view_encode_error=%v\n", encodeErr)
		}
	} else {
		_, _ = fmt.Fprintf(&report, "status_error=%v\n", err)
	}
	if snapshot, err := client.Snapshot(ctx); err == nil {
		if encoded, encodeErr := json.MarshalIndent(snapshot, "", "  "); encodeErr == nil {
			_, _ = fmt.Fprintf(&report, "snapshot=%s\n", encoded)
		} else {
			_, _ = fmt.Fprintf(&report, "snapshot_encode_error=%v\n", encodeErr)
		}
		for _, pane := range snapshot.Panes {
			for _, source := range []string{"visible", "detection", "recent_unwrapped"} {
				var read struct {
					Read struct {
						Text string `json:"text"`
					} `json:"read"`
				}
				readErr := client.Call(ctx, "pane.read", map[string]any{"pane_id": pane.PaneID, "source": source, "lines": 160, "format": "text"}, &read)
				_, _ = fmt.Fprintf(&report, "pane %s (%s, err=%v):\n%s\n", pane.PaneID, source, readErr, read.Read.Text)
			}
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

Correctness is exact integer equality for both cases. The primary metric is logical_operations, lower is better, reported independently for every case by the real Candidate implementation. There are no warmups or stochastic repeats because the metric is deterministic. The CPU process is this fixture's device domain: harness.py owns immutable canonical inputs and oracle outputs, exposes only stable working buffers to Candidate, restores before every invocation, validates after every invocation, and emits compact integrity counters only after the run. Baseline Verification must establish the Development Baseline values (1000 and 10000 operations) and verify the protocol; it must not require the Development Baseline to improve over itself. Candidate acceptance during Iteration and Integration requires correctness plus at least 90% fewer logical operations than those baseline values on every case, with no guard regression. The stop condition is a Best implementation reporting at most one logical operation per case.

The intended safe optimization is the arithmetic-series closed form. harness.py, its oracle, the cases, and the protocol are frozen after Baseline acceptance. Baseline Draft may add and commit a JSON Definition. Iteration must commit the Candidate and leave its worktree clean. Integration must use Pika's two-phase Git Intent. Preserve command output as evidence.
`,
		"candidate.py": `def sum_to_n(n):
    n = int(n)
    total = 0
    for value in range(1, n + 1):
        total += value
    return total, n
`,
		"harness.py": `import argparse
import json

import candidate

CASES = {"small": 1000, "large": 10000}


class WorkingInput:
    """A stable CPU-device buffer; canonical input stays private to this harness."""
    def __init__(self): self.value = 0
    def __int__(self): return self.value
    __index__ = __int__


def invoke(name):
    n, canonical, expected = CASES[name], (CASES[name],), (CASES[name] * (CASES[name] + 1) // 2,)
    working = WorkingInput()
    stats = {"warmup_invocations": 0, "measured_invocations": 1, "input_restores": 0, "checked_invocations": 0, "mismatches": 0, "nonfinite": 0, "canonical_input_mutations": 0, "tolerance_passed": True}
    # The CPU process is this fixture's device domain. Candidate sees only the
    # working buffer; compact counters are returned after validation.
    working.value = canonical[0]
    stats["input_restores"] += 1
    actual, operations = candidate.sum_to_n(working)
    if not isinstance(actual, int):
        stats["nonfinite"] += 1
        stats["tolerance_passed"] = False
    elif actual != expected[0]:
        stats["mismatches"] += 1
        stats["tolerance_passed"] = False
    stats["checked_invocations"] += 1
    if canonical != (n,):
        stats["canonical_input_mutations"] += 1
        stats["tolerance_passed"] = False
    if not stats["tolerance_passed"]:
        raise SystemExit(1)
    return actual, operations, stats


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["correctness", "benchmark"])
    parser.add_argument("--cases", nargs="+", required=True)
    args = parser.parse_args()
    if not args.cases or any(name not in CASES for name in args.cases):
        raise SystemExit("cases must be a non-empty subset of small, large")
    rows, integrity = [], {}
    for name in args.cases:
        actual, operations, stats = invoke(name)
        integrity[name] = stats
        if args.mode == "correctness":
            rows.append({"case": name, "ok": True, "actual": actual})
        else:
            rows.append({"case": name, "metric": "logical_operations", "unit": "operations", "direction": "lower", "value": operations})
    print(json.dumps({"mode": args.mode, "results": rows, "integrity": integrity}, sort_keys=True))


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
