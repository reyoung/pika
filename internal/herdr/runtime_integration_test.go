package herdr_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/herdr"
)

func TestRuntimeWithRealHerdrAndFakeAgent(t *testing.T) {
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

	root, err := os.MkdirTemp("/tmp", "pika-go-herdr-int-")
	if err != nil {
		t.Fatalf("create short integration directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	configHome := filepath.Join(root, "config")
	stateHome := filepath.Join(root, "state")
	herdrConfig := filepath.Join(configHome, "herdr")
	if err := os.MkdirAll(filepath.Join(herdrConfig, "agent-detection"), 0o700); err != nil {
		t.Fatalf("create Herdr config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(herdrConfig, "config.toml"), []byte("[update]\nversion_check = false\nmanifest_check = false\n"), 0o600); err != nil {
		t.Fatalf("write Herdr config: %v", err)
	}
	_, sourceFile, _, _ := runtime.Caller(0)
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "..", "..", "testdata", "herdr", "codex.toml"))
	if err != nil {
		t.Fatalf("read fake detection manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(herdrConfig, "agent-detection", "codex.toml"), manifest, 0o600); err != nil {
		t.Fatalf("write fake detection manifest: %v", err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatalf("create test bin directory: %v", err)
	}
	wrapper := "#!/bin/sh\nexec env HERDR_AGENT=codex PIKA_GO_FAKE_AGENT_OSC=1 \"$PIKA_GO_FAKE_AGENT_BIN\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write codex test wrapper: %v", err)
	}

	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	command := exec.CommandContext(serverCtx, herdrBinary, "server")
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"XDG_CONFIG_HOME":        configHome,
		"XDG_STATE_HOME":         stateHome,
		"PIKA_GO_FAKE_AGENT_BIN": fakeAgent,
		"PATH":                   binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	var serverOutput bytes.Buffer
	command.Stdout = &serverOutput
	command.Stderr = &serverOutput
	if err := command.Start(); err != nil {
		t.Fatalf("start Herdr server: %v", err)
	}
	t.Cleanup(func() {
		stopServer()
		_ = command.Wait()
	})
	socketPath := filepath.Join(herdrConfig, "herdr.sock")
	waitForSocket(t, socketPath, &serverOutput)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := herdr.NewClient(socketPath)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{"cwd": root, "label": "pika-go-runtime-test", "focus": false}, &created); err != nil {
		t.Fatalf("create workspace: %v; server output: %s", err, serverOutput.String())
	}
	runtimeAdapter := herdr.NewRuntime(client)
	if err := runtimeAdapter.ReportInstance(ctx, created.Workspace.WorkspaceID, "instance-test"); err != nil {
		t.Fatalf("report instance metadata: %v", err)
	}
	pane, err := runtimeAdapter.SplitPane(ctx, created.RootPane.PaneID, "down", root)
	if err != nil {
		t.Fatalf("split pane: %v", err)
	}
	agent, err := runtimeAdapter.Start(ctx, herdr.StartSpec{Name: "pika-test", Kind: "codex", PaneID: pane.PaneID, TimeoutMS: 15000})
	if err != nil {
		t.Fatalf("start fake agent: %v; server output: %s", err, serverOutput.String())
	}
	if agent.PaneID != pane.PaneID || !agent.InteractiveReady {
		var manifests, explain json.RawMessage
		_ = client.Call(ctx, "server.agent_manifests", map[string]any{}, &manifests)
		_ = client.Call(ctx, "agent.explain", map[string]string{"target": pane.PaneID}, &explain)
		t.Fatalf("started agent = %+v; manifests=%s; explain=%s", agent, manifests, explain)
	}
	if _, err := runtimeAdapter.Prompt(ctx, pane.PaneID, "hello from pika-go"); err != nil {
		t.Fatalf("prompt fake agent: %v", err)
	}
	var snapshotResult struct {
		Snapshot herdr.Snapshot `json:"snapshot"`
	}
	if err := client.Call(ctx, "session.snapshot", map[string]any{}, &snapshotResult); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snapshot := snapshotResult.Snapshot
	if len(snapshot.Workspaces) != 1 || snapshot.Workspaces[0].Tokens["pika_instance"] != "instance-test" {
		t.Fatalf("workspace metadata = %+v", snapshot.Workspaces)
	}
	foundAgent := false
	for _, observed := range snapshot.Agents {
		if observed.Name != nil && *observed.Name == "pika-test" && observed.TerminalID == pane.TerminalID {
			foundAgent = true
		}
	}
	if !foundAgent {
		t.Fatalf("snapshot agents = %+v, want pika-test on %s", snapshot.Agents, pane.TerminalID)
	}
	var moved struct {
		MoveResult struct {
			Pane herdr.Pane `json:"pane"`
		} `json:"move_result"`
	}
	if err := client.Call(ctx, "pane.move", map[string]any{
		"pane_id":     pane.PaneID,
		"destination": map[string]any{"type": "new_workspace", "label": "moved-agent", "tab_label": "main"},
		"focus":       false,
	}, &moved); err != nil {
		t.Fatalf("move agent pane: %v", err)
	}
	if moved.MoveResult.Pane.PaneID == pane.PaneID || moved.MoveResult.Pane.TerminalID != pane.TerminalID {
		t.Fatalf("moved pane = %+v, original = %+v", moved.MoveResult.Pane, pane)
	}
	if err := runtimeAdapter.ClosePane(ctx, moved.MoveResult.Pane.PaneID); err != nil {
		t.Fatalf("close fake agent pane: %v", err)
	}
	_ = client.Call(ctx, "workspace.close", map[string]string{"workspace_id": created.Workspace.WorkspaceID}, nil)
}

func waitForSocket(t *testing.T, socketPath string, serverOutput *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("unix", socketPath, 50*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Herdr socket did not appear: %s", serverOutput.String())
}

func replaceEnvironment(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
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
