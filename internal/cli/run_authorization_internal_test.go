package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
)

func TestDaemonReauthorizesGenerationAfterAcquiringInstanceLock(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "--quiet"}, {"config", "user.name", "Pika Test"}, {"config", "user.email", "pika@example.invalid"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "README.md"}, {"commit", "--quiet", "-m", "fixture"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(root, "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := daemonupdate.RestoreCurrent(workspace.Root, daemonupdate.Generation{
		Path:    "/retained/current/pika-go",
		Digest:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Version: "before", ActivatedAt: "2026-09-01T13:00:00Z",
	}); err != nil {
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
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = snapshot{contents: contents, modTime: info.ModTime()}
	}

	reached := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, daemonAuthorizationBarrierContextKey{}, daemonAuthorizationBarrier{
		reached: reached, release: release,
	})
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runDaemon(ctx, []string{
			"--workspace", workspace.Root,
			"--socket", filepath.Join(root, "daemon.sock"),
		}, &stderr)
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatalf("daemon did not reach authorization/lock barrier: %v stderr=%s", ctx.Err(), stderr.String())
	}
	if err := maintenance.NewFileStore(workspace.Root).Write(maintenance.Status{
		ID: "maintenance-after-check", RequestID: "prepare-after-check", State: maintenance.StateReady,
		FromGeneration: maintenance.Generation{Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		ToGeneration:   maintenance.Generation{Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		Targets:        []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
		StartedAt:      "2026-09-01T13:00:00Z", ReadyAt: "2026-09-01T13:01:00Z", UpdatedAt: "2026-09-01T13:01:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if code := <-done; code != 1 || !strings.Contains(stderr.String(), "maintenance.generation_rejected") {
		t.Fatalf("daemon rejection code=%d stderr=%s", code, stderr.String())
	}
	for _, path := range protected {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(contents, before[path].contents) || !info.ModTime().Equal(before[path].modTime) {
			t.Fatalf("post-lock generation rejection changed %s", path)
		}
	}
}

func TestDaemonRepeatsFullMaintenanceAdmissionAfterLockWait(t *testing.T) {
	for _, variant := range []string{"state transition", "active set drift"} {
		t.Run(variant, func(t *testing.T) {
			fixture := newPostLockAdmissionFixture(t)
			reached := make(chan struct{})
			release := make(chan struct{})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, daemonAuthorizationBarrierContextKey{}, daemonAuthorizationBarrier{
				reached: reached, release: release,
			})
			var stderr bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- runDaemon(ctx, []string{
					"--workspace", fixture.workspace.Root,
					"--socket", fixture.daemonSocket,
				}, &stderr)
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatalf("candidate did not pass pre-lock admission: %v stderr=%s", ctx.Err(), stderr.String())
			}

			switch variant {
			case "state transition":
				advanced := fixture.status
				advanced.State = maintenance.StateHolding
				advanced.HoldingGeneration = advanced.ToGeneration
				advanced.HoldingAt = "2026-09-01T14:02:00Z"
				advanced.UpdatedAt = advanced.HoldingAt
				if err := maintenance.NewFileStore(fixture.workspace.Root).Write(advanced); err != nil {
					t.Fatal(err)
				}
			case "active set drift":
				engine, err := symphony.Open(context.Background(), fixture.workspace.DatabasePath, symphony.Options{})
				if err != nil {
					t.Fatal(err)
				}
				if err := engine.MarkAgentSessionEnded(context.Background(), fixture.target.SessionID, symphony.AgentSessionExited); err != nil {
					_ = engine.Close()
					t.Fatal(err)
				}
				if err := engine.Close(); err != nil {
					t.Fatal(err)
				}
				snapshot := fixture.herdr.snapshotValue()
				snapshot.Agents = nil
				fixture.herdr.setSnapshot(snapshot)
			}
			protected := snapshotAdmissionFiles(t, fixture.workspace)
			close(release)
			select {
			case code := <-done:
				if code != 1 || (!strings.Contains(stderr.String(), "maintenance.admission_changed") &&
					!strings.Contains(stderr.String(), "maintenance.herdr_admission_failed")) {
					t.Fatalf("post-lock rejection code=%d stderr=%s", code, stderr.String())
				}
			case <-time.After(2 * time.Second):
				cancel()
				<-done
				t.Fatal("candidate opened the database using stale pre-lock maintenance admission")
			}
			assertAdmissionFilesUnchanged(t, fixture.workspace, protected)
		})
	}
}

type postLockAdmissionFixture struct {
	workspace    optimizationworkspace.Workspace
	daemonSocket string
	status       maintenance.Status
	target       maintenance.Target
	herdr        *authorizationHerdrServer
}

