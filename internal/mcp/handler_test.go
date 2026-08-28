package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemon"
	"github.com/reyoung/pika-go/internal/mcp"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
)

func TestStdioProxyForwardsRoleScopedMCPAndTerminalReceipt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatalf("init: %v", err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	effects, _ := engine.PendingEffects(ctx, 1)
	session := symphony.AgentSession{ID: effects[0].ID, WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-test", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1", TerminalID: "term-1"}); err != nil {
		t.Fatalf("bind session: %v", err)
	}
	grant, err := engine.MintAgentGrant(ctx, session.ID, toolapp.CatalogForRole(session.Role), time.Hour)
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}

	socketDir, err := os.MkdirTemp("/tmp", "pika-go-mcp-test-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mutated := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(serveCtx, daemon.Config{
			SocketPath: socketPath,
			Version:    "test",
			Symphony:   engine,
			MCPHandler: mcp.Handler{Application: toolapp.Application{Store: engine, WorktreeRoot: worktreeRoot}, AfterMutation: func() { mutated <- struct{}{} }},
		})
	}()
	waitHealth(t, socketPath)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"submit_baseline_definition","arguments":{"idempotency_key":"terminal","definition":%s}}}`, testcontract.Definition()),
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := mcp.RunProxy(ctx, socketPath, grant.Token, strings.NewReader(input), &output); err != nil {
		t.Fatalf("run MCP proxy: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("MCP response lines = %d: %s", len(lines), output.String())
	}
	var listed struct {
		Result struct {
			Tools []toolapp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil || len(listed.Result.Tools) != 2 {
		t.Fatalf("tools/list response=%s err=%v", lines[1], err)
	}
	select {
	case <-mutated:
	case <-time.After(time.Second):
		t.Fatal("terminal mutation callback was not delivered")
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil || view.Optimization.Status != symphony.OptimizationVerifyingBaseline || len(view.Works) != 2 {
		t.Fatalf("view after MCP terminal call=%+v err=%v", view, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func runGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", repository}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func waitHealth(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := control.Health(context.Background(), socketPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon did not become healthy")
}
