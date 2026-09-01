package daemon_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemon"
	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/protocol"
)

func TestAfterListenCanReachDaemonBeforeStartupEffectsRun(t *testing.T) {
	t.Parallel()

	directory, err := os.MkdirTemp("/tmp", "pika-daemon-listen-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "pika.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hookResult := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(ctx, daemon.Config{
			SocketPath: socketPath,
			Version:    "test",
			AfterListen: func(hookCtx context.Context) error {
				healthCtx, stopHealth := context.WithTimeout(hookCtx, time.Second)
				defer stopHealth()
				_, err := control.Health(healthCtx, socketPath)
				hookResult <- err
				cancel()
				return err
			},
		})
	}()

	select {
	case err := <-hookResult:
		if err != nil {
			t.Fatalf("daemon was not reachable from AfterListen: %v", err)
		}
	case err := <-done:
		t.Fatalf("daemon exited before AfterListen: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("AfterListen was not called")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestMaintenanceReadyKeepsSocketUntilTerminalMCPResponseCompletes(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pika-daemon-maintenance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "pika.sock")
	releaseResponse := make(chan struct{})
	requestStarted := make(chan struct{})
	process := &fakeMaintenance{}
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(context.Background(), daemon.Config{
			SocketPath: socketPath, Version: "test", Maintenance: process,
			MCPHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(requestStarted)
				process.ready.Store(true)
				<-releaseResponse
				w.WriteHeader(http.StatusOK)
			}),
		})
	}()
	waitForHealth(t, socketPath)
	if _, err := control.MaintenancePrepare(context.Background(), socketPath, protocol.MaintenancePrepareRequest{
		RequestID: "prepare-1", ToGeneration: protocol.MaintenanceGeneration{Digest: "target"},
	}); err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, "http://pika-go/mcp", nil)
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}}}
		response, requestErr := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- requestErr
	}()
	<-requestStarted
	select {
	case err := <-done:
		t.Fatalf("daemon exited before terminal MCP response: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("socket disappeared before terminal response: %v", err)
	}
	close(releaseResponse)
	if err := <-requestDone; err != nil {
		t.Fatalf("terminal MCP response failed: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("maintenance-ready daemon did not exit")
	}
}

func TestMaintenanceReadyForceClosesHalfOpenRequestWithinBound(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pika-daemon-maintenance-half-open-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "pika.sock")
	process := &fakeMaintenance{}
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(context.Background(), daemon.Config{
			SocketPath: socketPath, Version: "test", Maintenance: process,
		})
	}()
	waitForHealth(t, socketPath)
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(
		"POST /v1/maintenance/prepare HTTP/1.1\r\n" +
			"Host: pika-go\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{",
	)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	process.ready.Store(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("maintenance-ready daemon did not force-close half-open request")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("maintenance-ready daemon left socket behind: %v", err)
	}
	status, ready, err := process.Poll(context.Background())
	if err != nil || !ready || status.State != maintenance.StateReady {
		t.Fatalf("maintenance state was not preserved as Ready: status=%+v ready=%t err=%v", status, ready, err)
	}
}

type fakeMaintenance struct {
	ready atomic.Bool
}

func (f *fakeMaintenance) Prepare(context.Context, maintenance.PrepareRequest) (maintenance.Status, error) {
	return maintenance.Status{ID: "maintenance-1", RequestID: "prepare-1", State: maintenance.StateQuiescing}, nil
}

func (f *fakeMaintenance) Status() (maintenance.Status, error) {
	return maintenance.Status{ID: "maintenance-1", RequestID: "prepare-1", State: maintenance.StateQuiescing}, nil
}

func (f *fakeMaintenance) Resume(context.Context, maintenance.ResumeRequest) (maintenance.Status, error) {
	return maintenance.Status{ID: "maintenance-1", RequestID: "prepare-1", State: maintenance.StateResumed}, nil
}

func (f *fakeMaintenance) Poll(context.Context) (maintenance.Status, bool, error) {
	if f.ready.Load() {
		return maintenance.Status{ID: "maintenance-1", RequestID: "prepare-1", State: maintenance.StateReady}, true, nil
	}
	return maintenance.Status{ID: "maintenance-1", RequestID: "prepare-1", State: maintenance.StateQuiescing}, false, nil
}

func waitForHealth(t *testing.T, socketPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, err := control.Health(ctx, socketPath); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("daemon did not become healthy")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestUpdateHandoffForceClosesLongRunningRequest(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pika-daemon-update-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	socketPath := filepath.Join(directory, "pika.sock")
	requestStarted := make(chan struct{})
	requestStopped := make(chan struct{})
	updateAccepted := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- daemon.Serve(context.Background(), daemon.Config{
			SocketPath: socketPath,
			Version:    "test",
			MCPHandler: http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				close(requestStarted)
				<-request.Context().Done()
				close(requestStopped)
			}),
			PrepareUpdate: func(context.Context, daemonupdate.Candidate) (daemonupdate.Status, error) {
				return daemonupdate.Status{}, nil
			},
			UpdateAccepted: updateAccepted,
		})
	}()

	healthCtx, stopHealth := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopHealth()
	for {
		if _, err := control.Health(healthCtx, socketPath); err == nil {
			break
		}
		select {
		case <-healthCtx.Done():
			t.Fatalf("daemon did not become healthy: %v", healthCtx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}

	requestResult := make(chan error, 1)
	go func() {
		request, err := http.NewRequest(http.MethodPost, "http://pika-go/mcp", nil)
		if err != nil {
			requestResult <- err
			return
		}
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}}}
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("long-running request did not start")
	}

	updateCtx, stopUpdate := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopUpdate()
	if _, err := control.Update(updateCtx, socketPath, daemonupdate.Candidate{}); err != nil {
		t.Fatalf("accept update: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, daemon.ErrHandoff) {
			t.Fatalf("Serve() error = %v, want %v", err, daemon.ErrHandoff)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("daemon did not hand off after the graceful shutdown window")
	}
	select {
	case <-requestStopped:
	case <-time.After(time.Second):
		t.Fatal("long-running request context was not canceled")
	}
	select {
	case <-requestResult:
	case <-time.After(time.Second):
		t.Fatal("long-running request client did not return")
	}
}