func newPostLockAdmissionFixture(t *testing.T) postLockAdmissionFixture {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "--quiet"}, {"config", "user.name", "Pika Test"}, {"config", "user.email", "pika@example.invalid"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "README.md"}, {"commit", "--quiet", "-m", "fixture"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(root, "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(context.Background(), workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(context.Background(), symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: workspace.Identity.ID,
		Repository: workspace.Identity.SourceRepository,
	}); err != nil {
		_ = engine.Close()
		t.Fatal(err)
	}
	view, err := engine.Inspect(context.Background(), symphony.Status{})
	if err != nil {
		_ = engine.Close()
		t.Fatal(err)
	}
	session := symphony.AgentSession{
		ID: "frozen-session", WorkID: view.Works[0].ID, Generation: view.Works[0].Generation,
		Role: view.Works[0].Role, AgentKind: "codex", AgentName: "pika-frozen-session",
		Status: symphony.AgentSessionRunning,
	}
	if err := engine.EnsureAgentSession(context.Background(), session); err != nil {
		_ = engine.Close()
		t.Fatal(err)
	}
	binding := symphony.PaneBinding{
		WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p3", TerminalID: "terminal-agent",
	}
	if err := engine.BindPane(context.Background(), session.ID, binding); err != nil {
		_ = engine.Close()
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	daemonSocket := filepath.Join(root, "pika.sock")
	herdrServer := newAuthorizationHerdrServer(t, filepath.Join(root, "herdr.sock"))
	agentName, agentKind := session.AgentName, session.AgentKind
	herdrServer.setSnapshot(herdr.Snapshot{
		Version: "test", Protocol: herdr.MinimumProtocol,
		Workspaces: []herdr.Workspace{{WorkspaceID: binding.WorkspaceID}},
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: binding.WorkspaceID, TabID: binding.TabID},
			{PaneID: "w1:p2", WorkspaceID: binding.WorkspaceID, TabID: binding.TabID},
			{PaneID: binding.PaneID, TerminalID: binding.TerminalID, WorkspaceID: binding.WorkspaceID, TabID: binding.TabID},
		},
		Agents: []herdr.Agent{{
			Name: &agentName, Agent: &agentKind, WorkspaceID: binding.WorkspaceID, TabID: binding.TabID,
			PaneID: binding.PaneID, TerminalID: binding.TerminalID,
		}},
	})
	if err := workspace.WriteHerdrBinding(optimizationworkspace.HerdrBinding{
		SocketPath: herdrServer.socketPath, WorkspaceID: binding.WorkspaceID, TabID: binding.TabID,
		ControlPane: "w1:p1", DaemonPane: "w1:p2", DaemonSocket: daemonSocket,
		UpdatedAt: "2026-09-01T14:00:00Z",
	}); err != nil {
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
	target := maintenance.Target{
		WorkID: session.WorkID, SessionID: session.ID, WorkGeneration: session.Generation,
		AgentName: session.AgentName, AgentKind: session.AgentKind,
		WorkspaceID: binding.WorkspaceID, TabID: binding.TabID, PaneID: binding.PaneID, TerminalID: binding.TerminalID,
	}
	status := maintenance.Status{
		ID: "maintenance", RequestID: "prepare", State: maintenance.StateReady,
		FromGeneration: maintenance.Generation{Digest: strings.Repeat("a", 64), Version: "bridge"},
		ToGeneration:   maintenance.Generation{Digest: digest, Version: Version},
		Targets:        []maintenance.Target{target}, StartedAt: "2026-09-01T14:00:00Z",
		ReadyAt: "2026-09-01T14:01:00Z", UpdatedAt: "2026-09-01T14:01:00Z",
	}
	if err := maintenance.NewFileStore(workspace.Root).Write(status); err != nil {
		t.Fatal(err)
	}
	if err := daemonupdate.RestoreCurrent(workspace.Root, daemonupdate.Generation{
		Path: executable, Digest: digest, Version: Version, ActivatedAt: "2026-09-01T14:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_SOCKET_PATH", herdrServer.socketPath)
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	return postLockAdmissionFixture{
		workspace: workspace, daemonSocket: daemonSocket, status: status, target: target, herdr: herdrServer,
	}
}

type authorizationHerdrServer struct {
	socketPath string
	listener   net.Listener
	mu         sync.Mutex
	snapshot   herdr.Snapshot
}

func newAuthorizationHerdrServer(t *testing.T, socketPath string) *authorizationHerdrServer {
	t.Helper()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &authorizationHerdrServer{socketPath: socketPath, listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				var request struct {
					ID     any    `json:"id"`
					Method string `json:"method"`
				}
				if json.NewDecoder(connection).Decode(&request) != nil {
					return
				}
				if request.Method != "session.snapshot" {
					return
				}
				_ = json.NewEncoder(connection).Encode(map[string]any{
					"id": request.ID, "result": map[string]any{"snapshot": server.snapshotValue()},
				})
			}()
		}
	}()
	return server
}

func (server *authorizationHerdrServer) setSnapshot(snapshot herdr.Snapshot) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.snapshot = snapshot
}

func (server *authorizationHerdrServer) snapshotValue() herdr.Snapshot {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.snapshot
}

type admissionFileSnapshot struct {
	contents []byte
	modTime  time.Time
}

func snapshotAdmissionFiles(t *testing.T, workspace optimizationworkspace.Workspace) map[string]admissionFileSnapshot {
	t.Helper()
	paths := []string{
		workspace.DatabasePath,
		filepath.Join(workspace.RuntimeRoot, "daemon", "current.json"),
		workspace.HerdrBindingPath,
		filepath.Join(workspace.RuntimeRoot, "daemon", "maintenance.json"),
	}
	result := make(map[string]admissionFileSnapshot, len(paths))
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		result[path] = admissionFileSnapshot{contents: contents, modTime: info.ModTime()}
	}
	return result
}

func assertAdmissionFilesUnchanged(
	t *testing.T,
	workspace optimizationworkspace.Workspace,
	before map[string]admissionFileSnapshot,
) {
	t.Helper()
	for path, expected := range before {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(contents, expected.contents) || !info.ModTime().Equal(expected.modTime) {
			t.Fatalf("post-lock maintenance rejection changed %s", path)
		}
	}
}
