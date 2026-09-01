package optimizationworkspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateCleansPostClaimFailureAndRetrySucceeds(t *testing.T) {
	repository := createFailureRepository(t)
	root := filepath.Join(t.TempDir(), "workspace")
	injected := errors.New("post-claim failure")
	_, err := createWithAfterClaim(context.Background(), root, repository, true, func(workspace Workspace) error {
		if _, statErr := os.Stat(workspace.Root); statErr != nil {
			t.Fatalf("claimed root: %v", statErr)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("create error = %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed claim remains: %v", err)
	}
	if workspace, err := Create(context.Background(), root, repository); err != nil {
		t.Fatalf("retry Create: %v", err)
	} else if err := workspace.ValidateLayout(); err != nil {
		t.Fatalf("retry layout: %v", err)
	}
}

func TestCreatePostClaimCleanupNeverTouchesPreexistingDestination(t *testing.T) {
	repository := createFailureRepository(t)
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o751); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "owner")
	if err := os.WriteFile(marker, []byte("preexisting\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := createWithAfterClaim(context.Background(), root, repository, true, func(Workspace) error {
		called = true
		return errors.New("must not run")
	})
	if !errors.Is(err, ErrAlreadyExists) || called {
		t.Fatalf("create error=%v afterClaim=%t", err, called)
	}
	info, statErr := os.Stat(root)
	contents, readErr := os.ReadFile(marker)
	if statErr != nil || readErr != nil || info.Mode().Perm() != 0o751 || string(contents) != "preexisting\n" {
		t.Fatalf("preexisting destination changed: info=%v contents=%q stat=%v read=%v", info, contents, statErr, readErr)
	}
}

func TestCreateHoldsExclusiveLeaseThroughPostClaimRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repository := createFailureRepository(t)
	root := filepath.Join(t.TempDir(), "workspace")
	writerEntered := make(chan *MutationLease, 1)
	injected := errors.New("rollback after claim")
	_, err := createWithAfterClaim(ctx, root, repository, true, func(Workspace) error {
		go func() {
			lease, leaseErr := AcquireSharedMutationLease(ctx, root)
			if leaseErr == nil {
				writerEntered <- lease
			}
		}()
		select {
		case lease := <-writerEntered:
			_ = lease.Close()
			t.Fatal("writer entered while Create owned its post-claim lease")
		case <-time.After(100 * time.Millisecond):
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("create error=%v", err)
	}
	select {
	case lease := <-writerEntered:
		defer lease.Close()
	case <-ctx.Done():
		t.Fatal("writer did not enter after rollback released the exclusive lease")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left Workspace root: %v", err)
	}
}

func TestCreateRollsBackClaimWhenRootProtectionFails(t *testing.T) {
	repository := createFailureRepository(t)
	root := filepath.Join(t.TempDir(), "workspace")
	injected := errors.New("chmod failed")
	_, err := createWithLeaseAfterClaimOps(
		context.Background(), root, repository, true, nil, nil,
		createClaimOps{
			chmod: func(string, os.FileMode) error { return injected },
			lstat: os.Lstat,
		},
	)
	if !errors.Is(err, injected) {
		t.Fatalf("create error=%v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed root protection stranded claim: %v", err)
	}
	if _, err := Create(context.Background(), root, repository); err != nil {
		t.Fatalf("retry after root-protection failure: %v", err)
	}
}

func TestCreateRollsBackClaimWhenClaimIdentityReadFails(t *testing.T) {
	repository := createFailureRepository(t)
	root := filepath.Join(t.TempDir(), "workspace")
	injected := errors.New("lstat failed")
	_, err := createWithLeaseAfterClaimOps(
		context.Background(), root, repository, true, nil, nil,
		createClaimOps{
			chmod: os.Chmod,
			lstat: func(string) (os.FileInfo, error) { return nil, injected },
		},
	)
	if !errors.Is(err, injected) {
		t.Fatalf("create error=%v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed claim identity read stranded root: %v", err)
	}
	if _, err := Create(context.Background(), root, repository); err != nil {
		t.Fatalf("retry after claim-identity failure: %v", err)
	}
}

func createFailureRepository(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runCreateFailureGit(t, root, "init")
	runCreateFailureGit(t, root, "config", "user.name", "Pika Test")
	runCreateFailureGit(t, root, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCreateFailureGit(t, root, "add", "README.md")
	runCreateFailureGit(t, root, "commit", "-m", "fixture")
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func runCreateFailureGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
