package workruntime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/cli"
	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/testdriver"
)

func TestDaemonRestartCreatesFreshSessionMoveAndLostPaneReplacement(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatalf("find herdr: %v", err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	if fakeAgent == "" || !filepath.IsAbs(fakeAgent) {
		t.Fatal("PIKA_GO_FAKE_AGENT_BIN must be an absolute path")
	}

	root, err := os.MkdirTemp("/tmp", "pika-go-daemon-int-")
	if err != nil {
		t.Fatalf("create integration root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout = &serverOutput
	server.Stderr = &serverOutput
	if err := server.Start(); err != nil {
		t.Fatalf("start Herdr: %v", err)
	}
	t.Cleanup(func() {
		stopServer()
		_ = server.Wait()
	})
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "pika-daemon-test", "focus": false}, &created); err != nil {
		t.Fatalf("create workspace: %v; output=%s", err, serverOutput.String())
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatalf("create init pane: %v", err)
	}

	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot := filepath.Join(root, "pika-state")
	configRoot := filepath.Join(root, "pika-config")
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_PANE_ID", created.RootPane.PaneID)
	t.Setenv("PIKA_GO_AGENT_PATH_PREFIX", binDir)

	stopDaemon := startTestDaemon(t, pikaSocket, stateRoot, configRoot)
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{
		Mutation:          protocol.Mutation{RequestID: "init"},
		Repository:        repository,
		CallerPaneID:      initPane.PaneID,
		ConfigurationTOML: fixtureConfiguration(repository),
	}); err != nil {
		t.Fatalf("initialize daemon: %v", err)
	}
	view, err := control.Status(ctx, pikaSocket)
	if err != nil {
		t.Fatalf("status after init: %v", err)
	}
	workID := view.Works[0].ID
	first := waitForNamedAgent(t, ctx, client, "")
	var firstSession symphony.AgentSession
	waitForBinding(t, ctx, stateRoot, workID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		firstSession = session
		return binding.TerminalID == first.TerminalID
	})
	herdrRuntime := herdr.NewRuntime(client)
	if _, err := herdrRuntime.Prompt(ctx, first.PaneID, "/hang"); err != nil {
		t.Fatalf("make fake Agent busy before Scheduler pause: %v", err)
	}
	if err := herdrRuntime.SendAgentKeys(ctx, agentNameValue(first), []string{"enter"}); err != nil {
		t.Fatalf("submit fake Agent hang prompt: %v", err)
	}
	waitForHerdrAgentStatus(t, ctx, client, agentNameValue(first), "working")
	pause, err := control.PauseScheduler(ctx, pikaSocket, protocol.SchedulerControlRequest{Mutation: protocol.Mutation{RequestID: "pause"}})
	if err != nil {
		t.Fatalf("pause Scheduler through real Herdr: %v", err)
	}
	if pause.Control.HasDeliveryFailure() || len(pause.Control.Actions) != 1 || pause.Control.Actions[0].Status != symphony.SchedulerActionSent {
		t.Fatalf("real Herdr pause cycle=%+v", pause.Control)
	}
	waitForPaneText(t, ctx, client, first.PaneID, "FAKE_AGENT_INTERRUPTED")
	waitForHerdrAgentStatus(t, ctx, client, agentNameValue(first), "idle")
	resume, err := control.ResumeScheduler(ctx, pikaSocket, protocol.SchedulerControlRequest{Mutation: protocol.Mutation{RequestID: "resume"}})
	if err != nil {
		t.Fatalf("resume Scheduler through real Herdr: %v", err)
	}
	if resume.Control.HasDeliveryFailure() || len(resume.Control.Actions) != 1 ||
		resume.Control.Actions[0].AgentSessionID != firstSession.ID || resume.Control.Actions[0].Status != symphony.SchedulerActionSent {
		t.Fatalf("real Herdr resume cycle=%+v original Session=%s", resume.Control, firstSession.ID)
	}
	waitForPaneText(t, ctx, client, first.PaneID, `FAKE_AGENT_PROMPT "继续"`)
	current, _, found, err := readCurrentSession(stateRoot, workID)
	if err != nil || !found || current.ID != firstSession.ID {
		t.Fatalf("Scheduler control replaced Session: current=%+v found=%v err=%v", current, found, err)
	}
	stopDaemon()
	if _, err := herdr.NewRuntime(client).GetAgent(ctx, first.PaneID); err != nil {
		t.Fatalf("agent did not survive daemon crash: %v", err)
	}

	stopDaemon = startTestDaemon(t, pikaSocket, stateRoot, configRoot)
	defer stopDaemon()
	recovered := waitForNamedAgent(t, ctx, client, agentNameValue(first))
	if recovered.TerminalID == first.TerminalID {
		t.Fatalf("daemon recovery resumed old terminal %s", first.TerminalID)
	}
	var recoveredSession symphony.AgentSession
	waitForBinding(t, ctx, stateRoot, workID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		recoveredSession = session
		return session.ID != firstSession.ID && binding.TerminalID == recovered.TerminalID
	})

	var moved struct {
		MoveResult struct {
			Pane herdr.Pane `json:"pane"`
		} `json:"move_result"`
	}
	if err := client.Call(ctx, "pane.move", map[string]any{
		"pane_id":     recovered.PaneID,
		"destination": map[string]any{"type": "new_workspace", "label": "moved", "tab_label": "main"},
		"focus":       false,
	}, &moved); err != nil {
		t.Fatalf("move managed pane: %v", err)
	}
	waitForBinding(t, ctx, stateRoot, workID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		return session.ID == recoveredSession.ID && binding.TerminalID == recovered.TerminalID && binding.PaneID == moved.MoveResult.Pane.PaneID
	})

	if err := herdr.NewRuntime(client).ClosePane(ctx, moved.MoveResult.Pane.PaneID); err != nil {
		t.Fatalf("close managed pane: %v", err)
	}
	replacement := waitForNamedAgent(t, ctx, client, agentNameValue(recovered))
	if replacement.TerminalID == recovered.TerminalID {
		t.Fatalf("replacement reused terminal %s", recovered.TerminalID)
	}
	waitForBinding(t, ctx, stateRoot, workID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		return session.ID != recoveredSession.ID && binding.TerminalID == replacement.TerminalID
	})

	stopDaemon()
	stopDaemon = func() {}
	if _, err := herdr.NewRuntime(client).GetAgent(ctx, replacement.PaneID); err != nil {
		t.Fatalf("replacement agent did not survive daemon shutdown: %v", err)
	}
	snapshot, _ := client.Snapshot(ctx)
	for _, workspace := range snapshot.Workspaces {
		_ = client.Call(ctx, "workspace.close", map[string]string{"workspace_id": workspace.WorkspaceID}, nil)
	}
}

