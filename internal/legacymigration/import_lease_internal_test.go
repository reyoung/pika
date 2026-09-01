package legacymigration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
)

func TestImportOwnsExclusiveLeaseThroughRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repository := newImportLeaseRepository(t)
	roots := Roots{
		Config: filepath.Join(t.TempDir(), "config"),
		State:  filepath.Join(t.TempDir(), "state"),
	}
	const instanceID = "exclusive-import"
	configDir := filepath.Join(roots.Config, "instances", instanceID)
	stateDir := filepath.Join(roots.State, "instances", instanceID)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "config.toml"),
		[]byte(configuration.RenderDefaults(repository)),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "pika.db"), []byte("not a sqlite database\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	workspaceRoot := filepath.Join(t.TempDir(), "workspace")
	options := ImportOptions{Roots: roots, InstanceID: instanceID, WorkspaceRoot: workspaceRoot}
	created := make(chan struct{})
	continueImport := make(chan struct{})
	importDone := make(chan error, 1)
	go func() {
		_, err := importWithLeaseAfterCreate(ctx, options, nil, func(optimizationworkspace.Workspace) {
			close(created)
			<-continueImport
		})
		importDone <- err
	}()

	select {
	case <-created:
	case <-ctx.Done():
		t.Fatal("import did not reach the post-create barrier")
	}

	type writerObservation struct {
		lease           *optimizationworkspace.MutationLease
		workspaceExists bool
		err             error
	}
	writerEntered := make(chan writerObservation, 1)
	go func() {
		lease, err := optimizationworkspace.AcquireSharedMutationLease(ctx, workspaceRoot)
		_, statErr := os.Lstat(workspaceRoot)
		writerEntered <- writerObservation{
			lease:           lease,
			workspaceExists: statErr == nil,
			err:             err,
		}
	}()

	select {
	case observation := <-writerEntered:
		if observation.lease != nil {
			_ = observation.lease.Close()
		}
		t.Fatalf("shared writer entered before import settled: %+v", observation)
	case <-time.After(100 * time.Millisecond):
	}

	close(continueImport)
	select {
	case err := <-importDone:
		if err == nil {
			t.Fatal("corrupt legacy import succeeded")
		}
	case <-ctx.Done():
		t.Fatal("import did not fail and roll back")
	}

	select {
	case observation := <-writerEntered:
		if observation.err != nil {
			t.Fatalf("shared writer failed after import settled: %v", observation.err)
		}
		if observation.workspaceExists {
			t.Fatal("shared writer entered before failed import removed the claimed Workspace")
		}
		if observation.lease == nil {
			t.Fatal("shared writer entered without a lease")
		}
		if err := observation.lease.Close(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shared writer did not enter after import rollback")
	}
	if _, err := os.Lstat(workspaceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed import left Workspace root: %v", err)
	}
}

func newImportLeaseRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Pika Test"},
		{"config", "user.email", "pika@example.invalid"},
	} {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "kernel.go"), []byte("package kernel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "kernel.go"}, {"commit", "--quiet", "-m", "initial"}} {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
