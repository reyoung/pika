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

func TestRuntimeSendAgentKeysTargetsDurableAgentName(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pika-herdr-keys-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
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
			Params struct {
				Target string   `json:"target"`
				Keys   []string `json:"keys"`
			} `json:"params"`
		}
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			serverDone <- err
			return
		}
		if request.Method != "agent.send_keys" || request.Params.Target != "pika-session" || len(request.Params.Keys) != 1 || request.Params.Keys[0] != "ctrl+c" {
			serverDone <- &fixtureError{"request", request.Method + ":" + request.Params.Target}
			return
		}
		serverDone <- json.NewEncoder(connection).Encode(map[string]any{"id": request.ID, "result": map[string]any{}})
	}()
	if err := herdr.NewRuntime(herdr.NewClient(socketPath)).SendAgentKeys(context.Background(), "pika-session", []string{"ctrl+c"}); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
