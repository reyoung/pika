package workruntime_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/workruntime"
)

func TestHerdrRuntimePreparesPreferredPaneInAssignedRepository(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pika-herdr-pane-name-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	listener, err := net.Listen("unix", filepath.Join(directory, "herdr.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan error, 1)
	repository := filepath.Join(directory, "code")
	go func() {
		for _, expected := range []string{"pane.rename", "pane.send_input", "pane.get", "agent.start"} {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			line, readErr := bufio.NewReader(connection).ReadBytes('\n')
			if readErr != nil {
				_ = connection.Close()
				done <- readErr
				return
			}
			var request struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if unmarshalErr := json.Unmarshal(line, &request); unmarshalErr != nil {
				_ = connection.Close()
				done <- unmarshalErr
				return
			}
			if request.Method != expected {
				_ = connection.Close()
				done <- &fixtureMethodError{want: expected, got: request.Method}
				return
			}
			if expected == "pane.rename" && (request.Params["pane_id"] != "w1:p7" || request.Params["label"] != "Iteration R2 · p7") {
				_ = connection.Close()
				done <- &fixtureMethodError{want: "Iteration R2 · p7", got: string(line)}
				return
			}
			if expected == "pane.send_input" && (request.Params["pane_id"] != "w1:p7" || request.Params["text"] != "cd -- '"+repository+"'") {
				_ = connection.Close()
				done <- &fixtureMethodError{want: "change pane cwd to " + repository, got: string(line)}
				return
			}
			result := map[string]any{}
			if expected == "agent.start" {
				result["agent"] = map[string]any{
					"name": "pika-iteration", "agent": "codex", "agent_status": "working",
					"interactive_ready": false, "pane_id": "w1:p7", "terminal_id": "term-7",
					"workspace_id": "w1", "tab_id": "w1:t1",
				}
			} else if expected == "pane.get" {
				result["pane"] = map[string]any{"pane_id": "w1:p7", "foreground_cwd": repository}
			} else if expected == "pane.rename" {
				result["pane"] = map[string]any{"pane_id": "w1:p7", "label": "Iteration R2 · p7"}
			}
			encodeErr := json.NewEncoder(connection).Encode(map[string]any{"id": request.ID, "result": result})
			_ = connection.Close()
			if encodeErr != nil {
				done <- encodeErr
				return
			}
		}
		done <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runtime := workruntime.NewHerdrRuntime(herdr.NewClient(listener.Addr().String()), "w1:p1")
	observation, err := runtime.Start(ctx, workruntime.StartSpec{
		AgentName: "pika-iteration", AgentKind: "codex", PreferredPaneID: "w1:p7",
		Repository: repository, PaneLabel: "Iteration R2", PaneLabelNeedsID: true, ReturnOnLaunch: true,
	})
	if err != nil {
		t.Fatalf("start agent in assigned repository: %v; Herdr request: %v", err, <-done)
	}
	if observation.PaneID != "w1:p7" {
		t.Fatalf("observation = %+v", observation)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
