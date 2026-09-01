package workruntime_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/cli"
	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
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
	t.Setenv("PIKA_GO_CODEX_EXECUTABLE", filepath.Join(binDir, "codex"))
	writeHerdrIntegrationCodexModelsCache(t, configRoot)
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

func TestMaintenancePrepareWaitsForAgentTerminalThenHoldsColdOpen(t *testing.T) {
	if os.Getenv("PIKA_GO_SCHEMA20_BRIDGE") != "" || os.Getenv("PIKA_GO_SCHEMA23_TARGET") != "" {
		t.Run("target resume crash retry", func(t *testing.T) {
			runMaintenancePrepareWaitsForAgentTerminalThenHoldsColdOpen(t, false)
		})
		t.Run("bridge rollback resume", func(t *testing.T) {
			runMaintenancePrepareWaitsForAgentTerminalThenHoldsColdOpen(t, true)
		})
		return
	}
	runMaintenancePrepareWaitsForAgentTerminalThenHoldsColdOpen(t, false)
}

func runMaintenancePrepareWaitsForAgentTerminalThenHoldsColdOpen(t *testing.T, bridgeRollbackResume bool) {
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
	root, err := os.MkdirTemp("/tmp", "pika-go-maintenance-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	agentFinishGate := filepath.Join(root, "finish-active-agent")
	resumeCheckpointRoot := filepath.Join(root, "resume-checkpoints")
	if err := os.Mkdir(resumeCheckpointRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	herdrConfig, herdrState, binDir := configureFakeHerdr(t, root, fakeAgent)
	serverCtx, stopServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, herdrBinary, "server")
	serverEnvironment := map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(herdrConfig), "XDG_STATE_HOME": herdrState,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent, "PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PIKA_GO_FAKE_AGENT_AUTORUN": "baseline-accepted", "PIKA_GO_FAKE_AGENT_AUTORUN_GATE": agentFinishGate,
		"CODEX_HOME": filepath.Join(root, "codex-home"), "PIKA_GO_CODEX_EXECUTABLE": filepath.Join(binDir, "codex"),
		"PIKA_GO_INTEGRATION_RESUME_CHECKPOINT_DIR": resumeCheckpointRoot,
	}
	if os.Getenv("PIKA_GO_SCHEMA20_BRIDGE") != "" {
		serverEnvironment["PIKA_GO_FAKE_AGENT_SCHEMA20_BASELINE"] = "1"
	}
	server.Env = replacedEnvironment(serverEnvironment)
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(); _ = server.Wait() })
	herdrSocket := filepath.Join(herdrConfig, "herdr.sock")
	waitForUnixSocket(t, herdrSocket, &serverOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := herdr.NewClient(herdrSocket)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "maintenance-e2e", "focus": false}, &created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Call(context.Background(), "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
	})
	initPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	initializeFixtureRepository(t, repository)
	workspace, err := optimizationworkspace.Create(ctx, filepath.Join(root, "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	pikaSocket := filepath.Join(root, "pika.sock")
	daemonPane, err := herdr.NewRuntime(client).SplitPane(ctx, created.RootPane.PaneID, "right", workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteHerdrBinding(optimizationworkspace.HerdrBinding{
		SocketPath: herdrSocket, WorkspaceID: created.Workspace.WorkspaceID, TabID: created.RootPane.TabID,
		ControlPane: created.RootPane.PaneID, DaemonPane: daemonPane.PaneID, DaemonSocket: pikaSocket,
		UpdatedAt: "2026-09-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	writeHerdrIntegrationCodexModelsCache(t, root)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourcePath(t)), "..", ".."))
	oldBinary := os.Getenv("PIKA_GO_SCHEMA20_BRIDGE")
	targetBinary := os.Getenv("PIKA_GO_SCHEMA23_TARGET")
	crossSchema := oldBinary != "" || targetBinary != ""
	var oldProbe, targetProbe daemonupdate.Probe
	if (oldBinary == "") != (targetBinary == "") {
		t.Fatal("PIKA_GO_SCHEMA20_BRIDGE and PIKA_GO_SCHEMA23_TARGET must be set together")
	}
	if crossSchema {
		oldBinary, targetBinary = filepath.Clean(oldBinary), filepath.Clean(targetBinary)
		oldProbe = binaryProbe(t, oldBinary)
		targetProbe = binaryProbe(t, targetBinary)
		if oldProbe.SQLiteSchema != 20 || oldProbe.ControlProtocol != 2 || oldProbe.HandoffProtocol != 1 || oldProbe.WorkspaceFormat != 1 {
			t.Fatalf("schema20 bridge probe = %+v", oldProbe)
		}
		if targetProbe.SQLiteSchema != 23 || targetProbe.ControlProtocol != 2 || targetProbe.HandoffProtocol != 1 || targetProbe.WorkspaceFormat != 1 {
			t.Fatalf("schema23 target probe = %+v", targetProbe)
		}
	} else {
		oldBinary = filepath.Join(root, "pika-go-old")
		targetBinary = filepath.Join(root, "pika-go-target")
		for _, build := range []struct {
			path    string
			version string
		}{{oldBinary, "maintenance-old"}, {targetBinary, "maintenance-target"}} {
			command := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version="+build.version, "-o", build.path, "./cmd/pika-go")
			command.Dir = repositoryRoot
			if output, buildErr := command.CombinedOutput(); buildErr != nil {
				t.Fatalf("build %s: %v: %s", build.version, buildErr, output)
			}
		}
	}
	oldDigest, err := daemonupdate.FileDigest(oldBinary)
	if err != nil {
		t.Fatal(err)
	}
	targetDigest, err := daemonupdate.FileDigest(targetBinary)
	if err != nil {
		t.Fatal(err)
	}
	if oldDigest == targetDigest {
		t.Fatal("test daemon generations have identical digests")
	}
	if crossSchema {
		t.Logf("release bridge=%s digest=%s probe=%+v", oldBinary, oldDigest, oldProbe)
		t.Logf("release target=%s digest=%s probe=%+v", targetBinary, targetDigest, targetProbe)
	}
	startWorkspaceDaemon := func(binary string) *daemonProcess {
		process := &daemonProcess{done: make(chan error, 1), socketPath: pikaSocket}
		arguments := []string{"daemon", "--socket", pikaSocket, "--workspace", workspace.Root}
		process.command = exec.Command(binary, arguments...)
		process.command.Env = replacedEnvironment(map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket, "HERDR_PANE_ID": created.RootPane.PaneID,
			"PIKA_GO_AGENT_PATH_PREFIX": binDir, "CODEX_HOME": filepath.Join(root, "codex-home"),
			"PIKA_GO_CODEX_EXECUTABLE":       filepath.Join(binDir, "codex"),
			"PIKA_GO_INTEGRATION_EXECUTABLE": binary,
			"PATH":                           binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		})
		process.command.Stdout, process.command.Stderr = &process.output, &process.output
		if startErr := process.command.Start(); startErr != nil {
			t.Fatalf("start %s: %v", binary, startErr)
		}
		go func() { process.done <- process.command.Wait() }()
		waitForPikaProcessHealth(t, pikaSocket, process)
		return process
	}
	startWorkspaceDaemonViaOpen := func(binary string, checkpoint ...string) *daemonProcess {
		if len(checkpoint) > 0 {
			for _, suffix := range []string{".enable", ".reached", ".continue"} {
				_ = os.Remove(filepath.Join(resumeCheckpointRoot, checkpoint[0]+suffix))
			}
			if err := os.WriteFile(filepath.Join(resumeCheckpointRoot, checkpoint[0]+".enable"), []byte(checkpoint[0]+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		openCommand := exec.Command(binary, "open", workspace.Root, "--no-focus")
		openCommand.Env = replacedEnvironment(map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket,
			"HERDR_PANE_ID":     created.RootPane.PaneID,
			"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		})
		output, openErr := openCommand.CombinedOutput()
		if openErr != nil {
			t.Fatalf("maintenance-aware open with %s: %v\n%s", binary, openErr, output)
		}
		health, healthErr := control.Health(ctx, pikaSocket)
		if healthErr != nil || health.PID < 1 {
			t.Fatalf("opened daemon health=%+v err=%v output=%s", health, healthErr, output)
		}
		process := &daemonProcess{
			done: make(chan error, 1), socketPath: pikaSocket, pid: health.PID, external: true,
		}
		if len(checkpoint) > 0 {
			process.checkpointDirectory = resumeCheckpointRoot
			process.checkpointPoint = checkpoint[0]
		}
		process.output.Write(output)
		go func() {
			for processExists(health.PID) {
				time.Sleep(20 * time.Millisecond)
			}
			process.done <- nil
		}()
		return process
	}
	assertOpenRejectsExtraAgent := func(checkpoint string) {
		runtime := herdr.NewRuntime(client)
		extraPane, splitErr := runtime.SplitPane(ctx, created.RootPane.PaneID, "down", workspace.Root)
		if splitErr != nil {
			t.Fatal(splitErr)
		}
		t.Cleanup(func() { _ = runtime.ClosePane(context.Background(), extraPane.PaneID) })
		extraName := "pika-unexpected-" + strings.ReplaceAll(checkpoint, "_", "-")
		if _, startErr := runtime.Start(ctx, herdr.StartSpec{
			Name: extraName, Kind: "codex", PaneID: extraPane.PaneID,
			Arguments: []string{"--scenario", "idle"}, ReturnOnLaunch: true,
		}); startErr != nil {
			t.Fatal(startErr)
		}
		openCommand := exec.Command(targetBinary, "open", workspace.Root, "--no-focus")
		openCommand.Env = replacedEnvironment(map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket,
			"HERDR_PANE_ID":     created.RootPane.PaneID,
			"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		})
		output, openErr := openCommand.CombinedOutput()
		if openErr == nil {
			if health, healthErr := control.Health(ctx, pikaSocket); healthErr == nil && health.PID > 0 {
				_ = syscall.Kill(health.PID, syscall.SIGTERM)
			}
			t.Fatalf("maintenance open accepted extra Pika Agent at %s checkpoint: %s", checkpoint, output)
		}
		if !strings.Contains(string(output), "live Pika Agent count") {
			t.Fatalf("maintenance open rejected extra Agent for wrong reason at %s: %v\n%s", checkpoint, openErr, output)
		}
		if closeErr := runtime.ClosePane(ctx, extraPane.PaneID); closeErr != nil {
			t.Fatal(closeErr)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			present := false
			for _, observed := range mustHerdrSnapshot(t, ctx, client).Agents {
				if observed.Name != nil && *observed.Name == extraName {
					present = true
				}
			}
			if !present {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("extra Pika Agent %s remained after cleanup", extraName)
	}
	var daemonProcess *daemonProcess
	t.Cleanup(func() {
		if daemonProcess != nil {
			daemonProcess.killAndWait()
		}
	})
	daemonProcess = startWorkspaceDaemon(oldBinary)
	assertProcessExecutableDigest(t, daemonProcess.processID(), oldDigest)
	if _, err := control.Init(ctx, pikaSocket, protocol.InitRequest{
		Mutation: protocol.Mutation{RequestID: "init"}, Repository: repository, CallerPaneID: initPane.PaneID, ConfigurationTOML: fixtureConfiguration(repository),
	}); err != nil {
		t.Fatal(err)
	}
	agent := waitForNamedAgent(t, ctx, client, "")
	excludeCommand := exec.Command("git", "-C", workspace.BaseRepository, "rev-parse", "--git-path", "info/exclude")
	excludeOutput, err := excludeCommand.Output()
	if err != nil {
		t.Fatal(err)
	}
	excludePath := strings.TrimSpace(string(excludeOutput))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(workspace.BaseRepository, excludePath)
	}
	exclude, err := os.OpenFile(excludePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exclude.WriteString("\n.agents/\n"); err != nil {
		_ = exclude.Close()
		t.Fatal(err)
	}
	if err := exclude.Close(); err != nil {
		t.Fatal(err)
	}
	prepared, err := control.MaintenancePrepare(ctx, pikaSocket, protocol.MaintenancePrepareRequest{
		RequestID: "prepare-1", ToGeneration: protocol.MaintenanceGeneration{Digest: targetDigest, Version: "maintenance-target"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State != maintenance.StateQuiescing || len(prepared.Targets) != 1 ||
		prepared.Targets[0].SessionID == "" || prepared.Targets[0].AgentName == "" ||
		prepared.Targets[0].AgentKind == "" || prepared.Targets[0].WorkspaceID == "" ||
		prepared.Targets[0].TabID == "" || prepared.Targets[0].PaneID == "" ||
		prepared.Targets[0].TerminalID == "" {
		t.Fatalf("prepare did not freeze live identities: %+v", prepared)
	}
	select {
	case exitErr := <-daemonProcess.done:
		t.Fatalf("daemon exited before frozen Work was terminal: %v", exitErr)
	case <-time.After(300 * time.Millisecond):
	}
	if pikaAgentCount(t, ctx, client) != 1 {
		t.Fatalf("prepare started a second Agent")
	}
	halfOpen, err := net.Dial("unix", pikaSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer halfOpen.Close()
	if _, err := halfOpen.Write([]byte(
		"POST /v1/maintenance/prepare HTTP/1.1\r\n" +
			"Host: pika-go\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{",
	)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	shutdownStarted := time.Now()
	if err := os.WriteFile(agentFinishGate, []byte("finish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case exitErr := <-daemonProcess.done:
		if exitErr != nil {
			t.Fatalf("maintenance-ready daemon exit: %v output=%s", exitErr, daemonProcess.output.String())
		}
	case <-time.After(10 * time.Second):
		state, _ := control.MaintenanceStatus(ctx, pikaSocket)
		view, _ := control.Status(ctx, pikaSocket)
		var pane json.RawMessage
		_ = client.Call(ctx, "pane.read", map[string]any{"pane_id": agent.PaneID, "source": "recent", "lines": 100, "format": "text"}, &pane)
		snapshot, _ := client.Snapshot(ctx)
		t.Fatalf("daemon did not exit after terminal MCP: frozen=%+v maintenance=%+v works=%+v agents=%s pane=%s daemon=%s",
			prepared.Targets, state, view.Works, pikaAgentIdentities(snapshot), pane, daemonProcess.output.String())
	}
	if elapsed := time.Since(shutdownStarted); elapsed > 4*time.Second {
		t.Fatalf("external maintenance-ready shutdown exceeded bound: %s", elapsed)
	}
	if err := halfOpen.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var closed [1]byte
	if _, err := halfOpen.Read(closed[:]); err == nil {
		t.Fatal("half-open external request remained connected after maintenance-ready shutdown")
	}
	daemonProcess = nil
	if _, err := os.Lstat(pikaSocket); !os.IsNotExist(err) {
		t.Fatalf("socket remained after ready: %v", err)
	}
	ready, err := maintenance.NewFileStore(workspace.Root).Read()
	if err != nil || ready.State != maintenance.StateReady {
		t.Fatalf("ready state = %+v err=%v", ready, err)
	}
	var terminalPane struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := client.Call(ctx, "pane.read", map[string]any{"pane_id": agent.PaneID, "source": "recent", "lines": 100, "format": "text"}, &terminalPane); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(terminalPane.Read.Text, "FAKE_AGENT_MCP_RESPONSE "); count != 1 ||
		strings.Contains(terminalPane.Read.Text, "FAKE_AGENT_MCP_ERROR") ||
		strings.Contains(terminalPane.Read.Text, `"error"`) {
		t.Fatalf("terminal MCP did not receive exactly one successful response: count=%d pane=%q", count, terminalPane.Read.Text)
	}
	committed := inspectMaintenanceCommitRaw(t, workspace.DatabasePath, prepared.Targets[0])
	if crossSchema && committed.schema != 20 {
		t.Fatalf("bridge changed schema before cold open: %d", committed.schema)
	}
	if crossSchema {
		t.Logf("bridge ready schema=%d frozen=%+v", committed.schema, prepared.Targets[0])
	}
	var holdingBefore durableHoldingSnapshot
	var herdrBefore herdr.Snapshot
	var bindingBefore []byte
	if crossSchema {
		holdingBefore = captureDurableHoldingSnapshot(t, workspace.DatabasePath)
		herdrBefore = mustHerdrSnapshot(t, ctx, client)
		bindingBefore, err = os.ReadFile(workspace.HerdrBindingPath)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(agentFinishGate); err != nil {
		t.Fatal(err)
	}

	var schema20Backup map[string][]byte
	if crossSchema {
		schema20Backup = make(map[string][]byte)
		for _, suffix := range []string{"", "-wal", "-shm"} {
			contents, readErr := os.ReadFile(workspace.DatabasePath + suffix)
			if readErr == nil {
				schema20Backup[suffix] = contents
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
		}
		assertMaintenanceDirectDaemonRejected(t, targetBinary, workspace, pikaSocket, herdrSocket, daemonPane.PaneID)
	}

	daemonProcess = startWorkspaceDaemonViaOpen(targetBinary)
	assertProcessExecutableDigest(t, daemonProcess.processID(), targetDigest)
	holding, err := control.MaintenanceStatus(ctx, pikaSocket)
	if err != nil || holding.State != maintenance.StateHolding {
		t.Fatalf("cold holding = %+v err=%v", holding, err)
	}
	if crossSchema && sqliteSchemaVersion(t, workspace.DatabasePath) != 23 {
		t.Fatal("schema23 target did not migrate schema20 database")
	}
	if crossSchema {
		assertSchema23HoldingProjection(t, workspace.DatabasePath)
		t.Logf("target holding schema=%d holder=%s", sqliteSchemaVersion(t, workspace.DatabasePath), holding.HoldingGeneration.Digest)
	}
	holdingMutationSnapshot := captureDurableHoldingSnapshot(t, workspace.DatabasePath)
	gitHeadBefore := runGitCommand(t, workspace.BaseRepository, "rev-parse", "HEAD")
	gitStatusBefore := runGitCommand(t, workspace.BaseRepository, "status", "--porcelain=v2", "--untracked-files=all")
	evidenceBefore := snapshotHoldingTree(t, workspace.EvidenceRoot)
	for _, mutation := range []struct {
		path string
		body string
	}{
		{"/v1/baseline-drafts", `{"request_id":"holding-baseline"}`},
		{"/v1/works/" + prepared.Targets[0].WorkID + "/cancel", `{"request_id":"holding-cancel"}`},
		{"/v1/git-intents/missing/apply", `{"message":"holding"}`},
		{"/v1/provider-events/codex", fmt.Sprintf(`{"agent_session_id":%q,"event":{}}`, prepared.Targets[0].SessionID)},
		{"/v1/scheduler/pause", `{"request_id":"holding-pause"}`},
		{"/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`},
	} {
		assertHoldingMutationRejected(t, pikaSocket, mutation.path, mutation.body)
	}
	assertDurableHoldingSnapshotUnchanged(t, workspace.DatabasePath, holdingMutationSnapshot)
	if got := runGitCommand(t, workspace.BaseRepository, "rev-parse", "HEAD"); got != gitHeadBefore {
		t.Fatalf("Holding mutation changed Git HEAD: before=%s after=%s", gitHeadBefore, got)
	}
	if got := runGitCommand(t, workspace.BaseRepository, "status", "--porcelain=v2", "--untracked-files=all"); got != gitStatusBefore {
		t.Fatalf("Holding mutation changed Git state: before=%q after=%q", gitStatusBefore, got)
	}
	if after := snapshotHoldingTree(t, workspace.EvidenceRoot); !reflect.DeepEqual(after, evidenceBefore) {
		t.Fatalf("Holding mutation changed evidence:\nbefore=%v\nafter=%v", evidenceBefore, after)
	}
	time.Sleep(400 * time.Millisecond)
	if pikaAgentCount(t, ctx, client) != 1 {
		t.Fatalf("holding daemon launched another Agent")
	}
	if crossSchema {
		assertDurableHoldingSnapshotUnchanged(t, workspace.DatabasePath, holdingBefore)
		assertMaintenanceOpenHerdrSnapshot(t, herdrBefore, mustHerdrSnapshot(t, ctx, client),
			created.Workspace.WorkspaceID, created.RootPane.TabID, created.RootPane.PaneID, daemonPane.PaneID)
		if bindingAfter, readErr := os.ReadFile(workspace.HerdrBindingPath); readErr != nil || !bytes.Equal(bindingAfter, bindingBefore) {
			t.Fatalf("maintenance open changed binding bytes: err=%v\nbefore=%s\nafter=%s", readErr, bindingBefore, bindingAfter)
		}
	}
	if crossSchema {
		stopDaemonWithSignal(t, daemonProcess, syscall.SIGTERM)
		daemonProcess = nil
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if removeErr := os.Remove(workspace.DatabasePath + suffix); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				t.Fatal(removeErr)
			}
		}
		for suffix, contents := range schema20Backup {
			if err := os.WriteFile(workspace.DatabasePath+suffix, contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if schema := sqliteSchemaVersion(t, workspace.DatabasePath); schema != 20 {
			t.Fatalf("restored rollback schema = %d", schema)
		}
		daemonProcess = startWorkspaceDaemonViaOpen(oldBinary)
		assertProcessExecutableDigest(t, daemonProcess.processID(), oldDigest)
		rollbackHolding, statusErr := control.MaintenanceStatus(ctx, pikaSocket)
		if statusErr != nil || rollbackHolding.State != maintenance.StateHolding ||
			rollbackHolding.HoldingGeneration.Digest != oldDigest {
			t.Fatalf("bridge rollback holding = %+v err=%v", rollbackHolding, statusErr)
		}
		if schema := sqliteSchemaVersion(t, workspace.DatabasePath); schema != 20 {
			t.Fatalf("bridge rollback schema = %d", schema)
		}
		t.Logf("bridge rollback holding schema=20 holder=%s", rollbackHolding.HoldingGeneration.Digest)
		if bridgeRollbackResume {
			if _, resumeErr := control.MaintenanceResume(ctx, pikaSocket, protocol.MaintenanceResumeRequest{RequestID: "bridge-rollback-resume-1"}); resumeErr != nil {
				t.Fatal(resumeErr)
			}
			replacement := waitForNamedAgent(t, ctx, client, agentNameValue(agent))
			if replacement.TerminalID == agent.TerminalID || pikaAgentCount(t, ctx, client) != 1 {
				t.Fatalf("bridge rollback resume did not launch one successor: old=%+v replacement=%+v agents=%s",
					agent, replacement, pikaAgentIdentities(mustHerdrSnapshot(t, ctx, client)))
			}
			resumed, resumeErr := control.MaintenanceStatus(ctx, pikaSocket)
			if resumeErr != nil || resumed.State != maintenance.StateResumed || resumed.ResumeRequestID != "bridge-rollback-resume-1" {
				t.Fatalf("bridge rollback resume state=%+v err=%v", resumed, resumeErr)
			}
			if schema := sqliteSchemaVersion(t, workspace.DatabasePath); schema != 20 {
				t.Fatalf("bridge resume changed schema = %d", schema)
			}
			if err := os.WriteFile(agentFinishGate, []byte("finish\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			waitForPostResumeTerminalProgress(t, workspace.DatabasePath, prepared.Targets[0].WorkID)
			t.Logf("bridge resume terminal progress schema=20 request=%s successor_terminal=%s", resumed.ResumeRequestID, replacement.TerminalID)
			daemonProcess.killAndWait()
			daemonProcess = nil
			return
		}
		stopDaemonWithSignal(t, daemonProcess, syscall.SIGTERM)
		daemonProcess = startWorkspaceDaemonViaOpen(targetBinary)
		assertProcessExecutableDigest(t, daemonProcess.processID(), targetDigest)
		reclaimed, statusErr := control.MaintenanceStatus(ctx, pikaSocket)
		if statusErr != nil || reclaimed.State != maintenance.StateHolding ||
			reclaimed.HoldingGeneration.Digest != targetDigest {
			t.Fatalf("target reclaims rollback holding = %+v err=%v", reclaimed, statusErr)
		}
		if schema := sqliteSchemaVersion(t, workspace.DatabasePath); schema != 23 {
			t.Fatalf("target remigration schema = %d", schema)
		}
		t.Logf("target reclaimed holding schema=23 holder=%s", reclaimed.HoldingGeneration.Digest)
	}
	if crossSchema {
		stopDaemonWithSignal(t, daemonProcess, syscall.SIGTERM)
		daemonProcess = startWorkspaceDaemonViaOpen(targetBinary, "intent_persisted")
		assertProcessExecutableDigest(t, daemonProcess.processID(), targetDigest)
		resumeResult := make(chan error, 1)
		go func() {
			_, resumeErr := control.MaintenanceResume(context.Background(), pikaSocket, protocol.MaintenanceResumeRequest{RequestID: "resume-crash-1"})
			resumeResult <- resumeErr
		}()
		daemonProcess.waitForResumeCheckpointOrError(t, "intent_persisted", resumeResult)
		intent, intentErr := maintenance.NewFileStore(workspace.Root).Read()
		if intentErr != nil || intent.State != maintenance.StateHolding || intent.ResumeRequestID != "resume-crash-1" {
			t.Fatalf("persisted resume intent=%+v err=%v", intent, intentErr)
		}
		t.Logf("target resume checkpoint=%s pid=%d digest=%s schema=%d request=%s",
			"intent_persisted", daemonProcess.processID(), targetDigest,
			sqliteSchemaVersion(t, workspace.DatabasePath), intent.ResumeRequestID)
		daemonProcess.killAndWait()
		daemonProcess = nil
		<-resumeResult
		assertOpenRejectsExtraAgent("intent_persisted")

		daemonProcess = startWorkspaceDaemonViaOpen(targetBinary, "runtime_started")
		assertProcessExecutableDigest(t, daemonProcess.processID(), targetDigest)
		resumeResult = make(chan error, 1)
		go func() {
			_, resumeErr := control.MaintenanceResume(context.Background(), pikaSocket, protocol.MaintenanceResumeRequest{RequestID: "resume-crash-1"})
			resumeResult <- resumeErr
		}()
		daemonProcess.waitForResumeCheckpointOrError(t, "runtime_started", resumeResult)
		runtimeStarted, runtimeErr := maintenance.NewFileStore(workspace.Root).Read()
		if runtimeErr != nil || runtimeStarted.State != maintenance.StateHolding || runtimeStarted.ResumeRequestID != "resume-crash-1" {
			t.Fatalf("runtime-started resume intent=%+v err=%v", runtimeStarted, runtimeErr)
		}
		replacement := waitForNamedAgent(t, ctx, client, agentNameValue(agent))
		if replacement.TerminalID == agent.TerminalID || pikaAgentCount(t, ctx, client) != 1 {
			t.Fatalf("runtime-started checkpoint did not have one successor: old=%+v replacement=%+v agents=%s",
				agent, replacement, pikaAgentIdentities(mustHerdrSnapshot(t, ctx, client)))
		}
		t.Logf("target resume checkpoint=%s pid=%d digest=%s schema=%d request=%s successor_terminal=%s",
			"runtime_started", daemonProcess.processID(), targetDigest,
			sqliteSchemaVersion(t, workspace.DatabasePath), runtimeStarted.ResumeRequestID, replacement.TerminalID)
		daemonProcess.killAndWait()
		daemonProcess = nil
		<-resumeResult
		assertOpenRejectsExtraAgent("runtime_started")

		daemonProcess = startWorkspaceDaemonViaOpen(targetBinary)
		assertProcessExecutableDigest(t, daemonProcess.processID(), targetDigest)
		resumed, resumeErr := control.MaintenanceResume(ctx, pikaSocket, protocol.MaintenanceResumeRequest{RequestID: "resume-crash-1"})
		if resumeErr != nil || resumed.State != maintenance.StateResumed || resumed.ResumeRequestID != "resume-crash-1" {
			t.Fatalf("retried resume state=%+v err=%v", resumed, resumeErr)
		}
		assertSingleSuccessorDispatch(t, workspace.DatabasePath, prepared.Targets[0].WorkID)
		if pikaAgentCount(t, ctx, client) != 1 {
			t.Fatalf("resume retry launched duplicate Agents: %s", pikaAgentIdentities(mustHerdrSnapshot(t, ctx, client)))
		}
		t.Logf("target resume durable state=%s schema=%d request=%s", resumed.State, sqliteSchemaVersion(t, workspace.DatabasePath), resumed.ResumeRequestID)
		daemonProcess.killAndWait()
		daemonProcess = nil
		return
	}
	if _, err := control.MaintenanceResume(ctx, pikaSocket, protocol.MaintenanceResumeRequest{RequestID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	replacement := waitForNamedAgent(t, ctx, client, agentNameValue(agent))
	if replacement.TerminalID == agent.TerminalID {
		t.Fatalf("resume reused terminal %s", agent.TerminalID)
	}
	if pikaAgentCount(t, ctx, client) != 1 {
		t.Fatalf("resume launched more than one successor")
	}
	daemonProcess.killAndWait()
	daemonProcess = nil
}

func assertSchema23HoldingProjection(t *testing.T, databasePath string) {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var legacyFlows, total int
	if err := database.QueryRow("SELECT COUNT(*) FILTER (WHERE flow_version = 1), COUNT(*) FROM optimizations").Scan(&legacyFlows, &total); err != nil {
		t.Fatal(err)
	}
	if total == 0 || legacyFlows != total {
		t.Fatalf("schema23 holding changed legacy Optimization flow: flow1=%d total=%d", legacyFlows, total)
	}
	for _, table := range []string{
		"skill_snapshots", "skill_snapshot_entries", "diagnoses", "iteration_experiments",
		"benchmark_experiment_derived_comparisons", "commit_capability_receipts",
	} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&count); err != nil {
			t.Fatalf("read schema23 projection table %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("schema23 holding activated flow-v2 table %s: rows=%d", table, count)
		}
	}
	var indexCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'index' AND name = 'evidence_artifacts_work_relative_path'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatalf("schema23 projection index count=%d", indexCount)
	}
}

func assertMaintenanceOpenHerdrSnapshot(t *testing.T, before, after herdr.Snapshot, workspaceID, tabID, controlPaneID, daemonPaneID string) {
	t.Helper()
	if !reflect.DeepEqual(before.Workspaces, after.Workspaces) ||
		!reflect.DeepEqual(before.Tabs, after.Tabs) ||
		!reflect.DeepEqual(before.Layouts, after.Layouts) {
		t.Fatalf("maintenance open changed Herdr workspace/tab/layout snapshot:\nbefore=%+v\nafter=%+v", before, after)
	}
	if !reflect.DeepEqual(before.Agents, after.Agents) {
		t.Fatalf("maintenance open changed complete Herdr Agent/session snapshot:\nbefore=%+v\nafter=%+v", before.Agents, after.Agents)
	}
	if !reflect.DeepEqual(herdrPaneIdentities(before), herdrPaneIdentities(after)) {
		t.Fatalf("maintenance open changed complete Herdr pane identity snapshot:\nbefore=%+v\nafter=%+v",
			herdrPaneIdentities(before), herdrPaneIdentities(after))
	}
	for _, paneID := range []string{controlPaneID, daemonPaneID} {
		beforePane, beforeOK := herdrPaneByID(before, paneID)
		afterPane, afterOK := herdrPaneByID(after, paneID)
		if !beforeOK || !afterOK {
			t.Fatalf("maintenance open lost frozen pane %s: before=%t after=%t", paneID, beforeOK, afterOK)
		}
		if beforePane.PaneID != afterPane.PaneID || beforePane.TerminalID != afterPane.TerminalID ||
			beforePane.WorkspaceID != workspaceID || afterPane.WorkspaceID != workspaceID ||
			beforePane.TabID != tabID || afterPane.TabID != tabID ||
			!reflect.DeepEqual(beforePane.Agent, afterPane.Agent) {
			t.Fatalf("maintenance open drifted frozen pane %s:\nbefore=%+v\nafter=%+v", paneID, beforePane, afterPane)
		}
	}
}

func assertMaintenanceDirectDaemonRejected(
	t *testing.T,
	binary string,
	workspace optimizationworkspace.Workspace,
	daemonSocket, herdrSocket, daemonPaneID string,
) {
	t.Helper()
	binding, err := workspace.ReadHerdrBinding()
	if err != nil {
		t.Fatal(err)
	}
	bindingBytes, err := os.ReadFile(workspace.HerdrBindingPath)
	if err != nil {
		t.Fatal(err)
	}
	type directCase struct {
		name          string
		socketPath    string
		environment   map[string]string
		binding       *optimizationworkspace.HerdrBinding
		removeBinding bool
	}
	wrongSocket := filepath.Join(filepath.Dir(daemonSocket), "wrong.sock")
	missingControl := binding
	missingControl.ControlPane = binding.ControlPane + "-missing"
	missingDaemon := binding
	missingDaemon.DaemonPane = binding.DaemonPane + "-missing"
	cases := []directCase{
		{name: "missing Herdr environment", socketPath: daemonSocket},
		{name: "mismatched Herdr socket", socketPath: daemonSocket, environment: map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket + "-wrong", "HERDR_PANE_ID": daemonPaneID,
		}},
		{name: "mismatched daemon pane environment", socketPath: daemonSocket, environment: map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket, "HERDR_PANE_ID": daemonPaneID + "-wrong",
		}},
		{name: "mismatched daemon socket argument", socketPath: wrongSocket, environment: map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket, "HERDR_PANE_ID": daemonPaneID,
		}},
		{name: "missing frozen binding", socketPath: daemonSocket, environment: map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket, "HERDR_PANE_ID": daemonPaneID,
		}, removeBinding: true},
		{name: "missing control pane", socketPath: daemonSocket, environment: map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket, "HERDR_PANE_ID": daemonPaneID,
		}, binding: &missingControl},
		{name: "missing daemon pane", socketPath: daemonSocket, environment: map[string]string{
			"HERDR_SOCKET_PATH": herdrSocket, "HERDR_PANE_ID": missingDaemon.DaemonPane,
		}, binding: &missingDaemon},
	}
	for _, testCase := range cases {
		t.Run("direct daemon rejects "+testCase.name, func(t *testing.T) {
			if testCase.removeBinding {
				if err := os.Remove(workspace.HerdrBindingPath); err != nil {
					t.Fatal(err)
				}
			} else if testCase.binding != nil {
				if err := workspace.WriteHerdrBinding(*testCase.binding); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				if err := os.WriteFile(workspace.HerdrBindingPath, bindingBytes, 0o600); err != nil {
					t.Error(err)
				}
				_ = os.Remove(testCase.socketPath)
			})
			commandCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			command := exec.CommandContext(commandCtx, binary, "daemon", "--workspace", workspace.Root, "--socket", testCase.socketPath)
			command.Env = environmentWithout("HERDR_SOCKET_PATH", "HERDR_PANE_ID")
			for key, value := range testCase.environment {
				command.Env = append(command.Env, key+"="+value)
			}
			output, commandErr := command.CombinedOutput()
			if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
				t.Fatalf("direct daemon bypassed maintenance-aware open validation: %s", output)
			}
			if commandErr == nil {
				t.Fatalf("direct daemon unexpectedly succeeded: %s", output)
			}
			if schema := sqliteSchemaVersion(t, workspace.DatabasePath); schema != 20 {
				t.Fatalf("rejected direct daemon migrated schema to %d: %s", schema, output)
			}
			status, statusErr := maintenance.NewFileStore(workspace.Root).Read()
			if statusErr != nil || status.State != maintenance.StateReady {
				t.Fatalf("rejected direct daemon changed maintenance state: status=%+v err=%v output=%s", status, statusErr, output)
			}
		})
	}
}

func environmentWithout(keys ...string) []string {
	excluded := make(map[string]bool, len(keys))
	for _, key := range keys {
		excluded[key] = true
	}
	result := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !excluded[key] {
			result = append(result, entry)
		}
	}
	return result
}

type herdrPaneIdentity struct {
	PaneID        string
	TerminalID    string
	WorkspaceID   string
	TabID         string
	Agent         *string
	Tokens        map[string]string
	ForegroundCWD *string
}

func herdrPaneIdentities(snapshot herdr.Snapshot) []herdrPaneIdentity {
	result := make([]herdrPaneIdentity, 0, len(snapshot.Panes))
	for _, pane := range snapshot.Panes {
		result = append(result, herdrPaneIdentity{
			PaneID: pane.PaneID, TerminalID: pane.TerminalID, WorkspaceID: pane.WorkspaceID, TabID: pane.TabID,
			Agent: pane.Agent, Tokens: pane.Tokens, ForegroundCWD: pane.ForegroundCWD,
		})
	}
	slices.SortFunc(result, func(left, right herdrPaneIdentity) int {
		return strings.Compare(left.PaneID, right.PaneID)
	})
	return result
}

func herdrPaneByID(snapshot herdr.Snapshot, paneID string) (herdr.Pane, bool) {
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return pane, true
		}
	}
	return herdr.Pane{}, false
}

func pikaAgentIdentities(snapshot herdr.Snapshot) string {
	var identities []string
	for _, agent := range snapshot.Agents {
		if agent.Name != nil && strings.HasPrefix(*agent.Name, "pika-") {
			identities = append(identities, fmt.Sprintf("%s:%s:%s", *agent.Name, agent.PaneID, agent.TerminalID))
		}
	}
	return strings.Join(identities, ",")
}

func pikaAgents(t *testing.T, ctx context.Context, client *herdr.Client) []herdr.Agent {
	t.Helper()
	snapshot := mustHerdrSnapshot(t, ctx, client)
	var result []herdr.Agent
	for _, agent := range snapshot.Agents {
		if agent.Name != nil && strings.HasPrefix(*agent.Name, "pika-") {
			result = append(result, agent)
		}
	}
	slices.SortFunc(result, func(left, right herdr.Agent) int {
		return strings.Compare(left.TerminalID, right.TerminalID)
	})
	return result
}

func mustHerdrSnapshot(t *testing.T, ctx context.Context, client *herdr.Client) herdr.Snapshot {
	t.Helper()
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func waitForPostResumeTerminalProgress(t *testing.T, databasePath, frozenWorkID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var verificationWorks, verificationSessions, terminalEvents int
		queryErr := database.QueryRow(`SELECT
			(SELECT COUNT(*) FROM works WHERE role = 'baseline_verification' AND status = 'completed' AND id <> ?),
			(SELECT COUNT(*) FROM agent_sessions s JOIN works w ON w.id = s.work_id WHERE w.role = 'baseline_verification'),
			(SELECT COUNT(*) FROM domain_events WHERE event_type = 'baseline.verification_finished')`, frozenWorkID).
			Scan(&verificationWorks, &verificationSessions, &terminalEvents)
		_ = database.Close()
		if queryErr == nil && verificationWorks == 1 && verificationSessions == 1 && terminalEvents == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("resumed bridge did not produce singular terminal successor progress")
}

func assertSingleSuccessorDispatch(t *testing.T, databasePath, frozenWorkID string) {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var works, sessions, starts int
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM works WHERE role = 'baseline_verification' AND id <> ?),
		(SELECT COUNT(*) FROM agent_sessions s JOIN works w ON w.id = s.work_id WHERE w.role = 'baseline_verification' AND w.id <> ?),
		(SELECT COUNT(*) FROM runtime_outbox o JOIN works w ON json_extract(o.payload_json, '$.work_id') = w.id
			WHERE o.effect_type = 'work.start_requested' AND w.role = 'baseline_verification' AND w.id <> ?)`,
		frozenWorkID, frozenWorkID, frozenWorkID).Scan(&works, &sessions, &starts); err != nil {
		t.Fatal(err)
	}
	if works != 1 || sessions != 1 || starts != 1 {
		t.Fatalf("resume retry duplicated successor: works=%d sessions=%d start_effects=%d", works, sessions, starts)
	}
}

type durableTableSnapshot struct {
	Columns []string
	Rows    [][]string
}

type durableHoldingSnapshot map[string]durableTableSnapshot

func captureDurableHoldingSnapshot(t *testing.T, databasePath string) durableHoldingSnapshot {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	result := durableHoldingSnapshot{}
	rows, err := database.Query(`SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'migrations'
		ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"baseline_revisions", "baseline_verifications", "operation_receipts"} {
		if !slices.Contains(tables, required) {
			t.Fatalf("schema20 holding snapshot omitted required table %s: %v", required, tables)
		}
	}
	for _, table := range tables {
		result[table] = readDurableTableSnapshot(t, database, table, nil)
	}
	return result
}

func assertDurableHoldingSnapshotUnchanged(t *testing.T, databasePath string, before durableHoldingSnapshot) {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for table, expected := range before {
		actual := readDurableTableSnapshot(t, database, table, expected.Columns)
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("holding mutated durable %s state:\nbefore=%+v\nafter=%+v", table, expected, actual)
		}
	}
}

func assertHoldingMutationRejected(t *testing.T, socketPath, path, body string) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodPost, "http://pika"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Holding mutation %s: %v", path, err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusConflict || !bytes.Contains(responseBody, []byte("maintenance_mutation_rejected")) {
		t.Fatalf("Holding mutation %s status=%d body=%s", path, response.StatusCode, responseBody)
	}
}

func snapshotHoldingTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += "\x00" + target
		} else if !entry.IsDir() {
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += "\x00" + string(contents)
		}
		result[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func readDurableTableSnapshot(t *testing.T, database *sql.DB, table string, columns []string) durableTableSnapshot {
	t.Helper()
	quoteIdentifier := func(value string) string {
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	if len(columns) == 0 {
		rows, err := database.Query("PRAGMA table_info(" + quoteIdentifier(table) + ")")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var sequence, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if err := rows.Scan(&sequence, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			columns = append(columns, name)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	quoted := make([]string, len(columns))
	for index, column := range columns {
		quoted[index] = quoteIdentifier(column)
	}
	rows, err := database.Query("SELECT " + strings.Join(quoted, ",") + " FROM " + quoteIdentifier(table) + " ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := durableTableSnapshot{Columns: slices.Clone(columns)}
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			t.Fatal(err)
		}
		encoded := make([]string, len(values))
		for index, value := range values {
			switch typed := value.(type) {
			case nil:
				encoded[index] = "null"
			case []byte:
				encoded[index] = fmt.Sprintf("bytes:%x", typed)
			default:
				encoded[index] = fmt.Sprintf("%T:%v", typed, typed)
			}
		}
		result.Rows = append(result.Rows, encoded)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

type rawMaintenanceCommit struct {
	schema int
}

func inspectMaintenanceCommitRaw(t *testing.T, databasePath string, target maintenance.Target) rawMaintenanceCommit {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var result rawMaintenanceCommit
	if err := database.QueryRow("SELECT MAX(version) FROM migrations").Scan(&result.schema); err != nil {
		t.Fatal(err)
	}
	var workStatus string
	var workGeneration int64
	if err := database.QueryRow("SELECT status, generation FROM works WHERE id = ?", target.WorkID).Scan(&workStatus, &workGeneration); err != nil {
		t.Fatal(err)
	}
	if workStatus != string(symphony.WorkCompleted) || workGeneration != target.WorkGeneration {
		t.Fatalf("frozen Work terminal identity changed: status=%s generation=%d target=%+v", workStatus, workGeneration, target)
	}
	var sessionWorkID, sessionStatus string
	var sessionGeneration int64
	if err := database.QueryRow("SELECT work_id, generation, status FROM agent_sessions WHERE id = ?", target.SessionID).
		Scan(&sessionWorkID, &sessionGeneration, &sessionStatus); err != nil {
		t.Fatal(err)
	}
	if sessionWorkID != target.WorkID || sessionGeneration != target.WorkGeneration || sessionStatus == string(symphony.AgentSessionLost) {
		t.Fatalf("frozen Session identity changed: work=%s generation=%d status=%s target=%+v", sessionWorkID, sessionGeneration, sessionStatus, target)
	}
	var works, sessions, events int
	if err := database.QueryRow("SELECT COUNT(*) FROM works").Scan(&works); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM agent_sessions WHERE work_id = ? AND generation = ?", target.WorkID, target.WorkGeneration).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM domain_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	var terminalPayload []byte
	if err := database.QueryRow(`SELECT e.payload_json
		FROM operation_receipts r JOIN domain_events e ON e.revision = r.revision
		WHERE r.command_type = 'submit_baseline_definition' AND e.event_type = 'baseline.submitted'`).
		Scan(&terminalPayload); err != nil {
		t.Fatalf("read frozen terminal receipt/event: %v", err)
	}
	var terminalEvent map[string]any
	if err := json.Unmarshal(terminalPayload, &terminalEvent); err != nil {
		t.Fatal(err)
	}
	if works != 2 || sessions != 1 || events != 2 || terminalEvent["draft_work_id"] != target.WorkID {
		t.Fatalf("terminal mutation was not singular or frozen: works=%d sessions=%d events=%d payload=%v", works, sessions, events, terminalEvent)
	}
	return result
}

func sqliteSchemaVersion(t *testing.T, databasePath string) int {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var version int
	if err := database.QueryRow("SELECT MAX(version) FROM migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func binaryProbe(t *testing.T, binary string) daemonupdate.Probe {
	t.Helper()
	output, err := exec.Command(binary, "update-probe").Output()
	if err != nil {
		t.Fatalf("probe %s: %v", binary, err)
	}
	var probe daemonupdate.Probe
	if err := json.Unmarshal(output, &probe); err != nil {
		t.Fatalf("decode probe %s: %v", binary, err)
	}
	return probe
}

func stopDaemonWithSignal(t *testing.T, process *daemonProcess, signal os.Signal) {
	t.Helper()
	owner, err := os.FindProcess(process.processID())
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Signal(signal); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-process.done:
		if err != nil {
			t.Fatalf("daemon signal exit: %v output=%s", err, process.output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("daemon did not stop after %v", signal)
	}
}

func sourcePath(t *testing.T) string {
	t.Helper()
	_, path, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return path
}

func assertProcessExecutableDigest(t *testing.T, pid int, want string) {
	t.Helper()
	got, err := daemonupdate.FileDigest(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		t.Fatalf("hash /proc/%d/exe: %v", pid, err)
	}
	if got != want {
		t.Fatalf("/proc/%d/exe digest = %s, want %s", pid, got, want)
	}
	t.Logf("/proc/%d/exe digest=%s", pid, got)
}

func pikaAgentCount(t *testing.T, ctx context.Context, client *herdr.Client) int {
	t.Helper()
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, agent := range snapshot.Agents {
		if agent.Name != nil && strings.HasPrefix(*agent.Name, "pika-") {
			count++
		}
	}
	return count
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
	defer reader.Close()
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
	var recovered symphony.OperatorStatus
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
	var view symphony.OperatorStatus
	for time.Now().Before(deadline) {
		view, err = control.Status(ctx, pikaSocket)
		if err == nil && view.Optimization.Status == symphony.OptimizationOptimizing && len(view.Works) == 7 &&
			view.Works[0].Status == symphony.WorkCompleted && view.Works[1].Status == symphony.WorkCompleted &&
			view.Works[2].Role == symphony.RoleDiagnosis && view.Works[2].Status == symphony.WorkCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || view.Optimization.Status != symphony.OptimizationOptimizing || view.Baseline == nil || view.Baseline.Status != symphony.BaselineAccepted || len(view.Works) != 7 || view.Works[0].Status != symphony.WorkCompleted || view.Works[1].Status != symphony.WorkCompleted || view.Works[2].Role != symphony.RoleDiagnosis || view.Works[2].Status != symphony.WorkCompleted {
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
	defer reader.Close()
	var targetSession symphony.AgentSession
	for time.Now().Before(deadline) {
		var found bool
		targetSession, _, found, err = reader.CurrentAgentSession(ctx, target.ID)
		if err == nil && found && targetSession.Status == symphony.AgentSessionRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if targetSession.ID == "" {
		t.Fatal("target Agent Session did not bind")
	}
	waitForHerdrAgentStatus(t, ctx, client, targetSession.AgentName, "idle")
	hook := json.RawMessage(`{"session_id":"fake-provider-session","turn_id":"fake-stopped-turn","hook_event_name":"Stop","last_assistant_message":"waiting for guidance"}`)
	if err := control.IngestProviderEvent(ctx, pikaSocket, "codex", protocol.ProviderEventRequest{AgentSessionID: targetSession.ID, Event: hook}); err != nil {
		t.Fatal(err)
	}
	var view symphony.OperatorStatus
	deadline = time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		view, err = control.Status(ctx, pikaSocket)
		if err == nil && len(view.FollowUps) != 0 && view.FollowUps[0].Status == "delivered" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || len(view.FollowUps) != 1 || view.FollowUps[0].Status != "delivered" {
		t.Fatalf("Follow-up E2E view=%+v err=%v Herdr=%s", view, err, serverOutput.String())
	}
	persisted, err := reader.Inspect(ctx, symphony.Status{})
	if err != nil || len(persisted.FollowUps) != 1 || persisted.FollowUps[0].Message == "" {
		t.Fatalf("Follow-up message was not durably stored: view=%+v err=%v", persisted.FollowUps, err)
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
	var view symphony.OperatorStatus
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
	var view symphony.OperatorStatus
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
	wrapper := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo codex-test; exit 0; fi\n" +
		"if [ \"$1\" = login ] && [ \"$2\" = status ]; then echo Logged-in; exit 0; fi\n" +
		"if [ \"$1\" = debug ] && [ \"$2\" = prompt-input ] && [ -L .agents/skills/pika-kda-kernelwiki ] && [ -L .agents/skills/pika-kda-ncu-report ]; then root=\"$(pwd)/.agents/skills\"; printf '[{\"type\":\"message\",\"role\":\"developer\",\"content\":[{\"type\":\"input_text\",\"text\":\"<skills_instructions>\\\\n## Skills\\\\n### Skill roots\\\\n- `r0` = `%s`\\\\n### Available skills\\\\n- KernelWiki: probe (file: r0/pika-kda-kernelwiki/SKILL.md)\\\\n- ncu-report-skill: probe (file: r0/pika-kda-ncu-report/SKILL.md)\\\\n</skills_instructions>\"}]}]\\n' \"$root\"; exit 0; fi\n" +
		"exec env HERDR_AGENT=codex PIKA_GO_FAKE_AGENT_OSC=1 \"" + fakeAgent + "\"\n"
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
	writeHerdrIntegrationCodexModelsCache(t, configRoot)
	t.Setenv("CODEX_HOME", filepath.Join(configRoot, "codex-home"))
	t.Setenv("PIKA_GO_CODEX_EXECUTABLE", filepath.Join(os.Getenv("PIKA_GO_AGENT_PATH_PREFIX"), "codex"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var output bytes.Buffer
	var stopOnce sync.Once
	go func() {
		done <- cli.Run(ctx, []string{"daemon", "--socket", socketPath, "--state-dir", stateRoot, "--config-dir", configRoot, "--instance", "integration-instance"}, nil, &output, &output)
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
							t.Errorf("daemon exit code = %d; output=%s", code, output.String())
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
	t.Fatalf("daemon socket did not become healthy; output=%s", output.String())
	return func() {}
}

type daemonProcess struct {
	command             *exec.Cmd
	done                chan error
	output              bytes.Buffer
	socketPath          string
	pid                 int
	external            bool
	checkpointDirectory string
	checkpointPoint     string
}

func startDaemonProcess(t *testing.T, pikaBinary, socketPath, stateRoot, configRoot, herdrSocket, symphonyPane, binDir string) *daemonProcess {
	t.Helper()
	writeHerdrIntegrationCodexModelsCache(t, configRoot)
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
		"PIKA_GO_CODEX_EXECUTABLE":  filepath.Join(binDir, "codex"),
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

func writeHerdrIntegrationCodexModelsCache(t *testing.T, configRoot string) {
	t.Helper()
	codexHome := filepath.Join(configRoot, "codex-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("create Codex model cache directory: %v", err)
	}
	cache := []byte(`{"models":[{"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},{"slug":"gpt-5.6-terra","display_name":"GPT-5.6-Terra","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]}]}`)
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), cache, 0o600); err != nil {
		t.Fatalf("write Codex model cache: %v", err)
	}
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
	} else if process.command != nil && process.command.ProcessState == nil {
		_ = process.command.Process.Kill()
	}
	select {
	case <-process.done:
	case <-time.After(5 * time.Second):
	}
	if process.checkpointDirectory != "" && process.checkpointPoint != "" {
		for _, suffix := range []string{".enable", ".reached", ".continue"} {
			_ = os.Remove(filepath.Join(process.checkpointDirectory, process.checkpointPoint+suffix))
		}
	}
}

func (process *daemonProcess) processID() int {
	if process.pid > 0 {
		return process.pid
	}
	return process.command.Process.Pid
}

func processExists(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (process *daemonProcess) waitForResumeCheckpoint(t *testing.T, point string) {
	t.Helper()
	if process.checkpointDirectory == "" || process.checkpointPoint != point {
		t.Fatal("daemon has no matching resume checkpoint")
	}
	reached := filepath.Join(process.checkpointDirectory, point+".reached")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if contents, err := os.ReadFile(reached); err == nil {
			if strings.TrimSpace(string(contents)) != point {
				t.Fatalf("resume checkpoint = %q, want %q", strings.TrimSpace(string(contents)), point)
			}
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("wait for resume checkpoint %s: %v output=%s", point, err, process.output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for resume checkpoint %s output=%s", point, process.output.String())
}

func (process *daemonProcess) waitForResumeCheckpointOrError(t *testing.T, point string, result <-chan error) {
	t.Helper()
	if process.checkpointDirectory == "" || process.checkpointPoint != point {
		t.Fatal("daemon has no matching resume checkpoint")
	}
	reached := filepath.Join(process.checkpointDirectory, point+".reached")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(reached); err == nil {
			return
		}
		select {
		case err := <-result:
			t.Fatalf("resume failed before checkpoint %s: %v output=%s", point, err, process.output.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for resume checkpoint %s output=%s", point, process.output.String())
}

func waitForPikaProcessHealth(t *testing.T, socketPath string, process *daemonProcess) {
	waitForPikaProcessHealthWithin(t, socketPath, process, 15*time.Second)
}

func waitForPikaProcessHealthWithin(t *testing.T, socketPath string, process *daemonProcess, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
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
