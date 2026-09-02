package optimizationworkspace_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/optimizationworkspace"
)

func TestMutationLeaseCoordinatesAcrossProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	ready := filepath.Join(t.TempDir(), "ready")
	release := filepath.Join(t.TempDir(), "release")
	command := exec.Command(os.Args[0], "-test.run=^TestMutationLeaseHelperProcess$")
	command.Env = append(os.Environ(),
		"PIKA_GO_MUTATION_LEASE_HELPER=1",
		"PIKA_GO_MUTATION_LEASE_ROOT="+root,
		"PIKA_GO_MUTATION_LEASE_READY="+ready,
		"PIKA_GO_MUTATION_LEASE_RELEASE="+release,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		_ = os.WriteFile(release, nil, 0o600)
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("helper process did not acquire shared lease")
		}
		time.Sleep(10 * time.Millisecond)
	}
	blockedCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if lease, err := optimizationworkspace.AcquireExclusiveMutationLease(blockedCtx, root); !errors.Is(err, context.DeadlineExceeded) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("exclusive lease crossed helper process: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	finished = true
	lease, err := optimizationworkspace.AcquireExclusiveMutationLease(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMutationLeaseHelperProcess(t *testing.T) {
	if os.Getenv("PIKA_GO_MUTATION_LEASE_HELPER") != "1" {
		return
	}
	lease, err := optimizationworkspace.AcquireSharedMutationLease(context.Background(), os.Getenv("PIKA_GO_MUTATION_LEASE_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.WriteFile(os.Getenv("PIKA_GO_MUTATION_LEASE_READY"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(os.Getenv("PIKA_GO_MUTATION_LEASE_RELEASE")); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMutationLeaseSerializesMaintenanceActivationAgainstWriters(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	writer, err := optimizationworkspace.AcquireSharedMutationLease(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *optimizationworkspace.MutationLease, 1)
	go func() {
		lease, acquireErr := optimizationworkspace.AcquireExclusiveMutationLease(context.Background(), root)
		if acquireErr != nil {
			acquired <- nil
			return
		}
		acquired <- lease
	}()
	select {
	case lease := <-acquired:
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatal("maintenance activation crossed an active writer lease")
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "writer-committed"), []byte("after-check\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case lease := <-acquired:
		if lease == nil {
			t.Fatal("exclusive maintenance lease failed")
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance activation did not acquire after writer")
	}
}

func TestMutationLeaseBlocksWriterUntilMaintenanceStateIsDurable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	maintenanceLease, err := optimizationworkspace.AcquireExclusiveMutationLease(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *optimizationworkspace.MutationLease, 1)
	go func() {
		lease, _ := optimizationworkspace.AcquireSharedMutationLease(context.Background(), root)
		acquired <- lease
	}()
	select {
	case lease := <-acquired:
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatal("writer crossed an active maintenance lease")
	case <-time.After(100 * time.Millisecond):
	}
	if err := maintenanceLease.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case lease := <-acquired:
		if lease == nil {
			t.Fatal("shared writer lease failed")
		}
		_ = lease.Close()
	case <-time.After(time.Second):
		t.Fatal("writer did not acquire after maintenance activation")
	}
}

func TestMutationLeaseDowngradeAllowsWritersAndStillBlocksMaintenance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	creator, err := optimizationworkspace.AcquireExclusiveMutationLease(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = creator.Close() })

	if err := creator.DowngradeToShared(); err != nil {
		t.Fatal(err)
	}
	writer, err := optimizationworkspace.AcquireSharedMutationLease(context.Background(), root)
	if err != nil {
		t.Fatalf("shared writer did not cross downgraded creation lease: %v", err)
	}
	defer writer.Close()

	maintenanceCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	maintenance, err := optimizationworkspace.AcquireExclusiveMutationLease(maintenanceCtx, root)
	if maintenance != nil {
		_ = maintenance.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusive maintenance lease crossed downgraded creation lease: %v", err)
	}
}
