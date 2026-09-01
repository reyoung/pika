package evidence_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/evidence"
)

func TestReadStableRejectsPathAndSymlinkEscapes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"secret":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.json")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside.json", outside, "escape.json"} {
		if _, _, err := evidence.ReadStable(root, path, 1024); err == nil {
			t.Fatalf("unsafe path %q was accepted", path)
		}
	}
}

func TestReadStableReturnsNormalizedDigestMetadata(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evidence", "baseline.json"), []byte(`{"metric":"latency"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	contents, artifact, err := evidence.ReadStable(root, "evidence/../evidence/baseline.json", 1024)
	if err != nil {
		t.Fatalf("read stable evidence: %v", err)
	}
	if string(contents) != `{"metric":"latency"}` || artifact.RelativePath != "evidence/baseline.json" || artifact.ByteSize != int64(len(contents)) || len(artifact.ContentSHA256) != 64 {
		t.Fatalf("contents=%q artifact=%+v", contents, artifact)
	}
}

func TestReadStableAcceptsAbsolutePathWithinWorkRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	evidencePath := filepath.Join(root, "evidence", "baseline.json")
	if err := os.Mkdir(filepath.Dir(evidencePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, []byte(`{"metric":"latency"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	contents, artifact, err := evidence.ReadStable(root, evidencePath, 1024)
	if err != nil {
		t.Fatalf("read stable absolute evidence: %v", err)
	}
	if string(contents) != `{"metric":"latency"}` || artifact.RelativePath != "evidence/baseline.json" {
		t.Fatalf("contents=%q artifact=%+v", contents, artifact)
	}
}

func TestEnsureWorkRootOwnsCanonicalProtectedPaths(t *testing.T) {
	t.Parallel()
	base := filepath.Join(t.TempDir(), "evidence")
	root, err := evidence.EnsureWorkRoot(base, evidence.IterationScope, "work-1")
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(base, "iterations", "work-1") {
		t.Fatalf("Work evidence root = %q", root)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("Work evidence root mode = %v, err=%v", info, err)
	}
	for _, workID := range []string{"", ".", "..", "../escape", "nested/work", `nested\work`, "work id", "work:1"} {
		if _, err := evidence.EnsureWorkRoot(base, evidence.IterationScope, workID); err == nil {
			t.Fatalf("unsafe Work ID %q was accepted", workID)
		}
	}
}
