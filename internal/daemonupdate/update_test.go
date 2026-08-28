package daemonupdate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/daemonupdate"
)

func TestProbeRejectsIncompatibleHotUpdate(t *testing.T) {
	current := daemonupdate.CurrentProbe("old")
	candidate := current
	candidate.Version = "new"
	if err := candidate.ValidateCompatible(current); err != nil {
		t.Fatal(err)
	}
	candidate.SQLiteSchema++
	if err := candidate.ValidateCompatible(current); err == nil {
		t.Fatal("schema-changing candidate was accepted")
	}
}

func TestCurrentAndUpdateStatusAreAtomicWorkspaceState(t *testing.T) {
	root := t.TempDir()
	candidate := daemonupdate.Candidate{Path: filepath.Join(root, "runtime", "daemon", "generations", "digest", "pika-go"), Digest: "digest", Probe: daemonupdate.CurrentProbe("new")}
	generation, err := daemonupdate.CommitCurrent(root, candidate, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	read, err := daemonupdate.ReadCurrent(root)
	if err != nil || read.Digest != generation.Digest || read.Version != "new" {
		t.Fatalf("current generation = %+v, err=%v", read, err)
	}
	status := daemonupdate.NewStatus("update-1", generation, candidate)
	status.State = daemonupdate.StateCommitted
	if err := daemonupdate.WriteStatus(root, status); err != nil {
		t.Fatal(err)
	}
	readStatus, err := daemonupdate.ReadStatus(root)
	if err != nil || readStatus.ID != "update-1" || readStatus.State != daemonupdate.StateCommitted {
		t.Fatalf("update status = %+v, err=%v", readStatus, err)
	}
	info, err := os.Stat(filepath.Join(root, "runtime", "daemon", "update.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("update state permissions = %v", info.Mode().Perm())
	}
}

func TestValidateCandidateRejectsPathOutsideGenerationStore(t *testing.T) {
	root := t.TempDir()
	err := daemonupdate.ValidateCandidate(root, daemonupdate.Candidate{Path: "/tmp/pika-go", Digest: "digest", Probe: daemonupdate.CurrentProbe("new")}, daemonupdate.CurrentProbe("old"))
	if err == nil {
		t.Fatal("outside candidate was accepted")
	}
}

func TestStageRejectsNonExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pika-go")
	if err := os.WriteFile(path, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := daemonupdate.Stage(context.Background(), t.TempDir(), path); err == nil {
		t.Fatal("non-executable candidate was staged")
	}
}
