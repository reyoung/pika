package provider_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestCursorPrepareSessionCreatesPrivateStateAndShellSafeLaunch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realCursor := filepath.Join(root, "real-cursor-agent")
	if err := os.WriteFile(realCursor, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pikaExecutable := filepath.Join(root, "pika-go")
	if err := os.WriteFile(pikaExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	instanceBin := filepath.Join(runtimeRoot, "bin")
	adapter := provider.NewCursorAdapter(provider.CursorOptions{
		Executable: realCursor, RuntimeRoot: runtimeRoot, InstanceBin: instanceBin, PikaExecutable: pikaExecutable,
	})
	launch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "session-123",
		Repository:     filepath.Join(root, "repo with spaces"),
		Configuration: provider.AgentConfiguration{
			Kind: "cursor", Model: "gpt-5.6-sol", ReasoningEffort: "high",
			Args: []string{"--force", "--header", "X-Test: $(touch should-not-run)"},
		},
		SystemPrompt:  []byte("immutable role prompt\n\ndynamic context\n\nuser instructions"),
		InitialPrompt: "start the work safely",
		Environment: map[string]string{
			"PIKA_SESSION_ID": "session-123", "PIKA_GO_SOCKET": "/tmp/pika socket.sock", "PIKA_MCP_GRANT": "secret grant",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := launch.EphemeralPath
	if sessionDir == "" || launch.Environment["CURSOR_CONFIG_DIR"] != "" || launch.Environment["PIKA_GO_EXECUTABLE"] != pikaExecutable || launch.Environment["PIKA_CURSOR_SYSTEM_PROMPT_PATH"] != filepath.Join(sessionDir, "system-prompt.md") || launch.Environment["PIKA_CURSOR_MODEL"] != "gpt-5.6-sol-high" || launch.StartupTimeout != 2*time.Minute || !launch.HandlesInitialPrompt || !launch.ReturnOnLaunch {
		t.Fatalf("launch = %+v", launch)
	}
	for _, path := range []string{
		filepath.Join(sessionDir, "system-prompt.md"),
		filepath.Join(sessionDir, "cursor.args"),
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("generated file %s: info=%v err=%v", path, info, err)
		}
	}
	systemPrompt, _ := os.ReadFile(filepath.Join(sessionDir, "system-prompt.md"))
	if !strings.Contains(string(systemPrompt), "immutable role prompt") || !strings.Contains(string(systemPrompt), "user instructions") {
		t.Fatalf("system prompt = %s", systemPrompt)
	}
	wrapper := filepath.Join(instanceBin, "cursor-agent")
	command := exec.Command(wrapper)
	command.Env = os.Environ()
	for key, value := range launch.Environment {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run Cursor wrapper: %v: %s", err, output)
	}
	wantArgs := []string{
		"--force", "--header", "X-Test: $(touch should-not-run)",
		"--workspace", filepath.Join(root, "repo with spaces"),
		"--model", "gpt-5.6-sol-high", "--sandbox", "enabled", "start the work safely",
	}
	if strings.TrimSpace(string(output)) != strings.Join(wantArgs, "\n") {
		t.Fatalf("argv:\n%s\nwant:\n%s", output, strings.Join(wantArgs, "\n"))
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
		t.Fatal("Cursor arg was evaluated by a shell")
	}
	resume := exec.Command(wrapper, "--resume", "native-session")
	resume.Env = command.Env
	if err := resume.Run(); err == nil {
		t.Fatal("Cursor wrapper accepted native resume")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 64 {
		t.Fatalf("native resume exit = %v", err)
	}
	if err := launch.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral Cursor Session state still exists: %v", err)
	}
}

func TestRealCursorDiscoversInstalledPikaMCP(t *testing.T) {
	if os.Getenv("PIKA_GO_REAL_CURSOR_PLUGIN_DISCOVERY") != "1" {
		t.Skip("set PIKA_GO_REAL_CURSOR_PLUGIN_DISCOVERY=1 to test the installed Cursor CLI")
	}
	cursorExecutable, err := exec.LookPath("cursor-agent")
	if err != nil {
		t.Fatal(err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := provider.InstallCursorMCP(filepath.Join(userHome, ".cursor", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer rollback()
	status := exec.Command(cursorExecutable, "status")
	if output, err := status.CombinedOutput(); err != nil {
		t.Fatalf("Cursor authentication is unavailable: %v: %s", err, output)
	} else if strings.Contains(string(output), "Not logged in") || !strings.Contains(string(output), "Logged in") {
		t.Fatalf("Cursor authentication is unavailable: %s", output)
	}
	command := exec.Command(cursorExecutable, "mcp", "list")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list installed Cursor MCP servers: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "pika_go") {
		t.Fatalf("Cursor did not discover the Pika MCP server: %s", output)
	}
}

func TestReconcileCursorSessionsPreservesOnlyActiveSessions(t *testing.T) {
	t.Parallel()
	runtimeRoot := t.TempDir()
	for _, id := range []string{"active", "stale"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "cursor-sessions", id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.ReconcileCursorSessions(runtimeRoot, map[string]bool{"active": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRoot, "cursor-sessions", "active")); err != nil {
		t.Fatalf("active Plugin removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRoot, "cursor-sessions", "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale Plugin remains: %v", err)
	}
}
