package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/cli"
	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemon"
	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/instance"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
)

func TestInstallRejectsRelativeDirectory(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"install", "--dir", "relative"}, nil, &stdout, &stderr); code != 2 {
		t.Fatalf("install exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--dir must be an absolute path") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestMaintenanceCLIExposesPrepareStatusAndResume(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "pika.sock")
	process := &cliFakeMaintenance{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(ctx, daemon.Config{SocketPath: socketPath, Version: "test", Maintenance: process})
	}()
	waitForControlHealth(t, socketPath)

	var prepareOut, prepareErr bytes.Buffer
	if code := cli.Run(context.Background(), []string{
		"maintenance", "prepare", "--socket", socketPath, "--request-id", "prepare-1",
		"--to-digest", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "--to-version", "schema23", "--json",
	}, nil, &prepareOut, &prepareErr); code != 0 {
		t.Fatalf("prepare exit=%d out=%q err=%q", code, prepareOut.String(), prepareErr.String())
	}
	if status := process.current(); status.ToGeneration.Digest != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("prepare target = %+v", status.ToGeneration)
	}
	var statusOut bytes.Buffer
	if code := cli.Run(context.Background(), []string{"maintenance", "status", "--socket", socketPath, "--json"}, nil, &statusOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit=%d out=%q", code, statusOut.String())
	}
	var got maintenance.Status
	if err := json.Unmarshal(statusOut.Bytes(), &got); err != nil || got.RequestID != "prepare-1" {
		t.Fatalf("status=%+v err=%v", got, err)
	}
	if code := cli.Run(context.Background(), []string{
		"maintenance", "resume", "--socket", socketPath, "--request-id", "resume-1", "--json",
	}, nil, io.Discard, &bytes.Buffer{}); code != 0 {
		t.Fatalf("resume exit=%d", code)
	}
	if status := process.current(); status.State != maintenance.StateResumed || status.ResumeRequestID != "resume-1" {
		t.Fatalf("resume status = %+v", status)
	}
	cancel()
	<-done
}

func TestMaintenanceCLIRejectsAmbiguousWorkspaceAndDoesNotDiscoverOnSocketMiss(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{
		"maintenance", "status", "--socket", "/tmp/pika.sock", "--workspace", "/tmp/workspace",
	}, nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--socket and --workspace cannot be used together") {
		t.Fatalf("ambiguous flags exit=%d stderr=%q", code, stderr.String())
	}

	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	store := maintenance.NewFileStore(workspace.Root)
	if err := store.Write(maintenance.Status{
		ID: "foreign", RequestID: "foreign", State: maintenance.StateQuiescing,
		FromGeneration: maintenance.Generation{Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		ToGeneration:   maintenance.Generation{Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		StartedAt:      "now", UpdatedAt: "now",
	}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace.Root)
	missing := filepath.Join(t.TempDir(), "missing.sock")
	stderr.Reset()
	if code := cli.Run(context.Background(), []string{"maintenance", "status", "--socket", missing, "--json"}, nil, io.Discard, &stderr); code == 0 || strings.Contains(stderr.String(), "foreign") {
		t.Fatalf("explicit socket miss fell back to discovered workspace: exit=%d stderr=%q", code, stderr.String())
	}
}

type cliFakeMaintenance struct {
	mu     sync.Mutex
	status maintenance.Status
}

func (f *cliFakeMaintenance) Prepare(_ context.Context, request maintenance.PrepareRequest) (maintenance.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = maintenance.Status{
		ID: "maintenance-1", RequestID: request.RequestID, State: maintenance.StateQuiescing,
		ToGeneration: request.ToGeneration,
	}
	return f.status, nil
}

func (f *cliFakeMaintenance) Status() (maintenance.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, nil
}

func (f *cliFakeMaintenance) Resume(_ context.Context, request maintenance.ResumeRequest) (maintenance.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.State = maintenance.StateResumed
	f.status.ResumeRequestID = request.RequestID
	return f.status, nil
}

func (f *cliFakeMaintenance) Poll(context.Context) (maintenance.Status, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, false, nil
}

func (f *cliFakeMaintenance) current() maintenance.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func waitForControlHealth(t *testing.T, socketPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, err := control.Health(ctx, socketPath); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("daemon did not become healthy")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func newCommittedRepository(t *testing.T) string {
	t.Helper()
	return newCommittedRepositoryAt(t, filepath.Join(t.TempDir(), "repository"))
}

func newCommittedRepositoryAt(t *testing.T, repository string) string {
	t.Helper()
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Pika Test"},
		{"config", "user.email", "pika@example.invalid"},
	}
	for _, arguments := range commands {
		command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "README.md"}, {"commit", "--quiet", "-m", "initial"}} {
		command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

type fileSnapshot struct {
	contents []byte
	modTime  time.Time
}

type callbackReader struct {
	once   sync.Once
	before func()
	reader io.Reader
}

func (reader *callbackReader) Read(buffer []byte) (int, error) {
	reader.once.Do(reader.before)
	return reader.reader.Read(buffer)
}

func snapshotFiles(t *testing.T, paths []string) map[string]fileSnapshot {
	t.Helper()
	result := make(map[string]fileSnapshot, len(paths))
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		result[path] = fileSnapshot{contents: contents, modTime: info.ModTime()}
	}
	return result
}

func assertFilesUnchanged(t *testing.T, paths []string, before map[string]fileSnapshot) {
	t.Helper()
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(contents, before[path].contents) || !info.ModTime().Equal(before[path].modTime) {
			t.Fatalf("generation rejection changed %s", path)
		}
	}
}

