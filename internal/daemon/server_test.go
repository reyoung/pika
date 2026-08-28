package daemon_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemon"
	"github.com/reyoung/pika-go/internal/daemonupdate"
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
