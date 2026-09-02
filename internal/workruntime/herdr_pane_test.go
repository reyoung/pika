package workruntime_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/workruntime"
)

type herdrRPCStep struct {
	method string
	result map[string]any
	apiErr *herdr.APIError
	drop   bool
}

func startHerdrRPCScript(t *testing.T, steps []herdrRPCStep) (string, <-chan error) {
	t.Helper()
	directory := t.TempDir()
	listener, err := net.Listen("unix", filepath.Join(directory, "herdr.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		for _, step := range steps {
			connection, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			line, err := bufio.NewReader(connection).ReadBytes('\n')
			if err != nil {
				_ = connection.Close()
				done <- err
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(line, &request); err != nil {
				_ = connection.Close()
				done <- err
				return
			}
			if request.Method != step.method {
				_ = connection.Close()
				done <- &fixtureMethodError{want: step.method, got: request.Method}
				return
			}
			if step.drop {
				_ = connection.Close()
				continue
			}
			response := map[string]any{"id": request.ID, "result": step.result}
			if step.apiErr != nil {
				delete(response, "result")
				response["error"] = step.apiErr
			}
			err = json.NewEncoder(connection).Encode(response)
			_ = connection.Close()
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	return listener.Addr().String(), done
}

func TestHerdrRuntimeClassifiesLaunchSubmissionBoundaryAndCleansOwnedPane(t *testing.T) {
	t.Run("deterministic pre-RPC failure", func(t *testing.T) {
		socket, done := startHerdrRPCScript(t, []herdrRPCStep{
			{method: "pane.split", result: map[string]any{"pane": map[string]any{"pane_id": "w1:p9"}}},
			{method: "pane.rename", apiErr: &herdr.APIError{Code: "rename_failed", Message: "injected"}},
			{method: "pane.close", result: map[string]any{}},
		})
		submitted := false
		runtime := workruntime.NewHerdrRuntime(herdr.NewClient(socket), "w1:p1")
		_, err := runtime.Start(context.Background(), workruntime.StartSpec{
			AgentName: "pika-test", AgentKind: "codex", Repository: "/repo", PaneLabel: "Test",
			BeforeSubmit: func(context.Context) error { submitted = true; return nil },
		})
		if err == nil || !workruntime.LaunchDefinitelyNotSubmitted(err) || submitted {
			t.Fatalf("pre-RPC outcome submitted=%v err=%v", submitted, err)
		}
		if scriptErr := <-done; scriptErr != nil {
			t.Fatal(scriptErr)
		}
	})

	t.Run("post-RPC response loss", func(t *testing.T) {
		socket, done := startHerdrRPCScript(t, []herdrRPCStep{
			{method: "pane.split", result: map[string]any{"pane": map[string]any{"pane_id": "w1:p9"}}},
			{method: "pane.rename", result: map[string]any{}},
			{method: "agent.start", drop: true},
		})
		submitted := false
		runtime := workruntime.NewHerdrRuntime(herdr.NewClient(socket), "w1:p1")
		_, err := runtime.Start(context.Background(), workruntime.StartSpec{
			AgentName: "pika-test", AgentKind: "codex", Repository: "/repo", PaneLabel: "Test",
			BeforeSubmit: func(context.Context) error { submitted = true; return nil },
		})
		if err == nil || workruntime.LaunchDefinitelyNotSubmitted(err) || !submitted {
			t.Fatalf("post-RPC outcome submitted=%v err=%v", submitted, err)
		}
		if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "read Herdr response") {
			t.Fatalf("unexpected response-loss error: %v", err)
		}
		if scriptErr := <-done; scriptErr != nil {
			t.Fatal(scriptErr)
		}
	})
}

func TestHerdrRuntimeRetriesTransientBusyPreferredPane(t *testing.T) {
	repository := t.TempDir()
	socket, done := startHerdrRPCScript(t, []herdrRPCStep{
		{method: "pane.rename", result: map[string]any{}},
		{method: "pane.send_input", result: map[string]any{}},
		{method: "pane.get", result: map[string]any{"pane": map[string]any{
			"pane_id": "w1:p1", "foreground_cwd": repository,
		}}},
		{method: "agent.start", apiErr: &herdr.APIError{Code: "agent_pane_busy", Message: "pane is not an available shell"}},
		{method: "agent.start", result: map[string]any{"agent": map[string]any{
			"name": "pika-baseline", "agent": "codex", "agent_status": "working",
			"interactive_ready": false, "pane_id": "w1:p1", "terminal_id": "term-1",
			"workspace_id": "w1", "tab_id": "w1:t1",
		}}},
	})
	submissions := 0
	runtime := workruntime.NewHerdrRuntime(herdr.NewClient(socket), "w1:p2")
	runtime.AgentStartBusyRetryDelays = []time.Duration{0}
	observation, err := runtime.Start(context.Background(), workruntime.StartSpec{
		AgentName: "pika-baseline", AgentKind: "codex", PreferredPaneID: "w1:p1",
		Repository: repository, PaneLabel: "Baseline", ReturnOnLaunch: true,
		BeforeSubmit: func(context.Context) error { submissions++; return nil },
	})
	if err != nil {
		t.Fatalf("start agent after transient pane busy response: %v", err)
	}
	if observation.PaneID != "w1:p1" || submissions != 1 {
		t.Fatalf("observation=%+v submissions=%d", observation, submissions)
	}
	if scriptErr := <-done; scriptErr != nil {
		t.Fatal(scriptErr)
	}
}

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

func TestHerdrRuntimeCreatesDedicatedTab(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pika-herdr-tab-")
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
		for _, expected := range []string{"pane.get", "tab.create", "pane.rename", "agent.start"} {
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
			if expected == "tab.create" && (request.Params["workspace_id"] != "w1" || request.Params["cwd"] != repository || request.Params["label"] != "Iteration R2 · attempt" || request.Params["focus"] != false) {
				_ = connection.Close()
				done <- &fixtureMethodError{want: "background Iteration tab in w1", got: string(line)}
				return
			}
			if expected == "pane.rename" && (request.Params["pane_id"] != "w1:p8" || request.Params["label"] != "Iteration R2 · attempt") {
				_ = connection.Close()
				done <- &fixtureMethodError{want: "Iteration R2 · attempt", got: string(line)}
				return
			}
			result := map[string]any{}
			switch expected {
			case "pane.get":
				result["pane"] = map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "w1:t1"}
			case "tab.create":
				result["root_pane"] = map[string]any{"pane_id": "w1:p8", "workspace_id": "w1", "tab_id": "w1:t2"}
			case "pane.rename":
				result["pane"] = map[string]any{"pane_id": "w1:p8", "label": "Iteration R2 · attempt"}
			case "agent.start":
				result["agent"] = map[string]any{
					"name": "pika-iteration", "agent": "codex", "agent_status": "working",
					"interactive_ready": false, "pane_id": "w1:p8", "terminal_id": "term-8",
					"workspace_id": "w1", "tab_id": "w1:t2",
				}
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
		AgentName: "pika-iteration", AgentKind: "codex", Repository: repository,
		DedicatedTab: true, TabLabel: "Iteration R2 · attempt", PaneLabel: "Iteration R2 · attempt", ReturnOnLaunch: true,
	})
	if err != nil {
		t.Fatalf("start agent in dedicated tab: %v; Herdr request: %v", err, <-done)
	}
	if observation.PaneID != "w1:p8" || observation.TabID != "w1:t2" {
		t.Fatalf("observation = %+v", observation)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
