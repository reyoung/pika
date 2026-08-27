package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemon"
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
