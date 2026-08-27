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

func TestHerdrRuntimeResubmitsIdleCursorPrompt(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pika-herdr-cursor-prompt-")
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
	go func() {
		for _, expected := range []string{"agent.prompt", "pane.send_keys", "agent.get"} {
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
			if expected == "pane.send_keys" {
				keys, _ := request.Params["keys"].([]any)
				if len(keys) != 1 || keys[0] != "enter" {
					_ = connection.Close()
					done <- &fixtureMethodError{want: "enter key", got: string(line)}
					return
				}
			}
			result := map[string]any{}
			if expected != "pane.send_keys" {
				status := "idle"
				if expected == "agent.get" {
					status = "working"
				}
				result["agent"] = map[string]any{"agent": "cursor", "agent_status": status, "pane_id": "w1:p2"}
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
	if err := runtime.PromptForProvider(ctx, "w1:p2", "continue", "cursor"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type fixtureMethodError struct {
	want string
	got  string
}

func (e *fixtureMethodError) Error() string { return "want " + e.want + ", got " + e.got }
