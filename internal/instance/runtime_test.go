package instance_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/instance"
)

func TestResolveSocketContextUsesPublishedHerdrInstance(t *testing.T) {
	t.Setenv("PIKA_GO_SOCKET", "")
	socketDir, err := os.MkdirTemp("/tmp", "pika-instance-discovery-")
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
	t.Setenv("HERDR_SOCKET_PATH", herdrSocket)
	t.Setenv("HERDR_PANE_ID", "w42:p7")
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		var request struct {
			ID     string            `json:"id"`
			Method string            `json:"method"`
			Params map[string]string `json:"params"`
		}
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			serverDone <- err
			return
		}
		if request.Method != "workspace.get" || request.Params["workspace_id"] != "w42" {
			serverDone <- &unexpectedDiscoveryRequest{method: request.Method, workspaceID: request.Params["workspace_id"]}
			return
		}
		serverDone <- json.NewEncoder(connection).Encode(map[string]any{
			"id": request.ID,
			"result": map[string]any{"workspace": map[string]any{
				"workspace_id": "w42", "tokens": map[string]string{"pika_instance": "published-instance"},
			}},
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := instance.ResolveSocketContext(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	want, err := instance.SocketForInstance("published-instance")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved socket = %q, want metadata-derived %q", got, want)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

type unexpectedDiscoveryRequest struct {
	method      string
	workspaceID string
}

func (e *unexpectedDiscoveryRequest) Error() string {
	return "unexpected Herdr discovery request: " + e.method + " " + e.workspaceID
}

func TestResolveRuntimeBuildsPerInstanceLayout(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", "")
	t.Setenv("PIKA_GO_INSTANCE", "")
	root := t.TempDir()
	paths, err := instance.ResolveRuntime(instance.RuntimeOptions{
		SocketPath: filepath.Join(root, "daemon.sock"),
		ConfigRoot: filepath.Join(root, "config"),
		StateRoot:  filepath.Join(root, "state"),
		InstanceID: "instance-1",
	})
	if err != nil {
		t.Fatalf("resolve runtime: %v", err)
	}
	if paths.DatabasePath != filepath.Join(root, "state", "instances", "instance-1", "pika.db") {
		t.Fatalf("database path = %q", paths.DatabasePath)
	}
	if paths.LockPath != filepath.Join(root, "state", "instances", "instance-1", "daemon.lock") {
		t.Fatalf("lock path = %q", paths.LockPath)
	}
	if paths.ContextsRoot != filepath.Join(root, "state", "instances", "instance-1", "contexts") {
		t.Fatalf("contexts root = %q", paths.ContextsRoot)
	}
}

func TestSocketForHerdrWorkspaceMatchesEnvironmentDiscovery(t *testing.T) {
	t.Setenv("PIKA_GO_SOCKET", "")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr-kick-off.sock")
	t.Setenv("HERDR_PANE_ID", "w17:p4")

	fromEnvironment, err := instance.ResolveSocket("")
	if err != nil {
		t.Fatal(err)
	}
	fromWorkspace, err := instance.SocketForHerdrWorkspace("/tmp/herdr-kick-off.sock", "w17")
	if err != nil {
		t.Fatal(err)
	}
	if fromWorkspace != fromEnvironment {
		t.Fatalf("workspace socket = %q, environment socket = %q", fromWorkspace, fromEnvironment)
	}
}

func TestResolveRuntimeRequiresConfigAndStateTogether(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", "")
	_, err := instance.ResolveRuntime(instance.RuntimeOptions{SocketPath: filepath.Join(t.TempDir(), "daemon.sock"), StateRoot: filepath.Join(t.TempDir(), "state")})
	if err == nil {
		t.Fatal("state-only runtime was accepted")
	}
}
