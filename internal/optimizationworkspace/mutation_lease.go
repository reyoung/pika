package optimizationworkspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type MutationLease struct {
	file *os.File
	root string
	mode int
}

func AcquireSharedMutationLease(ctx context.Context, root string) (*MutationLease, error) {
	return acquireMutationLease(ctx, root, syscall.LOCK_SH)
}

func AcquireExclusiveMutationLease(ctx context.Context, root string) (*MutationLease, error) {
	return acquireMutationLease(ctx, root, syscall.LOCK_EX)
}

func acquireMutationLease(ctx context.Context, root string, mode int) (*MutationLease, error) {
	resolved, err := canonicalPath(root)
	if err != nil {
		return nil, fmt.Errorf("resolve Workspace mutation lease: %w", err)
	}
	parent := filepath.Dir(resolved)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create Workspace lease parent: %w", err)
	}
	path := filepath.Join(parent, "."+filepath.Base(resolved)+".pika-mutation.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Workspace mutation lease: %w", err)
	}
	for {
		if err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB); err == nil {
			return &MutationLease{file: file, root: resolved, mode: mode}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock Workspace mutation lease: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (lease *MutationLease) ownsExclusive(root string) bool {
	if lease == nil || lease.file == nil || lease.mode != syscall.LOCK_EX {
		return false
	}
	resolved, err := canonicalPath(root)
	return err == nil && resolved == lease.root
}

// DowngradeToShared keeps the lease held while allowing other normal Workspace
// processes to acquire shared leases. Callers must reauthorize their mutation
// after this returns because a waiting exclusive lease may win the conversion.
func (lease *MutationLease) DowngradeToShared() error {
	if lease == nil || lease.file == nil {
		return errors.New("workspace mutation lease is not held")
	}
	if lease.mode == syscall.LOCK_SH {
		return nil
	}
	if lease.mode != syscall.LOCK_EX {
		return errors.New("workspace mutation lease has an unsupported mode")
	}
	if err := syscall.Flock(int(lease.file.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("downgrade Workspace mutation lease: %w", err)
	}
	lease.mode = syscall.LOCK_SH
	return nil
}

func (lease *MutationLease) Close() error {
	if lease == nil || lease.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lease.file.Fd()), syscall.LOCK_UN)
	closeErr := lease.file.Close()
	lease.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