func TestGracefulShutdownWaitsForRealHerdrAgentTerminalMCP(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	if fakeAgent == "" || !filepath.IsAbs(fakeAgent) {
		t.Fatal("PIKA_GO_FAKE_AGENT_BIN must be an absolute path")
	}
	root, err := os.MkdirTemp("/tmp", "pika-go-drain-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "drain-e2e", "focus": false}, &created); err != nil {
		t.Fatal(err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_PANE_ID", created.RootPane.PaneID)
	t.Setenv("PIKA_GO_AGENT_PATH_PREFIX", binDir)
	t.Setenv("CODEX_HOME", filepath.Join(configRoot, "codex-home"))
	t.Setenv("PIKA_GO_CODEX_EXECUTABLE", "/bin/echo")
	daemonCtx, stopDaemon := context.WithCancel(context.Background())
	t.Cleanup(stopDaemon)
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- cli.Run(daemonCtx, []string{"daemon", "--socket", pikaSocket, "--state-dir", stateRoot, "--config-dir", configRoot, "--instance", "integration-instance"}, nil, io.Discard, io.Discard)
	}()
	waitForPikaHealth(t, pikaSocket, daemonDone)
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository)}); err != nil {
		t.Fatal(err)
	}
	agent := waitForNamedAgent(t, ctx, client, "")
	assertPaneLacksText(t, ctx, client, agent.PaneID, "FAKE_AGENT_PROMPT")
	if _, err := control.Shutdown(ctx, pikaSocket, protocol.ShutdownRequest{Mutation: protocol.Mutation{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-daemonDone:
		t.Fatalf("daemon exited before active Agent terminal MCP: code=%d", code)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := control.Health(ctx, pikaSocket); err != nil {
		t.Fatalf("draining daemon is not reachable: %v", err)
	}
	if _, err := herdr.NewRuntime(client).Prompt(ctx, agent.PaneID, "/finish"); err != nil {
		t.Fatalf("let Agent finish normally: %v", err)
	}
	select {
	case code := <-daemonDone:
		if code != 0 {
			t.Fatalf("drained daemon exit code = %d", code)
		}
	case <-time.After(10 * time.Second):
		view, statusErr := control.Status(context.Background(), pikaSocket)
		var active []symphony.ActiveAgentSession
		reader, openErr := symphony.Open(context.Background(), filepath.Join(stateRoot, "instances", "integration-instance", "pika.db"), symphony.Options{})
		if openErr == nil {
			active, openErr = reader.ActiveAgentSessions(context.Background())
			_ = reader.Close()
		}
		var paneRead json.RawMessage
		paneErr := client.Call(context.Background(), "pane.read", map[string]any{"pane_id": agent.PaneID, "source": "recent", "lines": 100, "format": "text"}, &paneRead)
		t.Fatalf("daemon did not exit after terminal MCP; view=%+v statusErr=%v active=%+v dbErr=%v pane=%s paneErr=%v Herdr=%s", view, statusErr, active, openErr, paneRead, paneErr, serverOutput.String())
	}
	if _, err := os.Lstat(pikaSocket); !os.IsNotExist(err) {
		t.Fatalf("daemon socket remains after drain: %v", err)
	}
	_ = client.Call(ctx, "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
}

func TestDaemonProcessCrashReplacesRunningHerdrAgentAndRecoversWork(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	pikaBinary := os.Getenv("PIKA_GO_BIN")
	if fakeAgent == "" || !filepath.IsAbs(fakeAgent) || pikaBinary == "" || !filepath.IsAbs(pikaBinary) {
		t.Fatal("PIKA_GO_FAKE_AGENT_BIN and PIKA_GO_BIN must be absolute paths")
	}
	root, err := os.MkdirTemp("/tmp", "pika-go-process-crash-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "process-crash-e2e", "focus": false}, &created); err != nil {
		t.Fatal(err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	databasePath := filepath.Join(stateRoot, "instances", "integration-instance", "pika.db")

	var daemon *daemonProcess
	t.Cleanup(func() {
		if daemon != nil {
			daemon.killAndWait()
		}
		_ = client.Call(context.Background(), "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
	})
	daemon = startDaemonProcess(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, binDir)
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{
		Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository),
	}); err != nil {
		t.Fatal(err)
	}
	view, err := control.Status(ctx, pikaSocket)
	if err != nil || len(view.Works) != 1 {
		t.Fatalf("status after init: view=%+v err=%v", view, err)
	}
	workID := view.Works[0].ID
	var crashStdout, crashStderr bytes.Buffer
	if code := testdriver.Run(ctx, []string{
		"kill-at", "--pid", strconv.Itoa(daemon.command.Process.Pid), "--database", databasePath,
		"--point", "agent-session-running", "--work-id", workID, "--timeout", "15s",
	}, &crashStdout, &crashStderr); code != 0 {
		t.Fatalf("crash driver exit=%d stdout=%q stderr=%q daemon=%s", code, crashStdout.String(), crashStderr.String(), daemon.output.String())
	}
	if err := <-daemon.done; err == nil {
		t.Fatal("crashed daemon exited successfully")
	}
	daemon = nil
	oldAgent := waitForNamedAgent(t, ctx, client, "")

	daemon = startDaemonProcess(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, binDir)
	replacement := waitForNamedAgent(t, ctx, client, agentNameValue(oldAgent))
	if replacement.TerminalID == oldAgent.TerminalID {
		t.Fatalf("recovery reused crashed Session terminal %s", oldAgent.TerminalID)
	}
	waitForBinding(t, ctx, stateRoot, workID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		return session.ID != "" && binding.TerminalID == replacement.TerminalID
	})
	if _, err := herdr.NewRuntime(client).GetAgent(ctx, oldAgent.PaneID); err == nil {
		t.Fatalf("old Agent still owns pane %s after recovery", oldAgent.PaneID)
	}
	assertPaneLacksText(t, ctx, client, replacement.PaneID, "FAKE_AGENT_PROMPT")
	if _, err := control.Shutdown(ctx, pikaSocket, protocol.ShutdownRequest{Mutation: protocol.Mutation{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := herdr.NewRuntime(client).Prompt(ctx, replacement.PaneID, "/finish"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-daemon.done:
		if err != nil {
			t.Fatalf("recovered daemon drain exit: %v output=%s", err, daemon.output.String())
		}
		daemon = nil
	case <-time.After(10 * time.Second):
		t.Fatalf("recovered daemon did not drain: %s", daemon.output.String())
	}
	if _, err := os.Lstat(pikaSocket); !os.IsNotExist(err) {
		t.Fatalf("daemon socket remains after recovered Work completed: %v", err)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range snapshot.Agents {
		if agent.Name != nil && strings.HasPrefix(*agent.Name, "pika-") {
			t.Fatalf("orphan Pika Agent after recovered drain: %+v", agent)
		}
	}
}

func TestDaemonProcessCrashAtDispatchingOutboxReconcilesWithoutDuplicateAgent(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	pikaBinary := os.Getenv("PIKA_GO_BIN")
	if fakeAgent == "" || !filepath.IsAbs(fakeAgent) || pikaBinary == "" || !filepath.IsAbs(pikaBinary) {
		t.Fatal("PIKA_GO_FAKE_AGENT_BIN and PIKA_GO_BIN must be absolute paths")
	}
	root, err := os.MkdirTemp("/tmp", "pika-go-outbox-crash-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PIKA_GO_FAKE_AGENT_START_DELAY": "2s",
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "outbox-crash-e2e", "focus": false}, &created); err != nil {
		t.Fatal(err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	databasePath := filepath.Join(stateRoot, "instances", "integration-instance", "pika.db")

	var daemon *daemonProcess
	t.Cleanup(func() {
		if daemon != nil {
			daemon.killAndWait()
		}
		_ = client.Call(context.Background(), "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
	})
	daemon = startDaemonProcess(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, binDir)
	var crashStdout, crashStderr bytes.Buffer
	driverDone := make(chan int, 1)
	go func() {
		driverDone <- testdriver.Run(ctx, []string{
			"kill-at", "--pid", strconv.Itoa(daemon.command.Process.Pid), "--database", databasePath,
			"--point", "outbox-dispatching", "--effect-type", "work.start_requested", "--timeout", "15s",
		}, &crashStdout, &crashStderr)
	}()
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{
		Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository),
	}); err != nil {
		t.Fatal(err)
	}
	if code := <-driverDone; code != 0 {
		t.Fatalf("crash driver exit=%d stdout=%q stderr=%q daemon=%s", code, crashStdout.String(), crashStderr.String(), daemon.output.String())
	}
	if err := <-daemon.done; err == nil {
		t.Fatal("crashed daemon exited successfully")
	}
	daemon = nil
	reader, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	viewBeforeRestart, err := reader.Inspect(ctx, symphony.Status{})
	activeBeforeRestart, activeErr := reader.ActiveAgentSessions(ctx)
	_ = reader.Close()
	if err != nil || activeErr != nil || len(viewBeforeRestart.Works) != 1 {
		t.Fatalf("inspect uncertain outbox state: view=%+v active=%+v err=%v activeErr=%v", viewBeforeRestart, activeBeforeRestart, err, activeErr)
	}
	workID := viewBeforeRestart.Works[0].ID
	oldSessionID := ""
	if len(activeBeforeRestart) != 0 {
		oldSessionID = activeBeforeRestart[0].Session.ID
	}

	daemon = startDaemonProcess(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, binDir)
	var recoveredSession symphony.AgentSession
	var recoveredBinding symphony.PaneBinding
	waitForBinding(t, ctx, stateRoot, workID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		recoveredSession, recoveredBinding = session, binding
		return session.Status == symphony.AgentSessionRunning && binding.TerminalID != "" && (oldSessionID == "" || session.ID != oldSessionID)
	})
	var replacement herdr.Agent
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		replacement, err = herdr.NewRuntime(client).GetAgent(ctx, recoveredBinding.PaneID)
		if err == nil && replacement.InteractiveReady {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || !replacement.InteractiveReady {
		t.Fatalf("recovered outbox Agent is not ready: session=%+v binding=%+v agent=%+v err=%v", recoveredSession, recoveredBinding, replacement, err)
	}
	view, err := control.Status(ctx, pikaSocket)
	if err != nil || len(view.Works) != 1 || view.Works[0].Status != symphony.WorkPending {
		t.Fatalf("recovered outbox duplicated or lost Work: view=%+v err=%v", view, err)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pikaAgents := 0
	for _, agent := range snapshot.Agents {
		if agent.Name != nil && strings.HasPrefix(*agent.Name, "pika-") {
			pikaAgents++
		}
	}
	if pikaAgents != 1 {
		t.Fatalf("recovered outbox has %d Pika Agents, want 1: %+v", pikaAgents, snapshot.Agents)
	}
	assertPaneLacksText(t, ctx, client, replacement.PaneID, "FAKE_AGENT_PROMPT")
	if _, err := control.Shutdown(ctx, pikaSocket, protocol.ShutdownRequest{Mutation: protocol.Mutation{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := herdr.NewRuntime(client).Prompt(ctx, replacement.PaneID, "/finish"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-daemon.done:
		if err != nil {
			t.Fatalf("recovered daemon drain exit: %v output=%s", err, daemon.output.String())
		}
		daemon = nil
	case <-time.After(10 * time.Second):
		t.Fatalf("recovered daemon did not drain: %s", daemon.output.String())
	}
}

func TestDaemonProcessCrashAfterTerminalCommitRecoversSingleSuccessor(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	pikaBinary := os.Getenv("PIKA_GO_BIN")
	if fakeAgent == "" || !filepath.IsAbs(fakeAgent) || pikaBinary == "" || !filepath.IsAbs(pikaBinary) {
		t.Fatal("PIKA_GO_FAKE_AGENT_BIN and PIKA_GO_BIN must be absolute paths")
	}
	root, err := os.MkdirTemp("/tmp", "pika-go-terminal-crash-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PIKA_GO_FAKE_AGENT_AUTORUN": "baseline-follow-up",
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "terminal-crash-e2e", "focus": false}, &created); err != nil {
		t.Fatal(err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	databasePath := filepath.Join(stateRoot, "instances", "integration-instance", "pika.db")

	var daemon *daemonProcess
	t.Cleanup(func() {
		if daemon != nil {
			daemon.killAndWait()
		}
		_ = client.Call(context.Background(), "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
	})
	daemon = startDaemonProcess(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, binDir)
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{
		Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository),
	}); err != nil {
		t.Fatal(err)
	}
	view, err := control.Status(ctx, pikaSocket)
	if err != nil || len(view.Works) != 1 {
		t.Fatalf("status after init: view=%+v err=%v", view, err)
	}
	draftWorkID := view.Works[0].ID
	var draftSession symphony.AgentSession
	waitForBinding(t, ctx, stateRoot, draftWorkID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		draftSession = session
		return session.Status == symphony.AgentSessionRunning && binding.TerminalID != ""
	})
	promptInitialBaselineFixture(t, ctx, client)
	terminalRequestID := "fake-baseline-submit-" + draftSession.ID
	var crashStdout, crashStderr bytes.Buffer
	if code := testdriver.Run(ctx, []string{
		"kill-at", "--pid", strconv.Itoa(daemon.command.Process.Pid), "--database", databasePath,
		"--point", "terminal-committed", "--request-id", terminalRequestID, "--timeout", "15s",
	}, &crashStdout, &crashStderr); code != 0 {
		t.Fatalf("crash driver exit=%d stdout=%q stderr=%q daemon=%s", code, crashStdout.String(), crashStderr.String(), daemon.output.String())
	}
	if err := <-daemon.done; err == nil {
		t.Fatal("crashed daemon exited successfully")
	}
	daemon = nil
	reader, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := reader.Inspect(ctx, symphony.Status{})
	_ = reader.Close()
	if err != nil || len(committed.Works) != 2 || committed.Works[0].Status != symphony.WorkCompleted ||
		committed.Works[1].Role != symphony.RoleBaselineVerification || committed.Works[1].Status != symphony.WorkPending {
		t.Fatalf("terminal commit did not persist exactly one successor: view=%+v err=%v", committed, err)
	}
	verificationWorkID := committed.Works[1].ID

	daemon = startDaemonProcess(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, binDir)
	var verificationSession symphony.AgentSession
	var verificationBinding symphony.PaneBinding
	waitForBinding(t, ctx, stateRoot, verificationWorkID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		verificationSession, verificationBinding = session, binding
		return session.Status == symphony.AgentSessionRunning && session.ID != draftSession.ID && binding.TerminalID != ""
	})
	if verificationSession.Role != symphony.RoleBaselineVerification {
		t.Fatalf("recovered successor Session = %+v", verificationSession)
	}
	view, err = control.Status(ctx, pikaSocket)
	if err != nil || len(view.Works) != 2 || view.Works[1].ID != verificationWorkID {
		t.Fatalf("terminal recovery duplicated successor: view=%+v err=%v", view, err)
	}
	if _, err := control.CancelWork(ctx, pikaSocket, verificationWorkID, protocol.CancelWorkRequest{Mutation: protocol.Mutation{RequestID: "cancel-verification"}}); err != nil {
		t.Fatal(err)
	}
	waitForNoActiveSessions(t, ctx, stateRoot)
	if _, err := control.Shutdown(ctx, pikaSocket, protocol.ShutdownRequest{Mutation: protocol.Mutation{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-daemon.done:
		if err != nil {
			t.Fatalf("terminal recovery daemon drain exit: %v output=%s", err, daemon.output.String())
		}
		daemon = nil
	case <-time.After(10 * time.Second):
		t.Fatalf("terminal recovery daemon did not drain: pane=%s output=%s", verificationBinding.PaneID, daemon.output.String())
	}
}

func TestDaemonProcessCrashAfterBestGitCommitRecoversIntegrationOnce(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	pikaBinary := os.Getenv("PIKA_GO_BIN")
	if fakeAgent == "" || !filepath.IsAbs(fakeAgent) || pikaBinary == "" || !filepath.IsAbs(pikaBinary) {
		t.Fatal("PIKA_GO_FAKE_AGENT_BIN and PIKA_GO_BIN must be absolute paths")
	}
	root, err := os.MkdirTemp("/tmp", "pika-go-best-crash-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PIKA_GO_FAKE_AGENT_AUTORUN": "optimization",
		"PIKA_GO_FAKE_COUNTER_DIR":             filepath.Join(root, "fake-counter"),
		"PIKA_GO_FAKE_AFTER_BEST_UPDATE_DELAY": "3s",
		"PATH":                                 binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "best-crash-e2e", "focus": false}, &created); err != nil {
		t.Fatal(err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	databasePath := filepath.Join(stateRoot, "instances", "integration-instance", "pika.db")
	configureTestScheduler(t, ctx, repository, stateRoot, configRoot, pikaBinary, 1, 1)

	var daemon *daemonProcess
	t.Cleanup(func() {
		if daemon != nil {
			daemon.killAndWait()
		}
		_ = client.Call(context.Background(), "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
	})
	daemon = startDaemonProcess(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, binDir)
	var crashStdout, crashStderr bytes.Buffer
	driverDone := make(chan int, 1)
	go func() {
		driverDone <- testdriver.Run(ctx, []string{
			"kill-at", "--pid", strconv.Itoa(daemon.command.Process.Pid), "--database", databasePath,
			"--point", "git-intent-applied", "--timeout", "60s",
		}, &crashStdout, &crashStderr)
	}()
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{
		Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID,
	}); err != nil {
		t.Fatal(err)
	}
	promptInitialBaselineFixture(t, ctx, client)
	if code := <-driverDone; code != 0 {
		t.Fatalf("Git crash driver exit=%d stdout=%q stderr=%q daemon=%s Herdr=%s", code, crashStdout.String(), crashStderr.String(), daemon.output.String(), serverOutput.String())
	}
	if err := <-daemon.done; err == nil {
		t.Fatal("Git-crashed daemon exited successfully")
	}
	daemon = nil
	reader, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	beforeRestart, err := reader.Inspect(ctx, symphony.Status{})
	activeBeforeRestart, activeErr := reader.ActiveAgentSessions(ctx)
	_ = reader.Close()
	if err != nil || activeErr != nil || beforeRestart.Best == nil || beforeRestart.Best.Sequence != 0 {
		t.Fatalf("inspect Git crash state: view=%+v active=%+v err=%v activeErr=%v", beforeRestart, activeBeforeRestart, err, activeErr)
	}
	var prepared symphony.IntegrationView
	for _, integration := range beforeRestart.Integrations {
		if integration.Status == "best_update_prepared" && integration.IntentID != "" {
			prepared = integration
			break
		}
	}
	if prepared.ID == "" {
		t.Fatalf("pending Git Intent missing after crash: %+v", beforeRestart.Integrations)
	}
	var integrationWork symphony.WorkView
	for _, work := range beforeRestart.Works {
		if work.IntegrationID == prepared.ID {
			integrationWork = work
			break
		}
	}
	if integrationWork.ID == "" || integrationWork.Status != symphony.WorkPending {
		t.Fatalf("prepared Integration Work missing after crash: %+v", beforeRestart.Works)
	}
	oldSessionID := ""
	for _, active := range activeBeforeRestart {
		if active.Session.WorkID == integrationWork.ID {
			oldSessionID = active.Session.ID
			break
		}
	}
	appliedGitSHA := strings.TrimSpace(runGitCommand(t, repository, "rev-parse", "refs/heads/pika/best"))
	if appliedGitSHA == beforeRestart.Best.CommitSHA {
		t.Fatalf("Git crash point did not advance pika/best beyond %s", appliedGitSHA)
	}
	if trailer := strings.TrimSpace(runGitCommand(t, repository, "show", "-s", "--format=%(trailers:key=Pika-Intent,valueonly)", appliedGitSHA)); trailer != prepared.IntentID {
		t.Fatalf("applied Best trailer=%q, want intent %q", trailer, prepared.IntentID)
	}

	daemon = startDaemonProcess(t, pikaBinary, pikaSocket, stateRoot, configRoot, herdrSocket, created.RootPane.PaneID, binDir)
	var recoveredSession symphony.AgentSession
	var recoveredBinding symphony.PaneBinding
	waitForBinding(t, ctx, stateRoot, integrationWork.ID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		recoveredSession, recoveredBinding = session, binding
		return session.Status == symphony.AgentSessionRunning && session.ID != oldSessionID && binding.TerminalID != ""
	})
	if recoveredSession.Role != symphony.RoleIntegration {
		t.Fatalf("recovered Session is not Integration: %+v", recoveredSession)
	}
	deadline := time.Now().Add(20 * time.Second)
	var recovered symphony.View
	for time.Now().Before(deadline) {
		recovered, err = control.Status(ctx, pikaSocket)
		if err == nil && recovered.Best != nil && recovered.Best.Sequence == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || recovered.Best == nil || recovered.Best.Sequence != 1 || recovered.Best.CommitSHA != appliedGitSHA {
		var paneRead json.RawMessage
		paneErr := client.Call(context.Background(), "pane.read", map[string]any{"pane_id": recoveredBinding.PaneID, "source": "recent", "lines": 120, "format": "text"}, &paneRead)
		snapshot, snapshotErr := client.Snapshot(context.Background())
		t.Fatalf("recovered Integration did not apply one Best revision: view=%+v err=%v pane=%s paneErr=%v snapshot=%+v snapshotErr=%v daemon=%s Herdr=%s", recovered, err, paneRead, paneErr, snapshot.Agents, snapshotErr, daemon.output.String(), serverOutput.String())
	}
	accepted := 0
	for _, integration := range recovered.Integrations {
		if integration.ID == prepared.ID && integration.Status == "accepted" && integration.IntentID == prepared.IntentID {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("prepared Integration was not accepted exactly once: %+v", recovered.Integrations)
	}
}

func TestBaselineAcceptedEndToEndThroughMCP(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatalf("find herdr: %v", err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	if fakeAgent == "" || !filepath.IsAbs(fakeAgent) {
		t.Fatal("PIKA_GO_FAKE_AGENT_BIN must be an absolute path")
	}
	root, err := os.MkdirTemp("/tmp", "pika-go-baseline-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PIKA_GO_FAKE_AGENT_AUTORUN": "baseline-accepted",
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatalf("start Herdr: %v", err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "baseline-e2e", "focus": false}, &created); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatalf("split init pane: %v", err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_PANE_ID", created.RootPane.PaneID)
	t.Setenv("PIKA_GO_AGENT_PATH_PREFIX", binDir)
	stopDaemon := startTestDaemon(t, pikaSocket, stateRoot, configRoot)
	defer stopDaemon()
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository)}); err != nil {
		t.Fatalf("init: %v", err)
	}
	promptInitialBaselineFixture(t, ctx, client)
	deadline := time.Now().Add(15 * time.Second)
	var view symphony.View
	for time.Now().Before(deadline) {
		view, err = control.Status(ctx, pikaSocket)
		if err == nil && view.Optimization.Status == symphony.OptimizationOptimizing {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || view.Optimization.Status != symphony.OptimizationOptimizing || view.Baseline == nil || view.Baseline.Status != symphony.BaselineAccepted || len(view.Works) != 6 || view.Works[0].Status != symphony.WorkCompleted || view.Works[1].Status != symphony.WorkCompleted {
		snapshot, _ := client.Snapshot(ctx)
		var reads []json.RawMessage
		for _, pane := range snapshot.Panes {
			var read json.RawMessage
			_ = client.Call(ctx, "pane.read", map[string]any{"pane_id": pane.PaneID, "source": "recent", "lines": 80, "format": "text"}, &read)
			reads = append(reads, read)
		}
		t.Fatalf("baseline E2E view=%+v err=%v; panes=%+v reads=%s; Herdr=%s", view, err, snapshot.Panes, reads, serverOutput.String())
	}
	iterationTabs := map[string]string{}
	for _, work := range view.Works {
		if work.Role == symphony.RoleIteration && work.Status == symphony.WorkPending {
			work := work
			waitForBinding(t, ctx, stateRoot, work.ID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
				if session.Role != symphony.RoleIteration || binding.TerminalID == "" {
					return false
				}
				iterationTabs[work.ID] = binding.TabID
				return true
			})
		}
	}
	if len(iterationTabs) != 4 {
		t.Fatalf("Iteration tab bindings = %d, want 4", len(iterationTabs))
	}
	seenTabs := map[string]string{}
	for workID, tabID := range iterationTabs {
		if tabID == "" || tabID == created.RootPane.TabID {
			t.Fatalf("Iteration Work %s was placed in the control tab %q", workID, tabID)
		}
		if otherWorkID, found := seenTabs[tabID]; found {
			t.Fatalf("Iteration Works %s and %s share tab %s", otherWorkID, workID, tabID)
		}
		seenTabs[tabID] = workID
	}
	leftovers, _ := filepath.Glob(filepath.Join(stateRoot, "instances", "integration-instance", "runtime", ".agent-env-*"))
	if len(leftovers) != 0 {
		t.Fatalf("agent environment files remain: %+v", leftovers)
	}
	_ = client.Call(ctx, "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
}

func TestFollowUpThroughRealHerdr(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	root, err := os.MkdirTemp("/tmp", "pika-go-follow-up-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PIKA_GO_FAKE_AGENT_AUTORUN": "baseline-follow-up",
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "follow-up-e2e", "focus": false}, &created); err != nil {
		t.Fatal(err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_PANE_ID", created.RootPane.PaneID)
	t.Setenv("PIKA_GO_AGENT_PATH_PREFIX", binDir)
	t.Setenv("PIKA_GO_FOLLOWUP_INACTIVITY", "500ms")
	stopDaemon := startTestDaemon(t, pikaSocket, stateRoot, configRoot)
	defer stopDaemon()
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository)}); err != nil {
		t.Fatal(err)
	}
	promptInitialBaselineFixture(t, ctx, client)
	var target symphony.WorkView
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		view, statusErr := control.Status(ctx, pikaSocket)
		if statusErr == nil {
			for _, work := range view.Works {
				if work.Role == symphony.RoleBaselineVerification && work.Status == symphony.WorkPending {
					target = work
				}
			}
		}
		if target.ID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if target.ID == "" {
		t.Fatalf("verification target did not start; Herdr=%s", serverOutput.String())
	}
	databasePath := filepath.Join(stateRoot, "instances", "integration-instance", "pika.db")
	reader, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var targetSession symphony.AgentSession
	for time.Now().Before(deadline) {
		var found bool
		targetSession, _, found, err = reader.CurrentAgentSession(ctx, target.ID)
		if err == nil && found && targetSession.Status == symphony.AgentSessionRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = reader.Close()
	if targetSession.ID == "" {
		t.Fatal("target Agent Session did not bind")
	}
	waitForHerdrAgentStatus(t, ctx, client, targetSession.AgentName, "idle")
	hook := json.RawMessage(`{"session_id":"fake-provider-session","turn_id":"fake-stopped-turn","hook_event_name":"Stop","last_assistant_message":"waiting for guidance"}`)
	if err := control.IngestProviderEvent(ctx, pikaSocket, "codex", protocol.ProviderEventRequest{AgentSessionID: targetSession.ID, Event: hook}); err != nil {
		t.Fatal(err)
	}
	var view symphony.View
	deadline = time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		view, err = control.Status(ctx, pikaSocket)
		if err == nil && len(view.FollowUps) != 0 && view.FollowUps[0].Status == "delivered" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || len(view.FollowUps) != 1 || view.FollowUps[0].Status != "delivered" || view.FollowUps[0].Message == "" {
		t.Fatalf("Follow-up E2E view=%+v err=%v Herdr=%s", view, err, serverOutput.String())
	}
	if targetAfter := func() symphony.WorkView {
		for _, work := range view.Works {
			if work.ID == target.ID {
				return work
			}
		}
		return symphony.WorkView{}
	}(); targetAfter.Status != symphony.WorkPending {
		t.Fatalf("Follow-up completed target Work: %+v", targetAfter)
	}
	_ = client.Call(ctx, "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
}

func TestRejectedBaselineCreatesFreshDraftSession(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatalf("find herdr: %v", err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	root, err := os.MkdirTemp("/tmp", "pika-go-rejected-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PIKA_GO_FAKE_AGENT_AUTORUN": "baseline-rejected",
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatalf("start Herdr: %v", err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "baseline-rejected", "focus": false}, &created); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatalf("split init pane: %v", err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_PANE_ID", created.RootPane.PaneID)
	t.Setenv("PIKA_GO_AGENT_PATH_PREFIX", binDir)
	stopDaemon := startTestDaemon(t, pikaSocket, stateRoot, configRoot)
	defer stopDaemon()
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository)}); err != nil {
		t.Fatalf("init: %v", err)
	}
	promptInitialBaselineFixture(t, ctx, client)
	deadline := time.Now().Add(15 * time.Second)
	var view symphony.View
	for time.Now().Before(deadline) {
		view, err = control.Status(ctx, pikaSocket)
		if err == nil && view.Optimization.Status == symphony.OptimizationDraftingBaseline && view.Optimization.Revision == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || view.Optimization.Revision != 3 || len(view.Baselines) != 2 || view.Baselines[0].Status != symphony.BaselineRejected || view.Baselines[1].Status != symphony.BaselineDrafting || len(view.Works) != 3 || view.Works[2].Role != symphony.RoleBaselineDraft || view.Works[2].Status != symphony.WorkPending {
		t.Fatalf("rejected baseline E2E view=%+v err=%v; Herdr=%s", view, err, serverOutput.String())
	}
	waitForBinding(t, ctx, stateRoot, view.Works[2].ID, func(session symphony.AgentSession, binding symphony.PaneBinding) bool {
		return session.Role == symphony.RoleBaselineDraft && session.Generation == 1 && binding.TerminalID != ""
	})
	_ = client.Call(ctx, "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
}

func TestOptimizationFIFOThroughRealHerdr(t *testing.T) {
	if os.Getenv("PIKA_GO_HERDR_INTEGRATION") != "1" {
		t.Skip("set PIKA_GO_HERDR_INTEGRATION=1 to run the real Herdr test")
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatalf("find herdr: %v", err)
	}
	fakeAgent := os.Getenv("PIKA_GO_FAKE_AGENT_BIN")
	root, err := os.MkdirTemp("/tmp", "pika-go-optimization-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	server.Env = replacedEnvironment(map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PIKA_GO_FAKE_AGENT_AUTORUN": "optimization",
		"PIKA_GO_FAKE_COUNTER_DIR": filepath.Join(root, "fake-counter"),
		"PATH":                     binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatalf("start Herdr: %v", err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "optimization-e2e", "focus": false}, &created); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatalf("split init pane: %v", err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	pikaSocket := filepath.Join(root, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "pika-state"), filepath.Join(root, "pika-config")
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_PANE_ID", created.RootPane.PaneID)
	t.Setenv("PIKA_GO_AGENT_PATH_PREFIX", binDir)
	stopDaemon := startTestDaemon(t, pikaSocket, stateRoot, configRoot)
	defer stopDaemon()
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository)}); err != nil {
		t.Fatalf("init: %v", err)
	}
	promptInitialBaselineFixture(t, ctx, client)
	deadline := time.Now().Add(100 * time.Second)
	var view symphony.View
	for time.Now().Before(deadline) {
		view, err = control.Status(ctx, pikaSocket)
		if err == nil && view.Best != nil && view.Best.Sequence >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || view.Best == nil || view.Best.Sequence < 3 {
		snapshot, _ := client.Snapshot(ctx)
		var reads []json.RawMessage
		for _, pane := range snapshot.Panes {
			var read json.RawMessage
			_ = client.Call(ctx, "pane.read", map[string]any{"pane_id": pane.PaneID, "source": "recent", "lines": 100, "format": "text"}, &read)
			reads = append(reads, read)
		}
		t.Fatalf("optimization did not reach three Best revisions: view=%+v err=%v panes=%+v reads=%s Herdr=%s", view, err, snapshot.Panes, reads, serverOutput.String())
	}
	var initialAttemptOrder []string
	for _, work := range view.Works {
		if work.Role == symphony.RoleIteration && work.IterationRound == 1 {
			initialAttemptOrder = append(initialAttemptOrder, work.AttemptID)
			if len(initialAttemptOrder) == 3 {
				break
			}
		}
	}
	var finishOrder []string
	accepted := 0
	for _, integration := range view.Integrations {
		if integration.IterationRound == 1 && integration.FIFOPosition <= 3 {
			finishOrder = append(finishOrder, integration.AttemptID)
		}
		if integration.Status == "accepted" {
			accepted++
		}
	}
	if len(initialAttemptOrder) != 3 || len(finishOrder) != 3 || slices.Equal(initialAttemptOrder, finishOrder) {
		t.Fatalf("initial Attempts did not finish out of order: initial=%v finish=%v", initialAttemptOrder, finishOrder)
	}
	if accepted < 3 {
		t.Fatalf("accepted Integrations = %d, want at least 3: %+v", accepted, view.Integrations)
	}
	if got := strings.TrimSpace(runGitCommand(t, repository, "rev-parse", "refs/heads/pika/best")); got != view.Best.CommitSHA {
		t.Fatalf("Git Best = %s, database Best = %s", got, view.Best.CommitSHA)
	}
	_ = client.Call(ctx, "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
}

func configureFakeHerdr(t *testing.T, root, fakeAgent string) (string, string, string) {
	t.Helper()
	configHome := filepath.Join(root, "herdr-config-home")
	configDir := filepath.Join(configHome, "herdr")
	stateHome := filepath.Join(root, "herdr-state")
	if err := os.MkdirAll(filepath.Join(configDir, "agent-detection"), 0o700); err != nil {
		t.Fatalf("create Herdr config: %v", err)
	}
	configPath := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[update]\nversion_check = false\nmanifest_check = false\n\n[session]\nresume_agents_on_restore = false\n"), 0o600); err != nil {
		t.Fatalf("write Herdr config: %v", err)
	}
	t.Setenv("HERDR_CONFIG_PATH", configPath)
	_, sourceFile, _, _ := runtime.Caller(0)
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "..", "..", "testdata", "herdr", "codex.toml"))
	if err != nil {
		t.Fatalf("read fake manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "agent-detection", "codex.toml"), manifest, 0o600); err != nil {
		t.Fatalf("write fake manifest: %v", err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatalf("create bin dir: %v", err)
	}
	wrapper := "#!/bin/sh\nexec env HERDR_AGENT=codex PIKA_GO_FAKE_AGENT_OSC=1 \"" + fakeAgent + "\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write codex wrapper: %v", err)
	}
	if pikaBinary := os.Getenv("PIKA_GO_BIN"); pikaBinary != "" {
		if err := os.Symlink(pikaBinary, filepath.Join(binDir, "pika-go")); err != nil {
			t.Fatalf("link pika-go test binary: %v", err)
		}
	}
	return configDir, stateHome, binDir
}

func runGitCommand(t *testing.T, repository string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", repository}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func initializeFixtureRepository(t *testing.T, repository string) {
	t.Helper()
	commands := [][]string{
		{"init", "--quiet", "--initial-branch=main", repository},
		{"-C", repository, "config", "user.name", "Pika Test"},
		{"-C", repository, "config", "user.email", "pika@example.invalid"},
	}
	for _, args := range commands {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "kernel.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatalf("write baseline fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("validation baseline\n"), 0o600); err != nil {
		t.Fatalf("write validation fixture: %v", err)
	}
	for _, args := range [][]string{{"-C", repository, "add", "kernel.txt", "target.txt"}, {"-C", repository, "commit", "-m", "baseline"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
}

func fixtureConfiguration(repository string) *string {
	agents := configuration.DefaultAgents()
	iteration := agents["iteration"]
	contents, err := configuration.RenderConfiguration(repository, agents, iteration, iteration, iteration, iteration)
	if err != nil {
		panic(err)
	}
	return &contents
}

func configureTestScheduler(t *testing.T, ctx context.Context, repository, stateRoot, configRoot, pikaBinary string, concurrency, maxPending int) {
	t.Helper()
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: stateRoot, InstanceID: "integration-instance",
		CodexHome: filepath.Join(configRoot, "codex-home"), PikaExecutable: pikaBinary, CodexExecutable: "/bin/sh",
	}
	if _, err := initializer.Prepare(ctx, repository); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configRoot, "instances", "integration-instance", "config.toml")
	agents := configuration.DefaultAgents()
	iterationAgents := make([]configuration.Agent, concurrency)
	for index := range iterationAgents {
		iterationAgents[index] = agents["iteration"]
	}
	configured, err := configuration.RenderConfiguration(repository, agents, iterationAgents...)
	if err != nil {
		t.Fatal(err)
	}
	configured = strings.Replace(configured, "max_pending_attempts = 8", "max_pending_attempts = "+strconv.Itoa(maxPending), 1)
	if err := os.WriteFile(configPath, []byte(configured), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startTestDaemon(t *testing.T, socketPath, stateRoot, configRoot string) func() {
	t.Helper()
	t.Setenv("CODEX_HOME", filepath.Join(configRoot, "codex-home"))
	t.Setenv("PIKA_GO_CODEX_EXECUTABLE", "/bin/echo")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stopOnce sync.Once
	go func() {
		done <- cli.Run(ctx, []string{"daemon", "--socket", socketPath, "--state-dir", stateRoot, "--config-dir", configRoot, "--instance", "integration-instance"}, nil, io.Discard, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := control.Health(context.Background(), socketPath); err == nil {
			return func() {
				stopOnce.Do(func() {
					cancel()
					select {
					case code := <-done:
						if code != 0 {
							t.Errorf("daemon exit code = %d", code)
						}
					case <-time.After(5 * time.Second):
						t.Errorf("daemon did not stop")
					}
				})
			}
		}
		select {
		case code := <-done:
			cancel()
			t.Fatalf("daemon exited before health check with code %d", code)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatal("daemon socket did not become healthy")
	return func() {}
}

type daemonProcess struct {
	command    *exec.Cmd
	done       chan error
	output     bytes.Buffer
	socketPath string
}

func startDaemonProcess(t *testing.T, pikaBinary, socketPath, stateRoot, configRoot, herdrSocket, symphonyPane, binDir string) *daemonProcess {
	t.Helper()
	process := &daemonProcess{done: make(chan error, 1), socketPath: socketPath}
	process.command = exec.Command(
		pikaBinary,
		"daemon",
		"--socket", socketPath,
		"--state-dir", stateRoot,
		"--config-dir", configRoot,
		"--instance", "integration-instance",
	)
	process.command.Env = replacedEnvironment(map[string]string{
		"HERDR_SOCKET_PATH":         herdrSocket,
		"HERDR_PANE_ID":             symphonyPane,
		"HERDR_ENV":                 "1",
		"PIKA_GO_AGENT_PATH_PREFIX": binDir,
		"CODEX_HOME":                filepath.Join(configRoot, "codex-home"),
		"PIKA_GO_CODEX_EXECUTABLE":  "/bin/echo",
		"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	process.command.Stdout = &process.output
	process.command.Stderr = &process.output
	if err := process.command.Start(); err != nil {
		t.Fatalf("start pika-go daemon process: %v", err)
	}
	go func() {
		process.done <- process.command.Wait()
	}()
	waitForPikaProcessHealth(t, socketPath, process)
	return process
}

func (process *daemonProcess) killAndWait() {
	if process == nil {
		return
	}
	if process.socketPath != "" {
		if health, err := control.Health(context.Background(), process.socketPath); err == nil && health.PID > 0 {
			if owner, findErr := os.FindProcess(health.PID); findErr == nil {
				_ = owner.Kill()
			}
		}
	} else if process.command.ProcessState == nil {
		_ = process.command.Process.Kill()
	}
	select {
	case <-process.done:
	case <-time.After(5 * time.Second):
	}
}

func waitForPikaProcessHealth(t *testing.T, socketPath string, process *daemonProcess) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := control.Health(context.Background(), socketPath); err == nil {
			return
		}
		select {
		case err := <-process.done:
			t.Fatalf("daemon process exited before health check: %v; output=%s", err, process.output.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = process.command.Process.Kill()
	t.Fatalf("daemon process did not become healthy: %s", process.output.String())
}

func waitForPikaHealth(t *testing.T, socketPath string, daemonDone <-chan int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := control.Health(context.Background(), socketPath); err == nil {
			return
		}
		select {
		case code := <-daemonDone:
			t.Fatalf("daemon exited before health check with code %d", code)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon socket did not become healthy")
}

func waitForNamedAgent(t *testing.T, ctx context.Context, client *herdr.Client, excludedName string) herdr.Agent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := client.Snapshot(ctx)
		if err == nil {
			for _, agent := range snapshot.Agents {
				if agent.Name != nil && strings.HasPrefix(*agent.Name, "pika-") && *agent.Name != excludedName && agent.InteractiveReady {
					return agent
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("managed agent did not appear")
	return herdr.Agent{}
}

func promptInitialBaselineFixture(t *testing.T, ctx context.Context, client *herdr.Client) herdr.Agent {
	t.Helper()
	agent := waitForNamedAgent(t, ctx, client, "")
	if _, err := herdr.NewRuntime(client).Prompt(ctx, agent.PaneID, "operator baseline configuration"); err != nil {
		t.Fatalf("prompt initial Baseline fixture: %v", err)
	}
	waitForPaneText(t, ctx, client, agent.PaneID, "FAKE_AGENT_PROMPT")
	return agent
}

func assertPaneLacksText(t *testing.T, ctx context.Context, client *herdr.Client, paneID, text string) {
	t.Helper()
	var result struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := client.Call(ctx, "pane.read", map[string]any{"pane_id": paneID, "source": "recent", "lines": 100, "format": "text"}, &result); err != nil {
		t.Fatalf("read pane %s: %v", paneID, err)
	}
	if strings.Contains(result.Read.Text, text) {
		t.Fatalf("pane %s unexpectedly contained %q: text=%q", paneID, text, result.Read.Text)
	}
}

func waitForHerdrAgentStatus(t *testing.T, ctx context.Context, client *herdr.Client, target, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last herdr.Agent
	var lastErr error
	runtimeAdapter := herdr.NewRuntime(client)
	for time.Now().Before(deadline) {
		last, lastErr = runtimeAdapter.GetAgent(ctx, target)
		if lastErr == nil && last.AgentStatus == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	var paneResult struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	_ = client.Call(ctx, "pane.read", map[string]any{"pane_id": last.PaneID, "source": "recent", "lines": 100, "format": "text"}, &paneResult)
	t.Fatalf("Herdr Agent %s status=%s want=%s err=%v pane=%q", target, last.AgentStatus, want, lastErr, paneResult.Read.Text)
}

func readCurrentSession(stateRoot, workID string) (symphony.AgentSession, symphony.PaneBinding, bool, error) {
	engine, err := symphony.Open(context.Background(), filepath.Join(stateRoot, "instances", "integration-instance", "pika.db"), symphony.Options{})
	if err != nil {
		return symphony.AgentSession{}, symphony.PaneBinding{}, false, err
	}
	defer engine.Close()
	return engine.CurrentAgentSession(context.Background(), workID)
}

func waitForPaneText(t *testing.T, ctx context.Context, client *herdr.Client, paneID, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		var result struct {
			Read struct {
				Text string `json:"text"`
			} `json:"read"`
		}
		lastErr = client.Call(ctx, "pane.read", map[string]any{"pane_id": paneID, "source": "recent", "lines": 100, "format": "text"}, &result)
		last = result.Read.Text
		if lastErr == nil && strings.Contains(last, text) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pane %s did not contain %q: err=%v text=%q", paneID, text, lastErr, last)
}

func waitForBinding(t *testing.T, ctx context.Context, stateRoot, workID string, predicate func(symphony.AgentSession, symphony.PaneBinding) bool) {
	t.Helper()
	path := filepath.Join(stateRoot, "instances", "integration-instance", "pika.db")
	deadline := time.Now().Add(10 * time.Second)
	var lastSession symphony.AgentSession
	var lastBinding symphony.PaneBinding
	var lastErr error
	for time.Now().Before(deadline) {
		engine, err := symphony.Open(ctx, path, symphony.Options{})
		lastErr = err
		if err == nil {
			session, binding, found, readErr := engine.CurrentAgentSession(ctx, workID)
			_ = engine.Close()
			lastSession, lastBinding, lastErr = session, binding, readErr
			if readErr == nil && found && predicate(session, binding) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("persisted pane binding did not reach expected state: session=%+v binding=%+v err=%v", lastSession, lastBinding, lastErr)
}

func waitForNoActiveSessions(t *testing.T, ctx context.Context, stateRoot string) {
	t.Helper()
	path := filepath.Join(stateRoot, "instances", "integration-instance", "pika.db")
	deadline := time.Now().Add(10 * time.Second)
	var last []symphony.ActiveAgentSession
	var lastErr error
	for time.Now().Before(deadline) {
		engine, err := symphony.Open(ctx, path, symphony.Options{})
		lastErr = err
		if err == nil {
			last, lastErr = engine.ActiveAgentSessions(ctx)
			_ = engine.Close()
			if lastErr == nil && len(last) == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("active sessions remain after terminal chain=%+v err=%v", last, lastErr)
}

func waitForUnixSocket(t *testing.T, path string, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("unix", path, 50*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Herdr socket did not appear: %s", output.String())
}

func replacedEnvironment(overrides map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "HERDR_") {
			continue
		}
		if _, replaced := overrides[name]; !replaced {
			result = append(result, entry)
		}
	}
	for name, value := range overrides {
		result = append(result, name+"="+value)
	}
	return result
}

func agentNameValue(a herdr.Agent) string {
	if a.Name == nil {
		return ""
	}
	return *a.Name
}
