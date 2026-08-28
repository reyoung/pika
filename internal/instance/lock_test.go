package instance_test

import (
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/instance"
)

func TestSharedLockRemainsHeldUntilSuccessorCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	owner, err := instance.AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	inheritedFile, err := owner.Share()
	if err != nil {
		t.Fatal(err)
	}
	successor, err := instance.AdoptLock(inheritedFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if contender, err := instance.AcquireLock(path); err == nil {
		_ = contender.Close()
		t.Fatal("lock became available while the successor still owned it")
	}
	if err := successor.Close(); err != nil {
		t.Fatal(err)
	}
	contender, err := instance.AcquireLock(path)
	if err != nil {
		t.Fatalf("lock was not released after the final inherited descriptor closed: %v", err)
	}
	_ = contender.Close()
}
