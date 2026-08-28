package herdr_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/herdr"
)

func TestRuntimeStartCanReturnBeforeInteractiveReadiness(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pika-herdr-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
			serverDone <- decodeErr
			return
		}
		if request.Method != "agent.start" {
			serverDone <- &fixtureError{"method", request.Method}
			return
		}
		response := map[string]any{
			"id": request.ID,
			"result": map[string]any{"agent": map[string]any{
				"name": "pika-cursor", "agent": "cursor", "agent_status": "working",
				"interactive_ready": false, "pane_id": "w1:p2", "terminal_id": "term-2",
				"workspace_id": "w1", "tab_id": "w1:t1",
			}},
		}
		serverDone <- json.NewEncoder(connection).Encode(response)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	agent, err := herdr.NewRuntime(herdr.NewClient(socketPath)).Start(ctx, herdr.StartSpec{
		Name: "pika-cursor", Kind: "cursor", PaneID: "w1:p2", TimeoutMS: 50, ReturnOnLaunch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.InteractiveReady || agent.AgentStatus != "working" || agent.PaneID != "w1:p2" {
		t.Fatalf("launch observation = %+v", agent)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("fixture server: %v", err)
	}
}

func TestRuntimeStartTreatsBlockedObservationAsLaunched(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pika-herdr-runtime-blocked-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		for call := 0; call < 2; call++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverDone <- acceptErr
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
				_ = connection.Close()
				serverDone <- decodeErr
				return
			}
			wantMethod := []string{"agent.start", "agent.get"}[call]
			if request.Method != wantMethod {
				_ = connection.Close()
				serverDone <- &fixtureError{"method", request.Method}
				return
			}
			status := "working"
			if call == 1 {
				status = "blocked"
			}
			response := map[string]any{"id": request.ID, "result": map[string]any{"agent": map[string]any{
				"name": "pika-codex", "agent": "codex", "agent_status": status,
				"interactive_ready": false, "pane_id": "w1:p2", "terminal_id": "term-2",
				"workspace_id": "w1", "tab_id": "w1:t1",
			}}}
			encodeErr := json.NewEncoder(connection).Encode(response)
			_ = connection.Close()
			if encodeErr != nil {
				serverDone <- encodeErr
				return
			}
		}
		serverDone <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	agent, err := herdr.NewRuntime(herdr.NewClient(socketPath)).Start(ctx, herdr.StartSpec{
		Name: "pika-codex", Kind: "codex", PaneID: "w1:p2", TimeoutMS: 100,
	})
	if err != nil {
		t.Fatalf("blocked live agent was rejected as a startup failure: %v", err)
	}
	if agent.AgentStatus != "blocked" || agent.InteractiveReady || agent.PaneID != "w1:p2" {
		t.Fatalf("blocked launch observation = %+v", agent)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("fixture server: %v", err)
	}
}