func seedUnauthorizedMaintenanceWorkspace(t *testing.T, workspace optimizationworkspace.Workspace) []string {
	t.Helper()
	if err := os.WriteFile(workspace.DatabasePath, []byte("database-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteHerdrBinding(optimizationworkspace.HerdrBinding{
		SocketPath: "/tmp/herdr-before.sock", WorkspaceID: "before", TabID: "tab-before",
		ControlPane: "control-before", DaemonPane: "daemon-before", DaemonSocket: "/tmp/pika-before.sock",
		UpdatedAt: "2026-09-01T13:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	layoutPath := filepath.Join(workspace.HerdrRoot, "layout.json")
	if err := os.WriteFile(layoutPath, []byte(`{"layout":"before"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := daemonupdate.RestoreCurrent(workspace.Root, daemonupdate.Generation{
		Path:    "/retained/from/pika-go",
		Digest:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Version: "from", ActivatedAt: "2026-09-01T13:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.NewFileStore(workspace.Root).Write(maintenance.Status{
		ID: "maintenance-unknown", RequestID: "prepare-unknown", State: maintenance.StateHolding,
		FromGeneration: maintenance.Generation{Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		ToGeneration:   maintenance.Generation{Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		HoldingGeneration: maintenance.Generation{
			Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		},
		Targets:   []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
		StartedAt: "2026-09-01T13:00:00Z", ReadyAt: "2026-09-01T13:01:00Z",
		HoldingAt: "2026-09-01T13:02:00Z", UpdatedAt: "2026-09-01T13:02:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(workspace.ContextsRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace.EvidenceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{
		workspace.DatabasePath,
		filepath.Join(workspace.RuntimeRoot, "daemon", "current.json"),
		workspace.HerdrBindingPath,
		layoutPath,
	}
}

func TestHelpExplainsThePrimaryWorkflow(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"--help"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{
		"Pika-Go orchestrates long-running coding-agent optimization in Herdr.",
		"Get started:",
		"pika-go install",
		"pika-go kick-off",
		"Workflow commands:",
		"pika-go COMMAND --help",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help output is missing %q:\n%s", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("help stderr = %q", stderr.String())
	}
}

func TestCommandHelpShowsSynopsisOptionsAndExamples(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"help", "kick-off"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("command help exit = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{
		"Usage:\n  pika-go kick-off [options]",
		"Create or resume an Optimization Workspace in Herdr",
		"--repository",
		"--timeout",
		"Examples:",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("command help is missing %q:\n%s", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("command help stderr = %q", stderr.String())
	}
}

func TestKickOffRequiresHerdrBeforeCreatingAnything(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "")
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"kick-off", "--repository", t.TempDir()}, nil, &stdout, &stderr); code != 2 {
		t.Fatalf("kick-off exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no Herdr session detected") || !strings.Contains(stderr.String(), "Start Herdr") {
		t.Fatalf("kick-off stderr = %q", stderr.String())
	}
}

func TestKickOffDoesNotCreateWorkspaceWhenHerdrConfigurationIsDeclined(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-kick-off-config-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	herdrSocket := filepath.Join(socketDir, "herdr.sock")
	listener, err := net.Listen("unix", herdrSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer connection.Close()
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
			done <- decodeErr
			return
		}
		if request.Method != "session.snapshot" {
			done <- fmt.Errorf("method = %s", request.Method)
			return
		}
		done <- json.NewEncoder(connection).Encode(map[string]any{
			"id": request.ID, "result": map[string]any{"snapshot": map[string]any{"version": "0.8.2", "protocol": 20}},
		})
	}()
	configPath := filepath.Join(socketDir, "config.toml")
	original := "[session]\n# resume_agents_on_restore = true\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_CONFIG_PATH", configPath)
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"kick-off", "--repository", t.TempDir()}, strings.NewReader("n\n"), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("kick-off exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "May Pika-Go update it and reload Herdr now? [y/N]") || !strings.Contains(stderr.String(), "resume_agents_on_restore = false") {
		t.Fatalf("kick-off stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	contents, err := os.ReadFile(configPath)
	if err != nil || string(contents) != original {
		t.Fatalf("declined config contents=%q err=%v", contents, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestKickOffHoldsMutationLeaseAcrossHerdrConfigurationPrompt(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-kick-off-reauthorize-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	herdrSocket := filepath.Join(socketDir, "herdr.sock")
	listener, err := net.Listen("unix", herdrSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	methods := make(chan string, 4)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.NewDecoder(connection).Decode(&request) == nil {
				methods <- request.Method
				_ = json.NewEncoder(connection).Encode(map[string]any{
					"id": request.ID, "result": map[string]any{"snapshot": map[string]any{"version": "0.8.2", "protocol": 20}},
				})
			}
			_ = connection.Close()
		}
	}()

	repository := newCommittedRepository(t)
	workspaceRoot, err := optimizationworkspace.DefaultRoot(repository)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := optimizationworkspace.Create(context.Background(), workspaceRoot, repository)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(socketDir, "config.toml")
	originalConfig := "[session]\nresume_agents_on_restore = true\n"
	if err := os.WriteFile(configPath, []byte(originalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_CONFIG_PATH", configPath)
	activationEntered := make(chan *optimizationworkspace.MutationLease, 1)
	input := &callbackReader{
		reader: strings.NewReader("n\n"),
		before: func() {
			go func() {
				lease, leaseErr := optimizationworkspace.AcquireExclusiveMutationLease(context.Background(), workspace.Root)
				if leaseErr == nil {
					activationEntered <- lease
				}
			}()
			select {
			case lease := <-activationEntered:
				_ = lease.Close()
				t.Fatal("maintenance activation entered during Herdr configuration prompt")
			case <-time.After(100 * time.Millisecond):
			}
		},
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"kick-off", "--repository", repository}, input, &stdout, &stderr); code != 2 {
		t.Fatalf("kick-off exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	select {
	case lease := <-activationEntered:
		defer lease.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance activation did not enter after kickoff released its mutation lease")
	}
	config, err := os.ReadFile(configPath)
	if err != nil || string(config) != originalConfig {
		t.Fatalf("Herdr configuration changed: contents=%q err=%v", config, err)
	}
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	<-serverDone
	close(methods)
	var called []string
	for method := range methods {
		called = append(called, method)
	}
	if !slices.Equal(called, []string{"session.snapshot"}) {
		t.Fatalf("Herdr mutations before authorization: %v", called)
	}
}

func TestWebUIRejectsUnauthorizedGenerationBeforeTokenOrPIDWrites(t *testing.T) {
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	protectedPaths := seedUnauthorizedMaintenanceWorkspace(t, workspace)
	before := snapshotFiles(t, protectedPaths)
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"webui", "--workspace", workspace.Root, "--listen", "127.0.0.1:0"}, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("webui exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "maintenance state rejected") {
		t.Fatalf("webui rejection=%q", stderr.String())
	}
	assertFilesUnchanged(t, protectedPaths, before)
	if _, err := os.Stat(filepath.Join(workspace.RuntimeRoot, "webui")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unauthorized WebUI created runtime state: %v", err)
	}
}

func TestWebUIHoldingTargetReusesTokenAndOnlyOwnsPID(t *testing.T) {
	ctx := context.Background()
	workspace, err := optimizationworkspace.Create(ctx, filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "pika-webui-holding-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	daemonSocket := filepath.Join(socketDir, "pika.sock")
	daemonCtx, stopDaemon := context.WithCancel(ctx)
	daemonDone := make(chan error, 1)
	go func() {
		daemonDone <- daemon.Serve(daemonCtx, daemon.Config{SocketPath: daemonSocket, Version: "test"})
	}()
	waitForHealth(t, daemonSocket, nil)
	t.Cleanup(func() {
		stopDaemon()
		<-daemonDone
	})
	binding := optimizationworkspace.HerdrBinding{
		SocketPath: "/tmp/herdr.sock", WorkspaceID: "workspace", TabID: "tab",
		ControlPane: "control", DaemonPane: "daemon", DaemonSocket: daemonSocket,
		UpdatedAt: "2026-09-01T12:00:00Z",
	}
	if err := workspace.WriteHerdrBinding(binding); err != nil {
		t.Fatal(err)
	}
	tokenDirectory := filepath.Join(workspace.RuntimeRoot, "webui")
	if err := os.MkdirAll(tokenDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(tokenDirectory, "token")
	tokenBytes := []byte("holding-token\n")
	if err := os.WriteFile(tokenPath, tokenBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	tokenInfo, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := daemonupdate.FileDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	generation := maintenance.Generation{Digest: digest, Version: "target"}
	if err := maintenance.NewFileStore(workspace.Root).Write(maintenance.Status{
		ID: "maintenance", RequestID: "prepare", State: maintenance.StateHolding,
		FromGeneration: maintenance.Generation{Digest: strings.Repeat("a", 64), Version: "bridge"},
		ToGeneration:   generation, HoldingGeneration: generation,
		Targets:   []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
		StartedAt: "2026-09-01T12:00:00Z", ReadyAt: "2026-09-01T12:01:00Z",
		HoldingAt: "2026-09-01T12:02:00Z", UpdatedAt: "2026-09-01T12:02:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	var rotateOut, rotateErr bytes.Buffer
	if code := cli.Run(ctx, []string{"webui", "--workspace", workspace.Root, "--listen", "127.0.0.1:0", "--rotate-token"}, nil, &rotateOut, &rotateErr); code != 1 ||
		!strings.Contains(rotateErr.String(), "--rotate-token is forbidden") {
		t.Fatalf("holding rotate exit=%d stdout=%q stderr=%q", code, rotateOut.String(), rotateErr.String())
	}
	webuiCtx, stopWebUI := context.WithCancel(ctx)
	webuiDone := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		webuiDone <- cli.Run(webuiCtx, []string{"webui", "--workspace", workspace.Root, "--listen", "127.0.0.1:0"}, nil, &stdout, &stderr)
	}()
	pidPath := filepath.Join(tokenDirectory, "pid")
	waitForPath(t, pidPath)
	stopWebUI()
	if code := <-webuiDone; code != 0 {
		t.Fatalf("holding WebUI exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(tokenPath)
	afterInfo, statErr := os.Stat(tokenPath)
	if err != nil || statErr != nil || !bytes.Equal(after, tokenBytes) || !afterInfo.ModTime().Equal(tokenInfo.ModTime()) {
		t.Fatalf("holding token changed: contents=%q read=%v stat=%v before=%v after=%v", after, err, statErr, tokenInfo.ModTime(), afterInfo.ModTime())
	}
	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("holding WebUI PID remains: %v", err)
	}
}

func TestWebUIWaitsForMaintenanceActivationAndRejectsBeforeWrites(t *testing.T) {
	ctx := context.Background()
	workspace, err := optimizationworkspace.Create(ctx, filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	tokenDirectory := filepath.Join(workspace.RuntimeRoot, "webui")
	if err := os.MkdirAll(tokenDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(tokenDirectory, "token")
	if err := os.WriteFile(tokenPath, []byte("stable-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	protected := []string{tokenPath}
	before := snapshotFiles(t, protected)
	exclusive, err := optimizationworkspace.AcquireExclusiveMutationLease(ctx, workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"webui", "--workspace", workspace.Root, "--listen", "127.0.0.1:0"}, nil, &stdout, &stderr)
	}()
	select {
	case code := <-done:
		t.Fatalf("WebUI crossed exclusive maintenance lease: exit=%d stderr=%q", code, stderr.String())
	case <-time.After(100 * time.Millisecond):
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := daemonupdate.FileDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenance.NewFileStore(workspace.Root).Write(maintenance.Status{
		ID: "maintenance", RequestID: "prepare", State: maintenance.StateReady,
		FromGeneration: maintenance.Generation{Digest: strings.Repeat("a", 64)},
		ToGeneration:   maintenance.Generation{Digest: digest},
		Targets:        []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
		StartedAt:      "2026-09-01T12:00:00Z", ReadyAt: "2026-09-01T12:01:00Z", UpdatedAt: "2026-09-01T12:01:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 1 || !strings.Contains(stderr.String(), "maintenance state rejected") {
		t.Fatalf("WebUI race exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertFilesUnchanged(t, protected, before)
	for _, path := range []string{filepath.Join(tokenDirectory, "pid"), workspace.HerdrBindingPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("WebUI race created %s: %v", path, err)
		}
	}
}

func TestUpdateRejectsUnauthorizedGenerationBeforeStagingCandidate(t *testing.T) {
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	protectedPaths := seedUnauthorizedMaintenanceWorkspace(t, workspace)
	before := snapshotFiles(t, protectedPaths)
	generationsRoot := filepath.Join(workspace.RuntimeRoot, "daemon", "generations")
	if _, err := os.Stat(generationsRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected generations directory before update: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"update", "--workspace", workspace.Root, "--binary", executable}, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("update exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "maintenance generation rejected") {
		t.Fatalf("update rejection=%q", stderr.String())
	}
	assertFilesUnchanged(t, protectedPaths, before)
	if _, err := os.Stat(generationsRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unauthorized update staged a generation: %v", err)
	}
}

func TestUpdateRejectsActiveMaintenanceForAuthorizedGenerationWithoutWrites(t *testing.T) {
	for _, state := range []maintenance.State{maintenance.StateQuiescing, maintenance.StateReady, maintenance.StateHolding} {
		t.Run(string(state), func(t *testing.T) {
			workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			digest, err := daemonupdate.FileDigest(executable)
			if err != nil {
				t.Fatal(err)
			}
			if err := daemonupdate.RestoreCurrent(workspace.Root, daemonupdate.Generation{
				Path: executable, Digest: digest, Version: "from", ActivatedAt: "2026-09-01T13:00:00Z",
			}); err != nil {
				t.Fatal(err)
			}
			if err := daemonupdate.WriteStatus(workspace.Root, daemonupdate.Status{
				ID: "prior-update", State: daemonupdate.StateCommitted,
				From:      daemonupdate.Generation{Path: executable, Digest: digest, Version: "from"},
				To:        daemonupdate.Generation{Path: executable, Digest: strings.Repeat("d", 64), Version: "prior"},
				StartedAt: "2026-09-01T12:00:00Z",
			}); err != nil {
				t.Fatal(err)
			}
			maintenanceStatus := maintenance.Status{
				ID: "maintenance", RequestID: "prepare", State: state,
				FromGeneration: maintenance.Generation{Digest: digest, Version: "from"},
				ToGeneration:   maintenance.Generation{Digest: strings.Repeat("b", 64), Version: "to"},
				Targets:        []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
				StartedAt:      "2026-09-01T13:00:00Z",
				UpdatedAt:      "2026-09-01T13:00:00Z",
			}
			if state == maintenance.StateReady || state == maintenance.StateHolding {
				maintenanceStatus.ReadyAt = "2026-09-01T13:01:00Z"
				maintenanceStatus.UpdatedAt = maintenanceStatus.ReadyAt
			}
			if state == maintenance.StateHolding {
				maintenanceStatus.HoldingGeneration = maintenanceStatus.ToGeneration
				maintenanceStatus.HoldingAt = "2026-09-01T13:02:00Z"
				maintenanceStatus.UpdatedAt = maintenanceStatus.HoldingAt
			}
			if err := maintenance.NewFileStore(workspace.Root).Write(maintenanceStatus); err != nil {
				t.Fatal(err)
			}
			generationsRoot := filepath.Join(workspace.RuntimeRoot, "daemon", "generations")
			if err := os.MkdirAll(generationsRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(generationsRoot, "retained-generation")
			if err := os.WriteFile(sentinel, []byte("retained\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			paths := []string{
				filepath.Join(workspace.RuntimeRoot, "daemon", "current.json"),
				daemonupdate.StatusPath(workspace.Root),
				filepath.Join(workspace.RuntimeRoot, "daemon", "maintenance.json"),
				sentinel,
			}
			before := snapshotFiles(t, paths)
			beforeEntries, err := os.ReadDir(generationsRoot)
			if err != nil {
				t.Fatal(err)
			}
			beforeNames := make([]string, len(beforeEntries))
			for index, entry := range beforeEntries {
				beforeNames[index] = entry.Name()
			}

			var stdout, stderr bytes.Buffer
			code := cli.Run(context.Background(), []string{
				"update", "--workspace", workspace.Root, "--binary", executable,
			}, nil, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), "hot update is forbidden during maintenance state "+string(state)) {
				t.Fatalf("update exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			assertFilesUnchanged(t, paths, before)
			afterEntries, err := os.ReadDir(generationsRoot)
			if err != nil {
				t.Fatal(err)
			}
			afterNames := make([]string, len(afterEntries))
			for index, entry := range afterEntries {
				afterNames[index] = entry.Name()
			}
			if !slices.Equal(beforeNames, afterNames) {
				t.Fatalf("staging entries changed: before=%v after=%v", beforeNames, afterNames)
			}
		})
	}
}

func TestUpdateWaitsForConcurrentMaintenanceActivationAndRechecksBeforeStage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	workspace, err := optimizationworkspace.Create(ctx, filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := daemonupdate.FileDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemonupdate.RestoreCurrent(workspace.Root, daemonupdate.Generation{
		Path: executable, Digest: digest, Version: "from", ActivatedAt: "2026-09-01T13:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	activation, err := optimizationworkspace.AcquireExclusiveMutationLease(ctx, workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"update", "--workspace", workspace.Root, "--binary", executable}, nil, &stdout, &stderr)
	}()
	select {
	case code := <-done:
		t.Fatalf("update crossed maintenance activation lease: exit=%d stderr=%q", code, stderr.String())
	case <-time.After(100 * time.Millisecond):
	}
	generationsRoot := filepath.Join(workspace.RuntimeRoot, "daemon", "generations")
	if _, err := os.Stat(generationsRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked update staged before maintenance activation: %v", err)
	}
	if err := maintenance.NewFileStore(workspace.Root).Write(maintenance.Status{
		ID: "maintenance", RequestID: "prepare", State: maintenance.StateQuiescing,
		FromGeneration: maintenance.Generation{Digest: digest, Version: "from"},
		ToGeneration:   maintenance.Generation{Digest: strings.Repeat("b", 64), Version: "to"},
		Targets:        []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
		StartedAt:      "2026-09-01T13:00:00Z", UpdatedAt: "2026-09-01T13:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := activation.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), "hot update is forbidden during maintenance state quiescing") {
			t.Fatalf("update exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-ctx.Done():
		t.Fatalf("update did not resume after maintenance activation: %v", ctx.Err())
	}
	if _, err := os.Stat(generationsRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected update staged after maintenance activation: %v", err)
	}
}

func TestEditInstructionRejectsUnauthorizedGenerationBeforeEditorMutation(t *testing.T) {
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	protectedPaths := seedUnauthorizedMaintenanceWorkspace(t, workspace)
	instructionPath := filepath.Join(workspace.InstructionsRoot, "baseline.md")
	if err := os.MkdirAll(filepath.Dir(instructionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructionPath, []byte("original instruction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectedPaths = append(protectedPaths, instructionPath)
	before := snapshotFiles(t, protectedPaths)
	editor := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf 'edited\\n' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", editor)
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"edit-instruction", "--workspace", workspace.Root, "baseline"}, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("edit-instruction exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "direct Workspace mutation rejected") {
		t.Fatalf("edit-instruction rejection=%q", stderr.String())
	}
	assertFilesUnchanged(t, protectedPaths, before)
}

func TestKickOffCreatesWorkspaceStartsDaemonAndInitializesRootPane(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-kick-off-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	herdrSocket := filepath.Join(socketDir, "herdr.sock")
	herdrListener, err := net.Listen("unix", herdrSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = herdrListener.Close() })

	type herdrRequest struct {
		Method string
		Params map[string]any
	}
	herdrRequests := make(chan herdrRequest, 16)
	herdrDone := make(chan error, 1)
	go func() {
		for {
			connection, acceptErr := herdrListener.Accept()
			if acceptErr != nil {
				if errors.Is(acceptErr, net.ErrClosed) {
					herdrDone <- nil
				} else {
					herdrDone <- acceptErr
				}
				return
			}
			var request struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
				_ = connection.Close()
				herdrDone <- decodeErr
				return
			}
			herdrRequests <- herdrRequest{Method: request.Method, Params: request.Params}
			var result any
			switch request.Method {
			case "session.snapshot":
				result = map[string]any{"snapshot": map[string]any{"version": "0.8.2", "protocol": 20, "workspaces": []map[string]any{{"workspace_id": "w-new"}}}}
			case "server.reload_config":
				result = map[string]any{"status": "applied", "diagnostics": []string{}}
			case "workspace.create":
				result = map[string]any{
					"workspace": map[string]any{"workspace_id": "w-new"},
					"root_pane": map[string]any{"pane_id": "w-new:p1", "workspace_id": "w-new", "tab_id": "w-new:t1", "terminal_id": "term-root", "agent_status": "idle", "revision": 1},
				}
			case "plugin.pane.open":
				result = map[string]any{"plugin_pane": map[string]any{
					"plugin_id": "pika-go", "entrypoint": "symphony",
					"pane": map[string]any{"pane_id": "w-new:p2", "workspace_id": "w-new", "tab_id": "w-new:t1", "terminal_id": "term-daemon", "agent_status": "working", "revision": 1},
				}}
			case "tab.rename":
				result = map[string]any{"tab": map[string]any{"tab_id": "w-new:t1", "workspace_id": "w-new", "label": request.Params["label"]}}
			case "pane.rename":
				result = map[string]any{"pane": map[string]any{"pane_id": request.Params["pane_id"], "workspace_id": "w-new", "tab_id": "w-new:t1", "label": request.Params["label"]}}
			case "workspace.focus":
				result = map[string]any{"workspace": map[string]any{"workspace_id": "w-new"}}
			case "tab.focus":
				result = map[string]any{"tab": map[string]any{"tab_id": "w-new:t1", "workspace_id": "w-new"}}
			case "pane.focus":
				result = map[string]any{"pane": map[string]any{"pane_id": "w-new:p1", "workspace_id": "w-new", "tab_id": "w-new:t1"}}
			case "pane.send_input":
				result = map[string]any{"pane": map[string]any{"pane_id": "w-new:p1", "workspace_id": "w-new", "tab_id": "w-new:t1"}}
			default:
				_ = json.NewEncoder(connection).Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": "unexpected_method", "message": request.Method}})
				_ = connection.Close()
				continue
			}
			_ = json.NewEncoder(connection).Encode(map[string]any{"id": request.ID, "result": result})
			_ = connection.Close()
		}
	}()

	pikaSocket, err := instance.SocketForHerdrWorkspace(herdrSocket, "w-new")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pikaSocket), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(pikaSocket)
	pikaListener, err := net.Listen("unix", pikaSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pikaListener.Close()
		_ = os.Remove(pikaSocket)
	})
	initRequests := make(chan protocol.InitRequest, 1)
	pikaMux := http.NewServeMux()
	pikaMux.HandleFunc("GET /v1/health", func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(protocol.Health{Status: "ok", Version: "test", ProtocolVersion: protocol.Version})
	})
	pikaMux.HandleFunc("GET /v1/init/options", func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(protocol.InitOptionsResponse{ConfigurationExists: true})
	})
	pikaMux.HandleFunc("POST /v1/init", func(response http.ResponseWriter, request *http.Request) {
		var input protocol.InitRequest
		if decodeErr := json.NewDecoder(request.Body).Decode(&input); decodeErr != nil {
			http.Error(response, decodeErr.Error(), http.StatusBadRequest)
			return
		}
		initRequests <- input
		_ = json.NewEncoder(response).Encode(symphony.Receipt{ID: "receipt-1", RequestID: input.RequestID, Command: "init", Revision: 1})
	})
	pikaServer := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set(protocol.VersionHeader, fmt.Sprint(protocol.Version))
		pikaMux.ServeHTTP(response, request)
	})}
	pikaDone := make(chan error, 1)
	go func() { pikaDone <- pikaServer.Serve(pikaListener) }()
	t.Cleanup(func() { _ = pikaServer.Close() })

	repository := newCommittedRepository(t)
	resolvedRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_PANE_ID", "w-origin:p1")
	herdrConfig := filepath.Join(socketDir, "config.toml")
	if err := os.WriteFile(herdrConfig, []byte("[session]\n# resume_agents_on_restore = true\n\n[remote]\nconnect_timeout_seconds = 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_CONFIG_PATH", herdrConfig)
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"kick-off", "--repository", repository}, strings.NewReader("yes\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("kick-off exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("kick-off stderr = %q", stderr.String())
	}
	for _, want := range []string{"May Pika-Go update it and reload Herdr now? [y/N]", "Updated Herdr configuration and reloaded the server.", "Creating Herdr workspace", "Waiting for the daemon", "Initialization command started", "Workspace:   w-new", "Init pane:   w-new:p1"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("kick-off stdout is missing %q:\n%s", want, stdout.String())
		}
	}

	requests := make(map[string]map[string]any)
	var paneRenames []map[string]any
	var requestOrder []string
	for range 11 {
		request := <-herdrRequests
		requestOrder = append(requestOrder, request.Method)
		requests[request.Method] = request.Params
		if request.Method == "pane.rename" {
			paneRenames = append(paneRenames, request.Params)
		}
	}
	if len(requestOrder) < 3 || requestOrder[0] != "session.snapshot" || requestOrder[1] != "server.reload_config" || requestOrder[2] != "workspace.create" {
		t.Fatalf("Herdr request order = %v", requestOrder)
	}
	configured, err := os.ReadFile(herdrConfig)
	if err != nil || !strings.Contains(string(configured), "[session]\n# resume_agents_on_restore = true\nresume_agents_on_restore = false\n\n[remote]") {
		t.Fatalf("configured Herdr file=%q err=%v", configured, err)
	}
	workspaceRoot, err := optimizationworkspace.DefaultRoot(resolvedRepository)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err = filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if requests["workspace.create"]["cwd"] != workspaceRoot || requests["workspace.create"]["focus"] != false {
		t.Fatalf("workspace.create params = %+v", requests["workspace.create"])
	}
	if requests["tab.rename"]["tab_id"] != "w-new:t1" || requests["tab.rename"]["label"] != "Pika" {
		t.Fatalf("tab.rename params = %+v", requests["tab.rename"])
	}
	if len(paneRenames) != 2 || paneRenames[0]["pane_id"] != "w-new:p1" || paneRenames[0]["label"] != "Control" ||
		paneRenames[1]["pane_id"] != "w-new:p2" || paneRenames[1]["label"] != "Daemon" {
		t.Fatalf("pane.rename params = %+v", paneRenames)
	}
	pluginParams := requests["plugin.pane.open"]
	if pluginParams["plugin_id"] != "pika-go" || pluginParams["entrypoint"] != "symphony" || pluginParams["target_pane_id"] != "w-new:p1" || pluginParams["focus"] != false {
		t.Fatalf("plugin.pane.open params = %+v", pluginParams)
	}
	if pluginParams["cwd"] != workspaceRoot {
		t.Fatalf("plugin.pane.open cwd = %v, want %s", pluginParams["cwd"], workspaceRoot)
	}
	environment, ok := pluginParams["env"].(map[string]any)
	if !ok || environment["PIKA_GO_WORKSPACE"] != workspaceRoot {
		t.Fatalf("plugin.pane.open env = %#v", pluginParams["env"])
	}
	integrationExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	integrationExecutable, err = filepath.Abs(integrationExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if environment["PIKA_GO_INTEGRATION_EXECUTABLE"] != integrationExecutable {
		t.Fatalf("plugin.pane.open integration executable = %v, want %s", environment["PIKA_GO_INTEGRATION_EXECUTABLE"], integrationExecutable)
	}
	if requests["workspace.focus"]["workspace_id"] != "w-new" {
		t.Fatalf("workspace.focus params = %+v", requests["workspace.focus"])
	}
	if requests["tab.focus"]["tab_id"] != "w-new:t1" {
		t.Fatalf("tab.focus params = %+v", requests["tab.focus"])
	}
	if requests["pane.focus"]["pane_id"] != "w-new:p1" {
		t.Fatalf("pane.focus params = %+v", requests["pane.focus"])
	}
	initPaneParams := requests["pane.send_input"]
	if initPaneParams["pane_id"] != "w-new:p1" || !strings.Contains(fmt.Sprint(initPaneParams["text"]), " init ") ||
		!strings.Contains(fmt.Sprint(initPaneParams["text"]), resolvedRepository) {
		t.Fatalf("pane.send_input params = %+v", initPaneParams)
	}
	keys, ok := initPaneParams["keys"].([]any)
	if !ok || len(keys) != 1 || keys[0] != "enter" {
		t.Fatalf("pane.send_input keys = %#v", initPaneParams["keys"])
	}
	select {
	case initialized := <-initRequests:
		t.Fatalf("kick-off initialized from its calling process instead of the new pane: %+v", initialized)
	default:
	}
	workspace, err := optimizationworkspace.Open(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := workspace.ReadHerdrBinding()
	if err != nil {
		t.Fatal(err)
	}
	if binding.WorkspaceID != "w-new" || binding.ControlPane != "w-new:p1" || binding.DaemonPane != "w-new:p2" || binding.DaemonSocket != pikaSocket {
		t.Fatalf("Herdr binding = %+v", binding)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cli.Run(context.Background(), []string{"open", workspace.Root}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("open exit = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "already running in Herdr workspace w-new") {
		t.Fatalf("open stdout = %q", stdout.String())
	}
	var resumedMethods []string
	for range 5 {
		request := <-herdrRequests
		resumedMethods = append(resumedMethods, request.Method)
	}
	wantResumedMethods := []string{"session.snapshot", "server.reload_config", "workspace.focus", "tab.focus", "pane.focus"}
	if !slices.Equal(resumedMethods, wantResumedMethods) {
		t.Fatalf("resume methods = %v, want %v", resumedMethods, wantResumedMethods)
	}
}

func TestStatusReportsHealthyDaemon(t *testing.T) {
	t.Parallel()

	socketDir, err := os.MkdirTemp("/tmp", "pika-go-test-")
	if err != nil {
		t.Fatalf("create short socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika-go.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var daemonStdout bytes.Buffer
	var daemonStderr bytes.Buffer
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- cli.Run(ctx, []string{"daemon", "--socket", socketPath}, nil, &daemonStdout, &daemonStderr)
	}()

	waitForStatus(t, socketPath, &daemonStderr)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"status", "--socket", socketPath, "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit code = %d, stderr = %q", code, stderr.String())
	}

	var got struct {
		Status          string `json:"status"`
		Version         string `json:"version"`
		ProtocolVersion int    `json:"protocol_version"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode status JSON: %v; output = %q", err, stdout.String())
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok", got.Status)
	}
	if got.Version == "" {
		t.Fatal("version is empty")
	}
	if got.ProtocolVersion != 2 {
		t.Fatalf("protocol_version = %d, want 2", got.ProtocolVersion)
	}

	cancel()
	select {
	case code := <-daemonDone:
		if code != 0 {
			t.Fatalf("daemon exit code = %d, want 0", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop after cancellation")
	}
}

func TestDaemonErrorsAreStructuredJSON(t *testing.T) {
	var stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"daemon", "--socket", "relative.sock"}, nil, &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("daemon exit = %d, stderr=%q", code, stderr.String())
	}
	var record map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &record); err != nil {
		t.Fatalf("daemon log is not JSON: %v; log=%q", err, stderr.String())
	}
	if record["level"] != "error" || record["event"] != "runtime.resolve_failed" || record["error"] == "" || record["time"] == "" {
		t.Fatalf("daemon log = %+v", record)
	}
}

func TestDaemonUsesOptimizationWorkspaceAsDurableRoot(t *testing.T) {
	isolateHerdrEnvironment(t)
	repository := newCommittedRepository(t)
	workspaceRoot := filepath.Join(t.TempDir(), "optimization-workspace")
	workspace, err := optimizationworkspace.Create(context.Background(), workspaceRoot, repository)
	if err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-workspace-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	ctx, cancel := context.WithCancel(context.Background())
	var daemonStderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"daemon", "--socket", socketPath, "--workspace", workspace.Root}, nil, io.Discard, &daemonStderr)
	}()
	waitForHealth(t, socketPath, &daemonStderr)
	if _, err := os.Stat(workspace.DatabasePath); err != nil {
		t.Fatalf("Workspace database: %v", err)
	}
	if _, err := os.Stat(workspace.LockPath); err != nil {
		t.Fatalf("Workspace lock: %v", err)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("daemon exit = %d, stderr = %s", code, daemonStderr.String())
	}
	engine, err := symphony.Open(context.Background(), workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	records, err := engine.GitWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Role != "base" || records[0].Repository != workspace.BaseRepository || records[0].Branch != workspace.BaseBranch() {
		t.Fatalf("Git worktree registry = %+v", records)
	}
}

