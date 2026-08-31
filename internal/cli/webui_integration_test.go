package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/cli"
	"github.com/reyoung/pika-go/internal/daemon"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/workbench"
)

func TestWebUICommandAttachesToRealDaemonReadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := newCommittedRepository(t)
	workspace, err := optimizationworkspace.Create(ctx, filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	var verificationWork string
	for _, work := range view.Works {
		if work.Role == symphony.RoleBaselineVerification && work.Status == symphony.WorkPending {
			verificationWork = work.ID
		}
	}
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "accept"}, WorkID: verificationWork, Decision: symphony.VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: "best-0"}); err != nil {
		t.Fatal(err)
	}

	socketDirectory, err := os.MkdirTemp("/tmp", "pika-webui-real-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "daemon.sock")
	daemonContext, stopDaemon := context.WithCancel(context.Background())
	daemonDone := make(chan error, 1)
	go func() {
		daemonDone <- daemon.Serve(daemonContext, daemon.Config{SocketPath: socketPath, Version: "test", InstanceID: "optimization", Symphony: engine, Workbench: workbench.New(engine, nil)})
	}()
	waitForHealth(t, socketPath, &bytes.Buffer{})
	if err := workspace.WriteHerdrBinding(optimizationworkspace.HerdrBinding{WorkspaceID: "w1", TabID: "t1", ControlPane: "p1", DaemonPane: "p2", DaemonSocket: socketPath, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	_ = probe.Close()
	webContext, stopWeb := context.WithCancel(context.Background())
	webDone := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		webDone <- cli.Run(webContext, []string{"webui", "--workspace", workspace.Root, "--listen", address}, nil, &stdout, &stderr)
	}()
	baseURL := "http://" + address
	time.Sleep(100 * time.Millisecond)
	select {
	case code := <-webDone:
		t.Fatalf("webui exited before readiness: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	default:
	}
	waitHTTP(t, baseURL+"/", http.StatusOK)
	tokenBytes, err := os.ReadFile(filepath.Join(workspace.RuntimeRoot, "webui", "token"))
	if err != nil {
		t.Fatal(err)
	}
	token := string(bytes.TrimSpace(tokenBytes))
	if strings.Contains(stdout.String(), token) {
		t.Fatal("WebUI startup output exposed the bearer token")
	}
	if tokenPath := filepath.Join(workspace.RuntimeRoot, "webui", "token"); !strings.Contains(stdout.String(), tokenPath) {
		t.Fatalf("WebUI startup output did not identify token path: %q", stdout.String())
	}
	request, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/workbench/snapshot", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodGet, baseURL+"/api/v1/workbench/snapshot?terminal_attempt_limit=25", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot workbench.Snapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || len(snapshot.Nodes) == 0 || snapshot.Version.DomainRevision == 0 {
		t.Fatalf("snapshot status=%d value=%+v", response.StatusCode, snapshot)
	}

	stopWeb()
	if code := <-webDone; code != 0 {
		t.Fatalf("webui exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(workspace.RuntimeRoot, "webui", "pid")); !os.IsNotExist(err) {
		t.Fatalf("WebUI PID file remained: %v", err)
	}
	if connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("WebUI listener remained after shutdown")
	}
	stopDaemon()
	if err := <-daemonDone; err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Inspect(ctx, symphony.Status{}); err != nil {
		t.Fatalf("daemon domain did not remain healthy: %v", err)
	}
	_ = engine.Close()
}

func waitHTTP(t *testing.T, url string, status int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == status {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("HTTP endpoint %s did not reach %d", url, status)
}
