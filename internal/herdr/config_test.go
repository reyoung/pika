package herdr_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/herdr"
)

func TestValidateFreshSessionConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		contents string
		wantErr  string
	}{
		{name: "disabled", contents: "[session]\nresume_agents_on_restore = false\n"},
		{name: "enabled", contents: "[session]\nresume_agents_on_restore = true\n", wantErr: "must be false"},
		{name: "missing", contents: "[session]\nscrollback = true\n", wantErr: "must explicitly contain"},
		{name: "invalid", contents: "[session]\nresume_agents_on_restore = maybe\n", wantErr: "must be false"},
		{name: "wrong-section", contents: "resume_agents_on_restore = false\n", wantErr: "must explicitly contain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			err := herdr.ValidateFreshSessionConfiguration(path)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestResolveConfigPathHonorsHerdrOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.toml")
	t.Setenv("HERDR_CONFIG_PATH", path)
	if got, err := herdr.ResolveConfigPath(); err != nil || got != path {
		t.Fatalf("path=%q err=%v", got, err)
	}
}

func TestRequireFreshSessionsReloadsValidatedConfiguration(t *testing.T) {
	t.Parallel()
	directory, err := os.MkdirTemp("/tmp", "pika-herdr-config-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(configPath, []byte("[session]\nresume_agents_on_restore = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(directory, "herdr.sock"))
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
		line, readErr := bufio.NewReader(connection).ReadBytes('\n')
		if readErr != nil {
			serverDone <- readErr
			return
		}
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if unmarshalErr := json.Unmarshal(line, &request); unmarshalErr != nil {
			serverDone <- unmarshalErr
			return
		}
		if request.Method != "server.reload_config" {
			serverDone <- &fixtureError{"method", request.Method}
			return
		}
		_, writeErr := connection.Write([]byte(`{"id":"` + request.ID + `","result":{"status":"applied","diagnostics":[]}}` + "\n"))
		serverDone <- writeErr
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := herdr.RequireFreshSessions(ctx, herdr.NewClient(listener.Addr().String()), configPath); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
