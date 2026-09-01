package maintenance_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/maintenance"
)

const storeDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const storeToDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestFileStoreWritesProtectedAtomicState(t *testing.T) {
	t.Parallel()
	workspace := filepath.Join(t.TempDir(), "workspace")
	store := maintenance.NewFileStore(workspace)
	want := maintenance.Status{
		ID: "maintenance-1", RequestID: "request-1", State: maintenance.StateQuiescing,
		FromGeneration: maintenance.Generation{Digest: storeDigest}, ToGeneration: maintenance.Generation{Digest: storeToDigest},
		Targets:   []maintenance.Target{{WorkID: "work-1", SessionID: "session-1", WorkGeneration: 3}},
		StartedAt: "2026-09-01T11:00:00Z", UpdatedAt: "2026-09-01T11:00:00Z",
	}
	if err := store.Write(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Targets[0] != want.Targets[0] {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	info, err := os.Stat(filepath.Join(workspace, "runtime", "daemon", "maintenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("maintenance state mode = %o", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(workspace, "runtime", "daemon", ".maintenance-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain after atomic commit: %v", matches)
	}
}

func TestFileStoreRejectsSecretBearingMalformedTrailingAndStateDependentRecords(t *testing.T) {
	t.Parallel()
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	for _, status := range []maintenance.Status{
		{ID: "", RequestID: "request", State: maintenance.StateQuiescing, FromGeneration: maintenance.Generation{Digest: storeDigest}, ToGeneration: maintenance.Generation{Digest: storeToDigest}},
		{ID: "id", RequestID: "request", State: "unknown", FromGeneration: maintenance.Generation{Digest: storeDigest}, ToGeneration: maintenance.Generation{Digest: storeToDigest}},
		{ID: "id", RequestID: "request", State: maintenance.StateFailed, FromGeneration: maintenance.Generation{Digest: storeDigest}, ToGeneration: maintenance.Generation{Digest: storeToDigest}, Failure: "credential=secret"},
		{ID: "id", RequestID: "request", State: maintenance.StateFailed, FromGeneration: maintenance.Generation{Digest: storeDigest}, ToGeneration: maintenance.Generation{Digest: storeToDigest}},
		{ID: "id", RequestID: "request", State: maintenance.StateHolding, FromGeneration: maintenance.Generation{Digest: storeDigest}, ToGeneration: maintenance.Generation{Digest: storeToDigest}, HoldingAt: "now"},
		{ID: "id", RequestID: "request", State: maintenance.StateQuiescing, FromGeneration: maintenance.Generation{Digest: "short"}, ToGeneration: maintenance.Generation{Digest: storeToDigest}},
	} {
		if err := store.Write(status); err == nil {
			t.Fatalf("malformed or secret-bearing status was accepted: %+v", status)
		}
	}
	valid := maintenance.Status{
		ID: "id", RequestID: "request", State: maintenance.StateQuiescing,
		FromGeneration: maintenance.Generation{Digest: storeDigest}, ToGeneration: maintenance.Generation{Digest: storeToDigest},
		StartedAt: "now", UpdatedAt: "now",
	}
	if err := store.Write(valid); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(store.Path()), "maintenance.json")
	if store.Path() != "" {
		path = store.Path()
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(contents, []byte("{\"extra\":true}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
	if err := os.WriteFile(path, append(contents, []byte("garbage\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); err == nil {
		t.Fatal("trailing non-JSON garbage was accepted")
	}
}
