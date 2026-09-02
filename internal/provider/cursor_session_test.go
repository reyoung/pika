package provider_test

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

	"github.com/reyoung/pika-go/internal/provider"
)

func TestCursorPrepareSessionCreatesPrivateStateAndShellSafeLaunch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realCursor := "/bin/echo"
	pikaExecutable := currentTestExecutable(t)
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
	firstWrapperFile, err := os.Open(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	defer firstWrapperFile.Close()
	firstWrapper, err := firstWrapperFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	secondLaunch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "session-456",
		Repository:     filepath.Join(root, "second repo"),
		Configuration:  provider.AgentConfiguration{Kind: "cursor", Model: "gpt-5.6-sol", ReasoningEffort: "high"},
		SystemPrompt:   []byte("second immutable prompt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondWrapper, err := os.Stat(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstWrapper, secondWrapper) {
		t.Fatal("identical Cursor wrapper was republished between Session preparations")
	}
	t.Cleanup(func() { _ = secondLaunch.Cleanup() })
	commandEnvironment := cursorCommandEnvironment(launch.Environment)
	output, err := runPublishedCursor(t, wrapper, pikaExecutable, commandEnvironment)
	if err != nil {
		t.Fatalf("run Cursor wrapper: %v: %s", err, output)
	}
	wantArgs := []string{
		"--force", "--header", "X-Test: $(touch should-not-run)",
		"--yolo",
		"--workspace", filepath.Join(root, "repo with spaces"),
		"--model", "gpt-5.6-sol-high", "start the work safely",
	}
	if strings.TrimSpace(string(output)) != strings.Join(wantArgs, " ") {
		t.Fatalf("argv:\n%s\nwant:\n%s", output, strings.Join(wantArgs, "\n"))
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
		t.Fatal("Cursor arg was evaluated by a shell")
	}
	if _, err := runPublishedCursor(t, wrapper, pikaExecutable, commandEnvironment, "--resume", "native-session"); err == nil {
		t.Fatal("Cursor wrapper accepted native resume")
	} else if !strings.Contains(err.Error(), "exit status 64") {
		t.Fatalf("native resume exit = %v", err)
	}
	if err := launch.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral Cursor Session state still exists: %v", err)
	}
}

func TestCursorPrepareSessionCanWaitForOperatorPrompt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realCursor := "/bin/echo"
	pikaExecutable := "/bin/true"
	adapter := provider.NewCursorAdapter(provider.CursorOptions{
		Executable: realCursor, RuntimeRoot: filepath.Join(root, "runtime"),
		InstanceBin: filepath.Join(root, "runtime", "bin"), PikaExecutable: pikaExecutable,
	})
	launch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "operator-session", Repository: filepath.Join(root, "repo"),
		Configuration: provider.AgentConfiguration{Kind: "cursor", Model: "auto"},
		SystemPrompt:  []byte("system prompt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if launch.HandlesInitialPrompt || launch.ReturnOnLaunch || launch.Environment["PIKA_CURSOR_INITIAL_PROMPT"] != "" {
		t.Fatalf("operator-driven launch = %+v", launch)
	}
	output, err := runCursorWrapperDirect(cursorCommandEnvironment(launch.Environment))
	if err != nil {
		t.Fatalf("run Cursor wrapper without initial prompt: %v: %s", err, output)
	}
	want := strings.Join([]string{"--yolo", "--workspace", filepath.Join(root, "repo"), "--model", "auto"}, " ")
	if strings.TrimSpace(string(output)) != want {
		t.Fatalf("argv:\n%s\nwant:\n%s", output, want)
	}
}

func TestPublishedCursorWrapperReplacesInheritedHerdrAgent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	testExecutable := currentTestExecutable(t)
	adapter := provider.NewCursorAdapter(provider.CursorOptions{
		Executable: testExecutable, RuntimeRoot: filepath.Join(root, "runtime"),
		InstanceBin: filepath.Join(root, "runtime", "bin"), PikaExecutable: testExecutable,
	})
	launch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "environment-session", Repository: filepath.Join(root, "repo"),
		Configuration: provider.AgentConfiguration{Kind: "cursor", Model: "auto"},
		SystemPrompt:  []byte("system prompt"), InitialPrompt: "start",
		Environment: map[string]string{
			"HERDR_AGENT": "codex", "PIKA_GO_TEST_CURSOR_CHILD": "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launch.Cleanup() })
	output, err := runPublishedCursor(
		t,
		filepath.Join(root, "runtime", "bin", "cursor-agent"),
		testExecutable,
		cursorCommandEnvironment(launch.Environment),
	)
	if err != nil {
		t.Fatalf("exec published Cursor wrapper: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "HERDR_AGENT=cursor count=1" {
		t.Fatalf("child environment = %q", got)
	}
}

func TestCursorPrepareSessionUsesAutoRoutingWithoutReasoningEffort(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realCursor := "/bin/echo"
	pikaExecutable := "/bin/true"
	adapter := provider.NewCursorAdapter(provider.CursorOptions{
		Executable: realCursor, RuntimeRoot: filepath.Join(root, "runtime"),
		InstanceBin: filepath.Join(root, "runtime", "bin"), PikaExecutable: pikaExecutable,
	})
	launch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "auto-session", Repository: filepath.Join(root, "repo"),
		Configuration: provider.AgentConfiguration{Kind: "cursor", Model: "auto"},
		SystemPrompt:  []byte("system prompt"), InitialPrompt: "start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if launch.Environment["PIKA_CURSOR_MODEL"] != "auto" || launch.Environment["PIKA_AGENT_MODEL"] != "auto" {
		t.Fatalf("auto-routing environment = %+v", launch.Environment)
	}
	if _, present := launch.Environment["PIKA_AGENT_REASONING_EFFORT"]; present {
		t.Fatalf("auto-routing unexpectedly set reasoning effort: %+v", launch.Environment)
	}
	output, err := runCursorWrapperDirect(cursorCommandEnvironment(launch.Environment))
	if err != nil {
		t.Fatalf("run Cursor wrapper: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "--model auto") {
		t.Fatalf("auto-routing argv:\n%s", output)
	}
}

func cursorCommandEnvironment(values map[string]string) []string {
	environment := os.Environ()
	for key, value := range values {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func runPublishedCursor(t *testing.T, executable, wantTarget string, environment []string, arguments ...string) ([]byte, error) {
	t.Helper()
	if target, err := os.Readlink(executable); err != nil || target != wantTarget {
		t.Fatalf("published Cursor launcher target=%q err=%v", target, err)
	}
	command := exec.Command(executable, arguments...)
	command.Env = append(environment, "PIKA_GO_TEST_CURSOR_WRAPPER_HELPER=1")
	output, err := command.CombinedOutput()
	return output, err
}

func runCursorWrapperDirect(environment []string, arguments ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	code := provider.RunCursorWrapper(arguments, environment, nil, &stdout, &stderr)
	if code != 0 {
		return append(stdout.Bytes(), stderr.Bytes()...), fmt.Errorf("exit status %d", code)
	}
	return stdout.Bytes(), nil
}

func currentTestExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable
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
