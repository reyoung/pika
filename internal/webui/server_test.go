package webui_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/webui"
)

func TestHandlerAuthenticatesOnlyReadOnlyWorkbenchAPI(t *testing.T) {
	t.Parallel()
	directory, err := os.MkdirTemp("/tmp", "pika-webui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/workbench/snapshot" {
			t.Errorf("forwarded %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"snapshot_version":{"api_version":1}}`))
	})}
	go daemon.Serve(listener)
	t.Cleanup(func() { _ = daemon.Shutdown(context.Background()) })

	handler, err := webui.NewHandler(socket, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/workbench/snapshot", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/workbench/snapshot?terminal_attempt_limit=25", nil)
	request.Header.Set("Authorization", "Bearer secret-token")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), `"api_version":1`) {
		t.Fatalf("authorized response = %d %s", authorized.Code, authorized.Body.String())
	}

	mutation := httptest.NewRequest(http.MethodPost, "/api/v1/scheduler/pause", nil)
	mutation.Header.Set("Authorization", "Bearer secret-token")
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, mutation)
	if blocked.Code != http.StatusMethodNotAllowed {
		t.Fatalf("mutation status = %d", blocked.Code)
	}
}

func TestEnsureTokenPersistsAndRotatesWithPrivatePermissions(t *testing.T) {
	t.Parallel()
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	first, path, err := webui.EnsureToken(runtimeRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := webui.EnsureToken(runtimeRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second != first {
		t.Fatalf("persistent tokens = %q %q", first, second)
	}
	rotated, _, err := webui.EnsureToken(runtimeRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	if rotated == first {
		t.Fatal("token did not rotate")
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("permissions file=%o dir=%o", fileInfo.Mode().Perm(), directoryInfo.Mode().Perm())
	}
}
