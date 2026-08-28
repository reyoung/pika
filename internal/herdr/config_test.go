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

func TestConfigureFreshSessionsUpdatesExistingSettingAndReloads(t *testing.T) {
	for _, test := range []struct {
		name     string
		original string
		want     string
	}{
		{
			name:     "replace existing value",
			original: "[ui]\nsidebar = true\n\n[session]\nresume_agents_on_restore = true # keep this explanation\n\n[remote]\nconnect_timeout_seconds = 10\n",
			want:     "[ui]\nsidebar = true\n\n[session]\nresume_agents_on_restore = false # keep this explanation\n\n[remote]\nconnect_timeout_seconds = 10\n",
		},
		{
			name:     "append missing session section",
			original: "[ui]\nsidebar = true\n",
			want:     "[ui]\nsidebar = true\n\n[session]\nresume_agents_on_restore = false\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "pika-herdr-configure-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			configPath := filepath.Join(directory, "config.toml")
			if err := os.WriteFile(configPath, []byte(test.original), 0o640); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(directory, "herdr.sock"))
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
				if request.Method != "server.reload_config" {
					done <- &fixtureError{"method", request.Method}
					return
				}
				done <- json.NewEncoder(connection).Encode(map[string]any{
					"id": request.ID, "result": map[string]any{"status": "applied", "diagnostics": []string{}},
				})
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := herdr.ConfigureFreshSessions(ctx, herdr.NewClient(listener.Addr().String()), configPath); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != test.want {
				t.Fatalf("configured contents:\n%s\nwant:\n%s", contents, test.want)
			}
			info, err := os.Stat(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o640 {
				t.Fatalf("configured mode=%v", info.Mode().Perm())
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfigureFreshSessionsRestoresOriginalWhenReloadFails(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pika-herdr-configure-rollback-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configPath := filepath.Join(directory, "config.toml")
	original := "[session]\nresume_agents_on_restore = true\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(directory, "herdr.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		for index := range 2 {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
				_ = connection.Close()
				done <- decodeErr
				return
			}
			if request.Method != "server.reload_config" {
				_ = connection.Close()
				done <- &fixtureError{"method", request.Method}
				return
			}
			status := "applied"
			diagnostics := []string{}
			if index == 0 {
				status = "failed"
				diagnostics = []string{"fixture rejected reload"}
			}
			encodeErr := json.NewEncoder(connection).Encode(map[string]any{
				"id": request.ID, "result": map[string]any{"status": status, "diagnostics": diagnostics},
			})
			_ = connection.Close()
			if encodeErr != nil {
				done <- encodeErr
				return
			}
		}
		done <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = herdr.ConfigureFreshSessions(ctx, herdr.NewClient(listener.Addr().String()), configPath)
	if err == nil || !strings.Contains(err.Error(), "original Herdr configuration restored") {
		t.Fatalf("configure error = %v", err)
	}
	contents, readErr := os.ReadFile(configPath)
	if readErr != nil || string(contents) != original {
		t.Fatalf("restored contents=%q err=%v", contents, readErr)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