func TestWorkspaceDaemonRejectsReadyMaintenanceWithoutHerdrAdmission(t *testing.T) {
	isolateHerdrEnvironment(t)
	repository := newCommittedRepository(t)
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	engine, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureWorkspaceIdentity(ctx, symphony.WorkspaceIdentity{
		ID: workspace.Identity.ID, Root: workspace.Identity.Root, SourceRepository: workspace.Identity.SourceRepository,
		GitCommonDir: workspace.Identity.GitCommonDir, GitCommonDirDevice: workspace.Identity.GitCommonDirDevice,
		GitCommonDirInode: workspace.Identity.GitCommonDirInode, InitialSHA: workspace.Identity.InitialSHA,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.UpsertGitWorktree(ctx, symphony.GitWorktreeRecord{
		Role: "base", Branch: workspace.BaseBranch(), Repository: workspace.BaseRepository,
		HeadSHA: workspace.Identity.InitialSHA, State: "active",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: workspace.Identity.ID,
		Repository: workspace.Identity.SourceRepository, FlowVersion: symphony.FlowVersion1,
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	session := symphony.AgentSession{
		ID: "session-hold", WorkID: view.Works[0].ID, Generation: view.Works[0].Generation,
		Role: view.Works[0].Role, AgentKind: "codex", AgentName: "pika-hold", Status: symphony.AgentSessionRunning,
	}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "terminal-before-hold"}, WorkID: session.WorkID,
		Definition: testcontract.Definition(),
	}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	pendingBefore := view.PendingEffectCount
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := daemonupdate.FileDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	fromDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := maintenance.NewFileStore(workspace.Root).Write(maintenance.Status{
		ID: "maintenance-hold", RequestID: "prepare-1", State: maintenance.StateReady,
		FromGeneration: maintenance.Generation{Digest: fromDigest, Version: "bridge"},
		ToGeneration:   maintenance.Generation{Digest: digest, Version: cli.Version},
		Targets: []maintenance.Target{{
			WorkID: session.WorkID, SessionID: session.ID, WorkGeneration: session.Generation,
			AgentName: session.AgentName, AgentKind: session.AgentKind,
		}},
		StartedAt: "2026-09-01T11:00:00Z", ReadyAt: "2026-09-01T11:01:00Z", UpdatedAt: "2026-09-01T11:01:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	socketDir, err := os.MkdirTemp("/tmp", "pika-go-hold-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	daemonCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var daemonStderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(daemonCtx, []string{"daemon", "--socket", socketPath, "--workspace", workspace.Root}, nil, io.Discard, &daemonStderr)
	}()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(daemonStderr.String(), "maintenance.herdr_admission_failed") {
			t.Fatalf("daemon exit=%d stderr=%s", code, daemonStderr.String())
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("daemon bypassed missing Herdr admission")
	}
	preserved, err := maintenance.NewFileStore(workspace.Root).Read()
	if err != nil || preserved.State != maintenance.StateReady {
		t.Fatalf("rejected daemon changed maintenance state: status=%+v err=%v", preserved, err)
	}
	reopened, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	preservedView, err := reopened.Inspect(ctx, symphony.Status{})
	_ = reopened.Close()
	if err != nil || preservedView.PendingEffectCount != pendingBefore {
		t.Fatalf("rejected daemon changed durable state: pending=%d want=%d err=%v", preservedView.PendingEffectCount, pendingBefore, err)
	}
}

func TestMaintenanceGenerationRejectsDaemonAndOpenBeforeWorkspaceWrites(t *testing.T) {
	isolateHerdrEnvironment(t)
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	from := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	to := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	holder := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if err := os.WriteFile(workspace.DatabasePath, []byte("schema20-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteHerdrBinding(optimizationworkspace.HerdrBinding{
		SocketPath: "/tmp/herdr-before.sock", WorkspaceID: "before", TabID: "tab-before",
		ControlPane: "control-before", DaemonPane: "daemon-before", DaemonSocket: "/tmp/pika-before.sock",
		UpdatedAt: "2026-09-01T11:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemonupdate.RestoreCurrent(workspace.Root, daemonupdate.Generation{
		Path: "/retained/from/pika-go", Digest: from, Version: "from", ActivatedAt: "2026-09-01T11:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.NewFileStore(workspace.Root).Write(maintenance.Status{
		ID: "maintenance-1", RequestID: "prepare-1", State: maintenance.StateHolding,
		FromGeneration: maintenance.Generation{Digest: from},
		ToGeneration:   maintenance.Generation{Digest: to},
		HoldingGeneration: maintenance.Generation{
			Digest: holder,
		},
		Targets:   []maintenance.Target{{WorkID: "work-1", SessionID: "session-1", WorkGeneration: 1}},
		StartedAt: "2026-09-01T11:00:00Z", ReadyAt: "2026-09-01T11:01:00Z",
		HoldingAt: "2026-09-01T11:02:00Z", UpdatedAt: "2026-09-01T11:02:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(workspace.ContextsRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace.EvidenceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	evidenceBefore, err := os.Stat(workspace.EvidenceRoot)
	if err != nil {
		t.Fatal(err)
	}
	protected := []string{
		workspace.DatabasePath,
		filepath.Join(workspace.RuntimeRoot, "daemon", "current.json"),
		workspace.HerdrBindingPath,
	}
	type snapshot struct {
		contents []byte
		modTime  time.Time
	}
	before := make(map[string]snapshot, len(protected))
	for _, path := range protected {
		contents, readErr := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if readErr != nil || statErr != nil {
			t.Fatalf("snapshot %s: read=%v stat=%v", path, readErr, statErr)
		}
		before[path] = snapshot{contents: contents, modTime: info.ModTime()}
	}
	assertUnchanged := func() {
		t.Helper()
		for _, path := range protected {
			contents, readErr := os.ReadFile(path)
			info, statErr := os.Stat(path)
			if readErr != nil || statErr != nil {
				t.Fatalf("re-read %s: read=%v stat=%v", path, readErr, statErr)
			}
			if !bytes.Equal(contents, before[path].contents) || !info.ModTime().Equal(before[path].modTime) {
				t.Fatalf("generation rejection changed %s", path)
			}
		}
		if _, err := os.Stat(workspace.ContextsRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("generation rejection created missing layout: %v", err)
		}
		evidenceAfter, err := os.Stat(workspace.EvidenceRoot)
		if err != nil {
			t.Fatal(err)
		}
		if evidenceAfter.Mode() != evidenceBefore.Mode() {
			t.Fatalf("generation rejection changed layout mode %v to %v", evidenceBefore.Mode(), evidenceAfter.Mode())
		}
	}

	var stderr bytes.Buffer
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-maintenance-generation-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	code := cli.Run(context.Background(), []string{"daemon", "--workspace", workspace.Root, "--socket", socketPath}, nil, io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "maintenance.generation_rejected") {
		t.Fatalf("daemon rejection code=%d stderr=%s", code, stderr.String())
	}
	assertUnchanged()

	stderr.Reset()
	code = cli.Run(context.Background(), []string{"open", "--no-focus", workspace.Root}, nil, io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "maintenance generation rejected") {
		t.Fatalf("open rejection code=%d stderr=%s", code, stderr.String())
	}
	assertUnchanged()
}

func TestKickOffRejectsUnknownMaintenanceGenerationForEveryExistingWorkspaceResolution(t *testing.T) {
	for _, route := range []string{"explicit workspace", "repository default root", "cwd discovery"} {
		t.Run(route, func(t *testing.T) {
			isolateHerdrEnvironment(t)
			parent := t.TempDir()
			repository := newCommittedRepositoryAt(t, filepath.Join(parent, "repository"))
			workspaceRoot, err := optimizationworkspace.DefaultRoot(repository)
			if err != nil {
				t.Fatal(err)
			}
			workspace, err := optimizationworkspace.Create(context.Background(), workspaceRoot, repository)
			if err != nil {
				t.Fatal(err)
			}
			protected := seedUnauthorizedMaintenanceWorkspace(t, workspace)
			before := snapshotFiles(t, protected)
			t.Setenv("HERDR_SOCKET_PATH", filepath.Join(parent, "unreachable-herdr.sock"))

			args := []string{"kick-off", "--workspace", workspace.Root}
			if route == "repository default root" {
				args = []string{"kick-off", "--repository", repository}
			}
			if route == "cwd discovery" {
				original, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chdir(workspace.Root); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chdir(original) })
				args = []string{"kick-off"}
			}
			var stderr bytes.Buffer
			if code := cli.Run(context.Background(), args, nil, io.Discard, &stderr); code != 1 ||
				!strings.Contains(stderr.String(), "maintenance generation rejected") {
				t.Fatalf("%s rejection code=%d stderr=%s", route, code, stderr.String())
			}
			assertFilesUnchanged(t, protected, before)
		})
	}
}

func TestMaintenancePrepareRejectsUnboundActiveSessionWithoutQuiescing(t *testing.T) {
	isolateHerdrEnvironment(t)
	ctx := context.Background()
	workspace, err := optimizationworkspace.Create(ctx, filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: workspace.Identity.ID, Repository: workspace.Identity.SourceRepository,
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	work := view.Works[0]
	session := symphony.AgentSession{
		ID: "frozen-session", WorkID: work.ID, Generation: work.Generation, Role: work.Role,
		AgentKind: "codex", AgentName: "frozen-agent", Status: symphony.AgentSessionRunning,
	}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	socketDir, err := os.MkdirTemp("/tmp", "pika-go-maintenance-dispatch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	daemonCtx, cancel := context.WithCancel(ctx)
	var daemonStderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(daemonCtx, []string{"daemon", "--workspace", workspace.Root, "--socket", socketPath}, nil, io.Discard, &daemonStderr)
	}()
	waitForHealth(t, socketPath, &daemonStderr)
	if _, err := control.MaintenancePrepare(ctx, socketPath, protocol.MaintenancePrepareRequest{
		RequestID: "prepare-1",
		ToGeneration: protocol.MaintenanceGeneration{
			Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}); err == nil || !strings.Contains(err.Error(), "maintenance_rejected") {
		cancel()
		t.Fatalf("prepare with unbound active Session error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.RuntimeRoot, "daemon", "maintenance.json")); !errors.Is(err, os.ErrNotExist) {
		cancel()
		t.Fatalf("rejected prepare persisted maintenance state: %v", err)
	}
	if _, err := control.CancelWork(ctx, socketPath, work.ID, protocol.CancelWorkRequest{
		Mutation: protocol.Mutation{RequestID: "cancel-after-prepare"},
	}); err != nil {
		cancel()
		t.Fatalf("runtime remained quiesced after rejected prepare: %v", err)
	}
	after, err := control.Status(ctx, socketPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if after.Works[0].Status != symphony.WorkCancelled {
		cancel()
		t.Fatalf("runtime mutation did not remain usable after rejected prepare: %+v", after.Works[0])
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("daemon exit=%d stderr=%s", code, daemonStderr.String())
	}
}

func TestWorkspaceDaemonInitPersistsConfigurationAndOptimization(t *testing.T) {
	isolateHerdrEnvironment(t)
	repository := newCommittedRepository(t)
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	// flow-v2 publishes immutable skill content below the workspace. Restore
	// write bits before t.TempDir's cleanup so this integration fixture can be
	// removed on filesystems where RemoveAll does not chmod descendants.
	t.Cleanup(func() { makeWorkspaceWritable(workspace.Root) })
	fakeCodex := writeTestCodexExecutable(t, t.TempDir())
	t.Setenv("PIKA_GO_CODEX_EXECUTABLE", fakeCodex)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	writeTestCodexModelsCache(t, codexHome)
	t.Setenv("CODEX_HOME", codexHome)
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-workspace-init-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	ctx, cancel := context.WithCancel(context.Background())
	var daemonStderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"daemon", "--socket", socketPath, "--workspace", workspace.Root}, nil, io.Discard, &daemonStderr)
	}()
	waitForHealth(t, socketPath, &daemonStderr)
	var initStdout, initStderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--socket", socketPath, "--repository", repository, "--defaults", "--json"}, nil, &initStdout, &initStderr); code != 0 {
		cancel()
		t.Fatalf("Workspace init exit = %d, stdout=%q stderr=%q daemon=%s", code, initStdout.String(), initStderr.String(), daemonStderr.String())
	}
	view, err := control.Status(context.Background(), socketPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if view.Optimization.Status != symphony.OptimizationDraftingBaseline || view.Optimization.FlowVersion != symphony.FlowVersion2 || view.SkillSnapshot == nil || len(view.Works) != 1 {
		cancel()
		t.Fatalf("Workspace optimization = %+v works=%+v", view.Optimization, view.Works)
	}
	var humanStatus, humanStatusErr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"status", "--socket", socketPath}, nil, &humanStatus, &humanStatusErr); code != 0 {
		cancel()
		t.Fatalf("human status exit = %d, stderr=%q", code, humanStatusErr.String())
	}
	if !strings.Contains(humanStatus.String(), "flow v2") || !strings.Contains(humanStatus.String(), "skill snapshot: "+view.SkillSnapshot.SnapshotID) || !strings.Contains(humanStatus.String(), "knowledge: verified=0 provisional=0 negative=0 inconclusive=0") {
		cancel()
		t.Fatalf("human status omitted flow-v2 summaries: %q", humanStatus.String())
	}
	if _, err := os.Stat(workspace.ConfigPath); err != nil {
		cancel()
		t.Fatalf("Workspace pika.toml: %v", err)
	}
	statusOutput, statusErr := exec.Command("git", "-C", repository, "status", "--porcelain=v1", "--untracked-files=all").CombinedOutput()
	if statusErr != nil {
		cancel()
		t.Fatalf("source status: %v: %s", statusErr, statusOutput)
	}
	if got := strings.TrimSpace(string(statusOutput)); got != "" {
		cancel()
		t.Fatalf("source checkout was modified: %q", got)
	}
	snapshotRoot := filepath.Join(workspace.Root, "toolkits", "kda-skills", view.SkillSnapshot.SnapshotID)
	makeWorkspaceWritable(snapshotRoot)
	if err := os.RemoveAll(snapshotRoot); err != nil {
		cancel()
		t.Fatal(err)
	}
	var backupStdout, backupStderr bytes.Buffer
	backupPath := filepath.Join(t.TempDir(), "backup", "pika.db")
	if code := cli.Run(context.Background(), []string{"backup", "--socket", socketPath, "--output", backupPath}, nil, &backupStdout, &backupStderr); code != 1 || !strings.Contains(backupStderr.String(), "frozen skill snapshot provenance") {
		cancel()
		t.Fatalf("backup with missing flow-v2 provenance exit=%d stdout=%q stderr=%q", code, backupStdout.String(), backupStderr.String())
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("daemon exit = %d, stderr=%s", code, daemonStderr.String())
	}
}

func TestColdWorkspaceDaemonRefreshesManagedCodexIntegrationWithStableExecutable(t *testing.T) {
	isolateHerdrEnvironment(t)
	repository := newCommittedRepository(t)
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	fakeCodex := writeTestCodexExecutable(t, t.TempDir())
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	writeTestCodexModelsCache(t, codexHome)
	initializer := configuration.Initializer{
		Workspace: &workspace, CodexHome: codexHome,
		PikaExecutable: "/usr/bin/false", CodexExecutable: fakeCodex,
	}
	if _, err := initializer.Prepare(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIKA_GO_CODEX_EXECUTABLE", fakeCodex)
	t.Setenv("PIKA_GO_INTEGRATION_EXECUTABLE", "/bin/echo")
	t.Setenv("CODEX_HOME", codexHome)

	socketDir, err := os.MkdirTemp("/tmp", "pika-go-workspace-refresh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	ctx, cancel := context.WithCancel(context.Background())
	var daemonStderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"daemon", "--socket", socketPath, "--workspace", workspace.Root}, nil, io.Discard, &daemonStderr)
	}()
	waitForHealth(t, socketPath, &daemonStderr)

	profile, err := os.ReadFile(filepath.Join(codexHome, "pika-go-managed.config.toml"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if !strings.Contains(string(profile), `command = "/bin/echo"`) || strings.Contains(string(profile), `command = "/usr/bin/false"`) {
		cancel()
		t.Fatalf("managed profile was not refreshed for stable integration executable: %s", profile)
	}

	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("daemon exit = %d, stderr=%s", code, daemonStderr.String())
	}
}

func TestDaemonAndStatusUseSocketFromEnvironment(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-env-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("PIKA_GO_SOCKET", filepath.Join(socketDir, "pika-go.sock"))

	ctx, cancel := context.WithCancel(context.Background())
	var daemonStderr bytes.Buffer
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- cli.Run(ctx, []string{"daemon"}, nil, &bytes.Buffer{}, &daemonStderr)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var stdout bytes.Buffer
		if cli.Run(context.Background(), []string{"status", "--json"}, nil, &stdout, &bytes.Buffer{}) == 0 {
			cancel()
			if code := <-daemonDone; code != 0 {
				t.Fatalf("daemon exit code = %d, stderr = %q", code, daemonStderr.String())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatalf("daemon did not become discoverable; stderr = %q", daemonStderr.String())
}

func TestDaemonAndStatusDeriveSameSocketInsideHerdrWorkspace(t *testing.T) {
	t.Setenv("PIKA_GO_SOCKET", "")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join("/tmp", "herdr-test-"+time.Now().Format("150405.000000000")+".sock"))
	t.Setenv("HERDR_PANE_ID", "w42:p7")

	ctx, cancel := context.WithCancel(context.Background())
	var daemonStderr bytes.Buffer
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- cli.Run(ctx, []string{"daemon"}, nil, &bytes.Buffer{}, &daemonStderr)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cli.Run(context.Background(), []string{"status", "--json"}, nil, &bytes.Buffer{}, &bytes.Buffer{}) == 0 {
			cancel()
			if code := <-daemonDone; code != 0 {
				t.Fatalf("daemon exit code = %d, stderr = %q", code, daemonStderr.String())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatalf("daemon did not become discoverable from Herdr context; stderr = %q", daemonStderr.String())
}

func TestDaemonReclaimsStaleSocket(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-stale-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika-go.sock")

	stale, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale socket: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var daemonStderr bytes.Buffer
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- cli.Run(ctx, []string{"daemon", "--socket", socketPath}, nil, &bytes.Buffer{}, &daemonStderr)
	}()
	waitForStatus(t, socketPath, &daemonStderr)
	cancel()
	if code := <-daemonDone; code != 0 {
		t.Fatalf("daemon exit code = %d, stderr = %q", code, daemonStderr.String())
	}
}

func TestSecondDaemonCannotTakeLiveSocket(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-owner-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika-go.sock")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var firstStderr bytes.Buffer
	firstDone := make(chan int, 1)
	go func() {
		firstDone <- cli.Run(ctx, []string{"daemon", "--socket", socketPath}, nil, &bytes.Buffer{}, &firstStderr)
	}()
	waitForStatus(t, socketPath, &firstStderr)

	var secondStderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"daemon", "--socket", socketPath}, nil, &bytes.Buffer{}, &secondStderr); code != 1 {
		t.Fatalf("second daemon exit code = %d, want 1; stderr = %q", code, secondStderr.String())
	}
	if !bytes.Contains(secondStderr.Bytes(), []byte("already listening")) {
		t.Fatalf("second daemon stderr = %q, want ownership error", secondStderr.String())
	}

	waitForStatus(t, socketPath, &firstStderr)
	cancel()
	select {
	case code := <-firstDone:
		if code != 0 {
			t.Fatalf("first daemon exit code = %d, stderr = %q", code, firstStderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first daemon did not stop after cancellation")
	}
}

func TestSecondDaemonCannotOwnSameDurableInstanceOnAnotherSocket(t *testing.T) {
	isolateHerdrEnvironment(t)
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-instance-owner-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	firstSocket := filepath.Join(socketDir, "first.sock")
	secondSocket := filepath.Join(socketDir, "second.sock")
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Cleanup(func() { makeWorkspaceWritable(stateDir) })
	configDir := filepath.Join(t.TempDir(), "config")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var firstStderr bytes.Buffer
	firstDone := make(chan int, 1)
	go func() {
		firstDone <- cli.Run(ctx, []string{"daemon", "--socket", firstSocket, "--state-dir", stateDir, "--config-dir", configDir, "--instance", "same-instance"}, nil, &bytes.Buffer{}, &firstStderr)
	}()
	waitForHealth(t, firstSocket, &firstStderr)

	var secondStderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"daemon", "--socket", secondSocket, "--state-dir", stateDir, "--config-dir", configDir, "--instance", "same-instance"}, nil, &bytes.Buffer{}, &secondStderr); code != 1 {
		t.Fatalf("second daemon exit = %d, stderr = %q", code, secondStderr.String())
	}
	if !bytes.Contains(secondStderr.Bytes(), []byte("already owned")) {
		t.Fatalf("second daemon stderr = %q", secondStderr.String())
	}
	if _, err := control.Health(context.Background(), firstSocket); err != nil {
		t.Fatalf("first daemon unhealthy after ownership conflict: %v", err)
	}
	cancel()
	if code := <-firstDone; code != 0 {
		t.Fatalf("first daemon exit = %d, stderr = %q", code, firstStderr.String())
	}
}

func TestDaemonPreservesNonSocketAtConfiguredPath(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-file-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika-go.sock")
	if err := os.WriteFile(socketPath, []byte("owned by user\n"), 0o600); err != nil {
		t.Fatalf("create regular file: %v", err)
	}

	var stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"daemon", "--socket", socketPath}, nil, &bytes.Buffer{}, &stderr); code != 1 {
		t.Fatalf("daemon exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	got, err := os.ReadFile(socketPath)
	if err != nil {
		t.Fatalf("read preserved file: %v", err)
	}
	if string(got) != "owned by user\n" {
		t.Fatalf("file content = %q", got)
	}
}

func TestCLIInitAndStatusPersistThroughDaemon(t *testing.T) {
	isolateHerdrEnvironment(t)
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	writeTestCodexModelsCache(t, codexHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("PIKA_GO_CODEX_EXECUTABLE", writeTestCodexExecutable(t, t.TempDir()))
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-control-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika-go.sock")
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Cleanup(func() { makeWorkspaceWritable(stateDir) })
	configDir := filepath.Join(t.TempDir(), "config")
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	resolvedRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatalf("resolve repository: %v", err)
	}
	initializer := configuration.Initializer{
		ConfigRoot: configDir, StateRoot: stateDir, InstanceID: "instance-1",
		CodexHome: codexHome, PikaExecutable: "/bin/echo", CodexExecutable: "/bin/sh",
	}
	if _, err := initializer.Prepare(context.Background(), repository); err != nil {
		t.Fatalf("prepare user configuration: %v", err)
	}
	configPath := filepath.Join(configDir, "instances", "instance-1", "config.toml")
	defaultAgents := configuration.DefaultAgents()
	customConfig, err := configuration.RenderConfiguration(resolvedRepository, defaultAgents, defaultAgents["iteration"], defaultAgents["iteration"])
	if err != nil {
		t.Fatal(err)
	}
	customConfig = strings.Replace(customConfig, "max_pending_attempts = 8", "max_pending_attempts = 6", 1)
	if err := os.WriteFile(configPath, []byte(customConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var daemonStderr bytes.Buffer
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- cli.Run(ctx, []string{
			"daemon", "--socket", socketPath,
			"--state-dir", stateDir,
			"--config-dir", configDir,
			"--instance", "instance-1",
		}, nil, &bytes.Buffer{}, &daemonStderr)
	}()
	waitForHealth(t, socketPath, &daemonStderr)

	var initStdout bytes.Buffer
	var initStderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{
		"init", "--socket", socketPath,
		"--repository", repository,
		"--request-id", "request-init",
		"--json",
	}, nil, &initStdout, &initStderr); code != 0 {
		t.Fatalf("init exit code = %d, stderr = %q", code, initStderr.String())
	}

	var statusStdout bytes.Buffer
	var statusStderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"status", "--socket", socketPath, "--json"}, nil, &statusStdout, &statusStderr); code != 0 {
		t.Fatalf("status exit code = %d, stderr = %q", code, statusStderr.String())
	}
	var status struct {
		Optimization struct {
			ID                   string `json:"id"`
			Status               string `json:"status"`
			IterationConcurrency int64  `json:"iteration_concurrency"`
			MaxPendingAttempts   int64  `json:"max_pending_attempts"`
		} `json:"optimization"`
		Works []struct {
			ID     string `json:"id"`
			Role   string `json:"role"`
			Status string `json:"status"`
		} `json:"works"`
		PendingEffectCount int64 `json:"pending_effect_count"`
	}
	if err := json.Unmarshal(statusStdout.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v; output = %q", err, statusStdout.String())
	}
	for _, forbidden := range [][]byte{[]byte(`"repository":`), []byte(`"experiment":`), []byte(`"message":`), []byte(`"provider_capabilities":`)} {
		if bytes.Contains(statusStdout.Bytes(), forbidden) {
			t.Fatalf("CLI status reintroduced forbidden field %s: %s", forbidden, statusStdout.Bytes())
		}
	}
	if status.Optimization.ID != "instance-1" || status.Optimization.Status != "drafting_baseline" {
		t.Fatalf("optimization status = %+v", status.Optimization)
	}
	if status.Optimization.IterationConcurrency != 2 || status.Optimization.MaxPendingAttempts != 6 {
		t.Fatalf("persisted scheduler settings = %+v", status.Optimization)
	}
	if len(status.Works) != 1 || status.Works[0].Role != "baseline_draft" || status.Works[0].Status != "pending" {
		t.Fatalf("works = %+v", status.Works)
	}
	deadline := time.Now().Add(2 * time.Second)
	for status.PendingEffectCount != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		view, err := control.Status(context.Background(), socketPath)
		if err != nil {
			t.Fatal(err)
		}
		status.PendingEffectCount = view.PendingEffectCount
	}
	if status.PendingEffectCount != 0 {
		t.Fatalf("recording dispatcher left %d pending effects", status.PendingEffectCount)
	}
	var replayStdout bytes.Buffer
	if code := cli.Run(context.Background(), []string{
		"init", "--socket", socketPath,
		"--repository", repository,
		"--request-id", "request-init",
		"--json",
	}, nil, &replayStdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("replayed init exit code = %d", code)
	}
	var originalReceipt, replayedReceipt struct {
		ID       string `json:"id"`
		Replayed bool   `json:"replayed"`
	}
	if err := json.Unmarshal(initStdout.Bytes(), &originalReceipt); err != nil {
		t.Fatalf("decode original receipt: %v", err)
	}
	if err := json.Unmarshal(replayStdout.Bytes(), &replayedReceipt); err != nil {
		t.Fatalf("decode replayed receipt: %v", err)
	}
	if replayedReceipt.ID != originalReceipt.ID || !replayedReceipt.Replayed {
		t.Fatalf("replayed receipt = %+v, original = %+v", replayedReceipt, originalReceipt)
	}

	var cancelStdout bytes.Buffer
	if code := cli.Run(context.Background(), []string{"cancel-work", "--socket", socketPath, "--request-id", "request-cancel", status.Works[0].ID}, nil, &cancelStdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("cancel-work exit code = %d", code)
	}
	var cancelReplayStdout bytes.Buffer
	if code := cli.Run(context.Background(), []string{"cancel-work", "--socket", socketPath, "--request-id", "request-cancel", status.Works[0].ID}, nil, &cancelReplayStdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("replayed cancel-work exit code = %d", code)
	}
	if err := json.Unmarshal(cancelStdout.Bytes(), &originalReceipt); err != nil {
		t.Fatalf("decode cancel receipt: %v", err)
	}
	if err := json.Unmarshal(cancelReplayStdout.Bytes(), &replayedReceipt); err != nil {
		t.Fatalf("decode replayed cancel receipt: %v", err)
	}
	if replayedReceipt.ID != originalReceipt.ID || !replayedReceipt.Replayed {
		t.Fatalf("replayed cancel receipt = %+v, original = %+v", replayedReceipt, originalReceipt)
	}
	if code := cli.Run(context.Background(), []string{"draft-baseline", "--socket", socketPath, "--request-id", "request-restart"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("draft-baseline exit code = %d", code)
	}
	if code := cli.Run(context.Background(), []string{"shutdown", "--socket", socketPath, "--request-id", "request-shutdown"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("shutdown exit code = %d", code)
	}
	select {
	case code := <-daemonDone:
		t.Fatalf("daemon exited with pending Work: code=%d stderr=%q", code, daemonStderr.String())
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case code := <-daemonDone:
		if code != 0 {
			t.Fatalf("daemon exit code = %d, stderr = %q", code, daemonStderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop after test context cancellation")
	}
	databasePath := filepath.Join(stateDir, "instances", "instance-1", "pika.db")
	persisted, err := symphony.Open(context.Background(), databasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("open drained database: %v", err)
	}
	drainedView, err := persisted.Inspect(context.Background(), symphony.Status{})
	if closeErr := persisted.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("inspect drained database: %v", err)
	}
	if drainedView.Optimization.Status != symphony.OptimizationDraining || len(drainedView.Works) != 2 || drainedView.Works[1].Status != symphony.WorkPending {
		t.Fatalf("persisted draining state = %+v", drainedView)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon socket remains after drain: %v", err)
	}
}

func writeTestCodexModelsCache(t *testing.T, codexHome string) {
	t.Helper()
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `{"models":[{"slug":"gpt-5.6-sol","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},{"slug":"gpt-5.6-terra","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]}]}`
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestCodexExecutable(t *testing.T, root string) string {
	t.Helper()
	executable := filepath.Join(root, "codex")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo codex-test; exit 0; fi\n" +
		"if [ \"$1\" = login ] && [ \"$2\" = status ]; then echo Logged-in; exit 0; fi\n" +
		"if [ \"$1\" = debug ] && [ \"$2\" = prompt-input ] && [ -L .agents/skills/pika-kda-kernelwiki ] && [ -L .agents/skills/pika-kda-ncu-report ]; then root=\"$(pwd)/.agents/skills\"; printf '[{\"type\":\"message\",\"role\":\"developer\",\"content\":[{\"type\":\"input_text\",\"text\":\"<skills_instructions>\\\\n## Skills\\\\n### Skill roots\\\\n- `r0` = `%s`\\\\n### Available skills\\\\n- KernelWiki: probe (file: r0/pika-kda-kernelwiki/SKILL.md)\\\\n- ncu-report-skill: probe (file: r0/pika-kda-ncu-report/SKILL.md)\\\\n</skills_instructions>\"}]}]\\n' \"$root\"; exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return executable
}

func TestCLIInteractiveInitBuildsPerRoleMixedProviderConfiguration(t *testing.T) {
	isolateHerdrEnvironment(t)
	root := t.TempDir()
	t.Cleanup(func() { makeWorkspaceWritable(root) })
	codexHome := filepath.Join(root, "codex-home")
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	modelsCache := `{"models":[
		{"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},
		{"slug":"gpt-5.6-terra","display_name":"GPT-5.6-Terra","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]}
	]}`
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(modelsCache), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIKA_GO_CODEX_EXECUTABLE", writeTestCodexExecutable(t, root))
	cursorExecutable := filepath.Join(root, "cursor-agent")
	t.Setenv("PIKA_GO_CURSOR_TEST_STATUS_SEEN", filepath.Join(root, "cursor-status-seen"))
	if err := os.WriteFile(cursorExecutable, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 2026.08.31-4057e58; exit 0; fi\nif [ \"$1\" = status ]; then if [ ! -e \"$PIKA_GO_CURSOR_TEST_STATUS_SEEN\" ]; then : > \"$PIKA_GO_CURSOR_TEST_STATUS_SEEN\"; sleep 10.1; fi; echo 'Logged in as test@example.com'; exit 0; fi\nif [ \"$1\" = --plugin-dir ] && [ \"$3\" = --help ]; then echo --plugin-dir; exit 0; fi\nif [ \"$1\" = --list-models ]; then printf 'Available models\\n\\nauto - Auto (default)\\ngpt-5.6-sol-low - GPT-5.6 Sol Low\\ngpt-5.6-sol-medium - GPT-5.6 Sol Medium\\ngpt-5.6-sol-high - GPT-5.6 Sol High\\ngpt-5.6-sol-xhigh - GPT-5.6 Sol Extra High\\ngpt-5.6-sol-max - GPT-5.6 Sol Max\\ngpt-5.6-terra-low - GPT-5.6 Terra Low\\ngpt-5.6-terra-medium - GPT-5.6 Terra Medium\\ngpt-5.6-terra-high - GPT-5.6 Terra High\\n'; exit 0; fi\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIKA_GO_CURSOR_EXECUTABLE", cursorExecutable)
	t.Setenv("PIKA_GO_CURSOR_MCP_PATH", filepath.Join(root, "cursor-home", "mcp.json"))
	t.Setenv("PIKA_GO_CURSOR_HOOKS_PATH", filepath.Join(root, "cursor-home", "hooks.json"))
	socketDirectory, err := os.MkdirTemp("/tmp", "pika-interactive-init-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "pika.sock")
	stateRoot, configRoot := filepath.Join(root, "state"), filepath.Join(root, "config")
	repository := filepath.Join(root, "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var daemonStderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"daemon", "--socket", socketPath, "--state-dir", stateRoot, "--config-dir", configRoot, "--instance", "interactive"}, nil, io.Discard, &daemonStderr)
	}()
	waitForHealth(t, socketPath, &daemonStderr)
	input := strings.Join([]string{
		"99", "2", "", "9", "", "", "2",
		"1", "", "",
		"2", "2", "5", "2", "2", "1",
		"2", "1", "2", "", "1",
		"1", "", "",
		"2", "", "3", "1", "1",
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--socket", socketPath, "--repository", repository}, strings.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("init exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	configPath := filepath.Join(configRoot, "instances", "interactive", "config.toml")
	baseline, err := configuration.LoadAgent(configPath, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	iterationAgents, err := configuration.LoadIterationAgents(configPath)
	if err != nil {
		t.Fatal(err)
	}
	iteration := iterationAgents[0]
	followUp, err := configuration.LoadAgent(configPath, "follow_up")
	if err != nil {
		t.Fatal(err)
	}
	verification, err := configuration.LoadAgent(configPath, "baseline_verify")
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Kind != "cursor" || baseline.Model != "auto" || baseline.ReasoningEffort != "" || !slices.Equal(baseline.Args, []string{"--force", "--approve-mcps", "--trust"}) ||
		iteration.Kind != "cursor" || iteration.Model != "gpt-5.6-sol" || iteration.ReasoningEffort != "max" || !slices.Equal(iteration.Args, []string{"--auto-review"}) ||
		len(iterationAgents) != 2 || iterationAgents[1].Kind != "codex" ||
		followUp.Kind != "cursor" || followUp.Model != "auto" || followUp.ReasoningEffort != "" || !slices.Equal(followUp.Args, []string{"--approve-mcps"}) || verification.Kind != "codex" {
		t.Fatalf("baseline=%+v verification=%+v iterations=%+v follow_up=%+v", baseline, verification, iterationAgents, followUp)
	}
	for _, want := range []string{"Configure agents.iteration[1]", "Configure agents.iteration[2]", "  configure another Iteration Agent:\n    1) No\n    2) Yes", "  backend:\n    1) codex\n    2) cursor", "  Select backend [1]:", "  model:\n    1) auto-routing (default)\n    2) gpt-5.6-sol", "  model:\n    1) default\n    2) GPT-5.6-Sol (gpt-5.6-sol)", "  reasoning effort: provider default", "  reasoning effort:\n    1) low", "  Cursor command approval:\n    1) Allow commands automatically", "  Cursor MCP approval:\n    1) Approve configured MCP servers automatically", "  Cursor workspace trust:\n    1) Keep the current workspace trust setting", "  Select model [1]:", "  Select model [2]:", "  Select reasoning effort [3]:", "Enter a number from 1 to 2.", "Enter a number from 1 to 3."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("interactive init output is missing %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "Cursor args JSON") {
		t.Fatalf("interactive init still exposes raw Cursor argv:\n%s", stdout.String())
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("daemon exit=%d stderr=%q", code, daemonStderr.String())
	}
}

func TestInitFailureIsRecordedAndStopsDaemonWithoutWork(t *testing.T) {
	isolateHerdrEnvironment(t)
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-init-failure-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika-go.sock")
	stateDir := filepath.Join(t.TempDir(), "state")
	configDir := filepath.Join(t.TempDir(), "config")
	repository := filepath.Join(t.TempDir(), "not-a-git-repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatalf("create non-Git repository: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var daemonStderr bytes.Buffer
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- cli.Run(ctx, []string{
			"daemon", "--socket", socketPath,
			"--state-dir", stateDir,
			"--config-dir", configDir,
			"--instance", "failed-instance",
		}, nil, &bytes.Buffer{}, &daemonStderr)
	}()
	waitForHealth(t, socketPath, &daemonStderr)

	var initStderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--socket", socketPath, "--repository", repository, "--defaults", "--json"}, nil, &bytes.Buffer{}, &initStderr); code != 1 {
		t.Fatalf("init exit = %d, stderr = %q", code, initStderr.String())
	}
	if !bytes.Contains(initStderr.Bytes(), []byte("invalid_configuration")) {
		t.Fatalf("init stderr = %q", initStderr.String())
	}
	select {
	case code := <-daemonDone:
		if code != 1 {
			t.Fatalf("daemon exit = %d, stderr = %q", code, daemonStderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not exit after initialization failure")
	}

	engine, err := symphony.Open(context.Background(), filepath.Join(stateDir, "instances", "failed-instance", "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatalf("reopen failed instance database: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	diagnostics, err := engine.InitDiagnostics(context.Background())
	if err != nil {
		t.Fatalf("read init diagnostics: %v", err)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Message, "not a Git worktree") {
		t.Fatalf("init diagnostics = %+v", diagnostics)
	}
	_, err = engine.Inspect(context.Background(), symphony.Status{})
	var domainErr *symphony.DomainError
	if !errors.As(err, &domainErr) || domainErr.Code != symphony.CodeNotInitialized {
		t.Fatalf("inspect error = %v, want not initialized", err)
	}
}

func TestCLIBackOffTraversesDaemonAndSQLite(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-backoff-control-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika-go.sock")
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	draft, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: draft.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatalf("submit baseline: %v", err)
	}

	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	daemonDone := make(chan error, 1)
	go func() {
		daemonDone <- daemon.Serve(serveCtx, daemon.Config{SocketPath: socketPath, Version: "test", InstanceID: "optimization-1", Symphony: engine})
	}()
	waitForHealth(t, socketPath, &bytes.Buffer{})
	var stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"back-off", "--socket", socketPath, "--request-id", "back-off", "-m", "increase benchmark repeats"}, nil, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("back-off exit = %d, stderr = %q", code, stderr.String())
	}
	after, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect after back-off: %v", err)
	}
	if after.Optimization.Status != symphony.OptimizationDraftingBaseline || len(after.BackOffs) != 1 || after.BackOffs[0].Message != "increase benchmark repeats" {
		t.Fatalf("view after CLI back-off = %+v", after)
	}
	cancel()
	if err := <-daemonDone; err != nil {
		t.Fatalf("daemon exit: %v", err)
	}
}

func TestCLISchedulerControlUsesNewCommandsAndFailsOnPartialDelivery(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-scheduler-control-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika-go.sock")
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	commands := make(chan symphony.Command, 2)
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(serveCtx, daemon.Config{
			SocketPath: socketPath, Version: "test", InstanceID: "optimization", Symphony: engine,
			SchedulerControl: func(_ context.Context, command symphony.Command) (protocol.SchedulerControlResponse, error) {
				commands <- command
				response := protocol.SchedulerControlResponse{Receipt: symphony.Receipt{ID: "receipt", RequestID: "request", Revision: 2}}
				switch command.(type) {
				case symphony.PauseScheduler:
					response.Control = symphony.SchedulerControlCycleView{ID: "pause-cycle", Action: "pause", Status: "complete"}
				case symphony.ResumeScheduler:
					response.Control = symphony.SchedulerControlCycleView{ID: "resume-cycle", Action: "resume", Status: "partial", Actions: []symphony.SchedulerControlActionView{{ID: "action", Status: symphony.SchedulerActionDeliveryUnknown}}}
				}
				return response, nil
			},
		})
	}()
	waitForHealth(t, socketPath, &bytes.Buffer{})
	var pauseOut, pauseErr bytes.Buffer
	if code := cli.Run(ctx, []string{"pause", "--socket", socketPath, "--request-id", "pause"}, nil, &pauseOut, &pauseErr); code != 0 {
		t.Fatalf("pause exit=%d out=%q err=%q", code, pauseOut.String(), pauseErr.String())
	}
	if _, ok := (<-commands).(symphony.PauseScheduler); !ok {
		t.Fatal("pause endpoint did not receive PauseScheduler")
	}
	var resumeOut, resumeErr bytes.Buffer
	if code := cli.Run(ctx, []string{"resume", "--socket", socketPath, "--request-id", "resume"}, nil, &resumeOut, &resumeErr); code != 1 {
		t.Fatalf("resume exit=%d out=%q err=%q", code, resumeOut.String(), resumeErr.String())
	}
	if _, ok := (<-commands).(symphony.ResumeScheduler); !ok {
		t.Fatal("resume endpoint did not receive ResumeScheduler")
	}
	if !strings.Contains(resumeOut.String(), "delivery_unknown") {
		t.Fatalf("partial response was not printed: %q", resumeOut.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCLIBackupCreatesValidatedSnapshotThroughDaemon(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}

	socketDir, err := os.MkdirTemp("/tmp", "pika-go-backup-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(serveCtx, daemon.Config{
			SocketPath: socketPath, Version: "test", InstanceID: "optimization", Symphony: engine,
			Backup: engine.Backup,
		})
	}()
	waitForHealth(t, socketPath, &bytes.Buffer{})

	destination := filepath.Join(t.TempDir(), "backups", "snapshot.db")
	var stdout, stderr bytes.Buffer
	if code := cli.Run(ctx, []string{"backup", "--socket", socketPath, "--output", destination}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("backup exit = %d, stderr = %q", code, stderr.String())
	}
	var response struct {
		Destination string `json:"destination"`
		ByteSize    int64  `json:"byte_size"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode backup response: %v; output=%q", err, stdout.String())
	}
	if response.Destination != destination || response.ByteSize <= 0 {
		t.Fatalf("backup response = %+v", response)
	}
	snapshot, err := symphony.Open(ctx, destination, symphony.Options{})
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer snapshot.Close()
	view, err := snapshot.Inspect(ctx, symphony.Status{})
	if err != nil || view.Optimization.ID != "optimization" {
		t.Fatalf("backup view = %+v, err=%v", view, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cli.Run(ctx, []string{"backup", "--socket", socketPath, "--output", destination}, nil, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("second backup exit=%d stderr=%q", code, stderr.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("daemon exit: %v", err)
	}
}

func TestDaemonGracefulShutdownWaitsForChildThenExits(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	startEffects, err := engine.ClaimPendingEffects(ctx, 10)
	if err != nil || len(startEffects) != 1 {
		t.Fatalf("initial effects=%+v err=%v", startEffects, err)
	}
	if err := engine.MarkEffectDispatched(ctx, startEffects[0].ID); err != nil {
		t.Fatal(err)
	}
	session := symphony.AgentSession{ID: "child-session", WorkID: view.Works[0].ID, Generation: view.Works[0].Generation, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "child", Status: symphony.AgentSessionRunning}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}

	socketDir, err := os.MkdirTemp("/tmp", "pika-go-drain-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(serveCtx, daemon.Config{
			SocketPath: socketPath, Version: "test", InstanceID: "optimization", Symphony: engine,
			DrainReady: engine.DrainReady,
		})
	}()
	waitForHealth(t, socketPath, &bytes.Buffer{})
	if _, err := control.Shutdown(ctx, socketPath, protocol.ShutdownRequest{Mutation: protocol.Mutation{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("daemon exited while child was active: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "finish"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	closeEffects, err := engine.ClaimPendingEffects(ctx, 10)
	if err != nil || len(closeEffects) != 1 || closeEffects[0].Type != "work.close_requested" {
		t.Fatalf("close effects=%+v err=%v", closeEffects, err)
	}
	if err := engine.MarkAgentSessionEnded(ctx, session.ID, symphony.AgentSessionExited); err != nil {
		t.Fatal(err)
	}
	if err := engine.MarkEffectDispatched(ctx, closeEffects[0].ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon drain exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not exit after its child and effects drained")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon socket remains after drain: %v", err)
	}
}

func TestCodexHookIsSilentAndFailOpen(t *testing.T) {
	t.Setenv("PIKA_GO_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv("PIKA_SESSION_ID", "pika-session")
	for _, input := range []string{`{"session_id":"codex-session","hook_event_name":"SessionStart"}`, `{not-json`} {
		var stdout, stderr bytes.Buffer
		if code := cli.Run(context.Background(), []string{"hook", "codex"}, strings.NewReader(input), &stdout, &stderr); code != 0 {
			t.Fatalf("hook exit = %d for %q", code, input)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("hook emitted output: stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"hook", "codex"}, strings.NewReader(`{"session_id":"codex-session","turn_id":"turn","hook_event_name":"Stop"}`), &stdout, &stderr); code != 0 {
		t.Fatalf("Stop hook exit = %d", code)
	}
	if stdout.String() != "{}\n" || stderr.Len() != 0 {
		t.Fatalf("Stop hook protocol output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func isolateHerdrEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HERDR_WORKSPACE_ID", "")
	t.Setenv("PIKA_GO_AGENT_PATH_PREFIX", "")
}

func TestCodexHookTraversesDaemonIntoConversationJournal(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	session := symphony.AgentSession{
		ID: "pika-session", WorkID: view.Works[0].ID, Generation: view.Works[0].Generation,
		Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "test-codex", Status: symphony.AgentSessionStarting,
	}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}

	socketDir, err := os.MkdirTemp("/tmp", "pika-go-hook-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(serveCtx, daemon.Config{
			SocketPath: socketPath, Version: "test", InstanceID: "optimization", Symphony: engine,
			IngestProviderEvent: engine.IngestProviderEvent,
		})
	}()
	waitForHealth(t, socketPath, &bytes.Buffer{})
	t.Setenv("PIKA_GO_SOCKET", socketPath)
	t.Setenv("PIKA_SESSION_ID", session.ID)
	event := `{"session_id":"codex-session","turn_id":"turn-1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"tool-1","tool_input":{"command":"printf hello"},"tool_response":{"output":"hello","exit_code":0}}`
	var stdout, stderr bytes.Buffer
	if code := cli.Run(ctx, []string{"hook", "codex"}, strings.NewReader(event), &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("hook result: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	journal, err := engine.ConversationJournal(ctx, view.Works[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.ProviderSessionID != "codex-session" || len(journal.Tools) != 1 || string(journal.Tools[0].Output) != `{"exit_code":0,"output":"hello"}` {
		t.Fatalf("journal = %+v", journal)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("daemon exit: %v", err)
	}
}

func TestCursorHookMCPPolicyOnlyAllowsPikaServer(t *testing.T) {
	t.Setenv("PIKA_GO_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv("PIKA_SESSION_ID", "session")
	for _, test := range []struct {
		server string
		want   string
	}{
		{server: "pika_go", want: `{"permission":"allow"}` + "\n"},
		{server: "foreign", want: `{"agent_message":"Pika only allows its Session-scoped MCP server.","permission":"deny","user_message":"Pika blocked a non-Pika MCP server for this Session."}` + "\n"},
	} {
		var stdout, stderr bytes.Buffer
		event := fmt.Sprintf(`{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"beforeMCPExecution","mcp_server_name":%q,"tool_name":"tool","tool_input":"{}"}`, test.server)
		if code := cli.Run(context.Background(), []string{"hook", "cursor"}, strings.NewReader(event), &stdout, &stderr); code != 0 {
			t.Fatalf("hook exit = %d", code)
		}
		if stdout.String() != test.want || stderr.Len() != 0 {
			t.Fatalf("server=%s stdout=%q stderr=%q", test.server, stdout.String(), stderr.String())
		}
	}
}

func TestCursorBeforeSubmitHookContinues(t *testing.T) {
	t.Setenv("PIKA_GO_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv("PIKA_SESSION_ID", "session")
	var stdout, stderr bytes.Buffer
	event := `{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"beforeSubmitPrompt","prompt":"measure it"}`
	if code := cli.Run(context.Background(), []string{"hook", "cursor"}, strings.NewReader(event), &stdout, &stderr); code != 0 {
		t.Fatalf("hook exit = %d", code)
	}
	if stdout.String() != `{"continue":true}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCursorSessionStartHookInjectsFrozenSystemPrompt(t *testing.T) {
	t.Setenv("PIKA_GO_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv("PIKA_SESSION_ID", "session")
	promptPath := filepath.Join(t.TempDir(), "system-prompt.md")
	if err := os.WriteFile(promptPath, []byte("frozen dynamic system context"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIKA_CURSOR_SYSTEM_PROMPT_PATH", promptPath)
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"hook", "cursor", "sessionStart"}, strings.NewReader(`{"session_id":"conversation"}`), &stdout, &stderr); code != 0 {
		t.Fatalf("hook exit = %d", code)
	}
	if stdout.String() != `{"additional_context":"frozen dynamic system context"}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCursorHookAcceptsCamelCaseEnvelope(t *testing.T) {
	t.Setenv("PIKA_GO_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv("PIKA_SESSION_ID", "session")
	var stdout, stderr bytes.Buffer
	event := `{"conversationId":"conversation","generationId":"generation","hookEventName":"beforeMCPExecution","mcpServerName":"pika_go","toolName":"tool","toolInput":{}}`
	if code := cli.Run(context.Background(), []string{"hook", "cursor"}, strings.NewReader(event), &stdout, &stderr); code != 0 {
		t.Fatalf("hook exit = %d", code)
	}
	if stdout.String() != `{"permission":"allow"}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCursorProviderEventLargerThan16MiBReachesJournalByteForByte(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	session := symphony.AgentSession{ID: "cursor-session", WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "cursor", AgentName: "cursor-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-large-hook-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(serveCtx, daemon.Config{SocketPath: socketPath, Version: "test", InstanceID: "optimization", Symphony: engine, IngestProviderEvent: engine.IngestProviderEvent})
	}()
	waitForHealth(t, socketPath, &bytes.Buffer{})
	payload, err := json.Marshal(map[string]any{
		"conversation_id": "conversation", "generation_id": "generation", "hook_event_name": "afterShellExecution",
		"command": "benchmark", "output": strings.Repeat("x", 17<<20), "duration": 1, "sandbox": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := control.IngestProviderEvent(ctx, socketPath, "cursor", protocol.ProviderEventRequest{AgentSessionID: session.ID, Event: payload}); err != nil {
		t.Fatal(err)
	}
	journal, err := engine.ConversationJournal(ctx, view.Works[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Events) != 1 || !bytes.Equal(journal.Events[0].Raw, payload) || len(journal.ToolSupplements) != 1 || len(journal.ToolSupplements[0].Output) <= 16<<20 {
		t.Fatalf("large journal payload was truncated: events=%d supplements=%d raw=%d output=%d", len(journal.Events), len(journal.ToolSupplements), len(journal.Events[0].Raw), len(journal.ToolSupplements[0].Output))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLargeProviderRouteDoesNotRemoveControlPlaneBodyLimit(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-control-limit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(serveCtx, daemon.Config{SocketPath: socketPath, Version: "test", InstanceID: "optimization", Symphony: engine})
	}()
	waitForHealth(t, socketPath, &bytes.Buffer{})
	configuration := strings.Repeat("x", (1<<20)+1)
	_, err = control.Init(ctx, socketPath, protocol.InitRequest{
		Mutation:          protocol.Mutation{RequestID: "large-init"},
		Repository:        "/repo",
		ConfigurationTOML: &configuration,
	})
	if !control.IsHTTPStatus(err, 400) {
		t.Fatalf("large control request error = %v, want HTTP 400", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func waitForStatus(t *testing.T, socketPath string, daemonStderr *bytes.Buffer) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var stdout bytes.Buffer
		if cli.Run(context.Background(), []string{"status", "--socket", socketPath, "--json"}, nil, &stdout, &bytes.Buffer{}) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon did not become healthy; stderr = %q", daemonStderr.String())
}

func makeWorkspaceWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		mode := os.FileMode(0o600)
		if info.IsDir() {
			mode = 0o700
		}
		return os.Chmod(path, mode)
	})
}

func waitForHealth(t *testing.T, socketPath string, daemonStderr *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := control.Health(context.Background(), socketPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon did not become healthy; stderr = %q", daemonStderr.String())
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("path did not appear: %s", path)
}
