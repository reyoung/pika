package symphony

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestFlowVersionFreezesLegacyAndV2Initialization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	legacy, err := Open(ctx, filepath.Join(t.TempDir(), "legacy.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if _, err := legacy.Apply(ctx, Init{Meta: CommandMeta{RequestID: "legacy-init"}, OptimizationID: "legacy", Repository: "/repo"}); err != nil {
		t.Fatalf("initialize flow v1 compatibility workspace: %v", err)
	}
	legacyView, err := legacy.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	if legacyView.Optimization.FlowVersion != FlowVersion1 || legacyView.SkillSnapshot != nil {
		t.Fatalf("legacy flow projection = %+v", legacyView.Optimization)
	}

	v2, err := Open(ctx, filepath.Join(t.TempDir(), "v2.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v2.Close() })
	snapshot := testSkillSnapshot(t)
	if _, err := v2.Apply(ctx, Init{Meta: CommandMeta{RequestID: "v2-init"}, OptimizationID: "v2", Repository: "/repo", FlowVersion: FlowVersion2, SkillSnapshot: &snapshot}); err != nil {
		t.Fatalf("initialize flow v2 workspace: %v", err)
	}
	v2View, err := v2.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	if v2View.Optimization.FlowVersion != FlowVersion2 || v2View.SkillSnapshot == nil || v2View.SkillSnapshot.SnapshotID != snapshot.SnapshotID {
		t.Fatalf("flow v2 projection = %+v", v2View)
	}
	if got := v2View.SkillSnapshot.Entries; len(got) != 2 || got[0] != snapshot.Entries[0] || got[1] != snapshot.Entries[1] {
		t.Fatalf("frozen skill entries = %+v", got)
	}
}

func TestFlowV2InitializationRejectsIncompleteSnapshotWithoutStartingBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	_, err = engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "v2", Repository: "/repo", FlowVersion: FlowVersion2})
	assertFlowDomainCode(t, err, CodeInvalidCommand)
	if _, err := engine.Inspect(ctx, Status{}); err == nil {
		t.Fatal("failed snapshot acquisition created baseline Work")
	}
}

func TestFlowV2SnapshotRequiresCanonicalIdentityAndPaths(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*SkillSnapshotInput){
		func(snapshot *SkillSnapshotInput) { snapshot.SnapshotID = "short" },
		func(snapshot *SkillSnapshotInput) { snapshot.Entries[0].RelativePath = "skills/not-KernelWiki" },
		func(snapshot *SkillSnapshotInput) { snapshot.Entries[1].RelativePath = "skills/not-ncu" },
	} {
		snapshot := testSkillSnapshot(t)
		mutate(&snapshot)
		if err := validateSkillSnapshotInput(&snapshot); err == nil {
			t.Fatal("flow-v2 snapshot validation accepted non-canonical identity or path")
		}
	}
}

func TestInitRejectsUnsupportedFlowVersion(t *testing.T) {
	t.Parallel()
	engine, err := Open(context.Background(), filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	_, err = engine.Apply(context.Background(), Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "future", Repository: "/repo", FlowVersion: 3})
	assertFlowDomainCode(t, err, CodeInvalidCommand)
}

func testSkillSnapshot(t *testing.T) SkillSnapshotInput {
	t.Helper()
	manifest, err := json.Marshal(map[string]any{"schema_version": 1, "skills": []string{"KernelWiki", "ncu-report-skill"}})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	return SkillSnapshotInput{
		SchemaVersion:  SkillSnapshotSchemaV1,
		SnapshotID:     "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		RootPath:       filepath.Join(t.TempDir(), "toolkits", "snapshot"),
		Manifest:       manifest,
		ManifestSHA256: hex.EncodeToString(digest[:]),
		Entries: []SkillSnapshotEntry{
			{Name: "KernelWiki", Repository: KernelWikiRepository, Branch: KernelWikiBranch, CommitSHA: "1111111111111111111111111111111111111111", RelativePath: "skills/KernelWiki", ContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{Name: "ncu-report-skill", Repository: NCUReportSkillRepository, Branch: NCUReportSkillBranch, CommitSHA: "2222222222222222222222222222222222222222", RelativePath: "skills/ncu-report-skill", ContentSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}
}

func assertFlowDomainCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	var domainErr *DomainError
	if !errors.As(err, &domainErr) || domainErr.Code != want {
		t.Fatalf("error = %v, want domain code %s", err, want)
	}
}
