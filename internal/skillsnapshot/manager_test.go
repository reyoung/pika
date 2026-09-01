package skillsnapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/symphony"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPreparePublishesValidatedImmutableSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := t.TempDir()
	makeCleanupWritable(t, workspace)
	kernel := createSkillRepository(t, "master", "KernelWiki", "kernel-v1")
	ncu := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	if err := os.WriteFile(filepath.Join(kernel, "executable.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	commitAll(t, kernel, "add executable skill helper")
	manager := Manager{sources: []source{
		{Name: "KernelWiki", Repository: kernel, Branch: "master"},
		{Name: "ncu-report-skill", Repository: ncu, Branch: "main"},
	}, Now: func() time.Time { return time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC) }}

	snapshot, err := manager.Prepare(ctx, Request{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("prepare snapshot: %v", err)
	}
	if len(snapshot.Input.Entries) != 2 || !strings.HasPrefix(snapshot.Input.RootPath, filepath.Join(workspace, "toolkits", "kda-skills")+string(filepath.Separator)) {
		t.Fatalf("snapshot = %+v", snapshot.Input)
	}
	if info, err := os.Stat(filepath.Join(snapshot.Input.RootPath, "skills", "KernelWiki", "SKILL.md")); err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("published skill mode: info=%v err=%v", info, err)
	}
	if info, err := os.Stat(filepath.Join(snapshot.Input.RootPath, "skills", "KernelWiki", "executable.sh")); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("published executable mode: info=%v err=%v", info, err)
	}
	plugin, err := os.ReadFile(filepath.Join(snapshot.Input.RootPath, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(plugin, &decoded); err != nil || decoded["name"] != "pika-kda-skills" || len(decoded) != 5 {
		t.Fatalf("plugin manifest = %s, err=%v", plugin, err)
	}
	if again, err := manager.Prepare(ctx, Request{WorkspaceRoot: workspace}); err != nil || again.Input.SnapshotID != snapshot.Input.SnapshotID || again.Input.ManifestSHA256 != snapshot.Input.ManifestSHA256 {
		t.Fatalf("repeat snapshot = %+v, err=%v", again.Input, err)
	}
}

func TestDefaultManagerIgnoresTestLikeBinaryNameAndEnvironment(t *testing.T) {
	// This is deliberately not parallel: it proves no runtime basename or
	// environment branch can replace the production allowlisted HTTPS sources.
	originalArguments := append([]string{}, os.Args...)
	t.Cleanup(func() { os.Args = originalArguments })
	os.Args[0] = "pika-go.test"
	t.Setenv("PIKA_GO_TEST_KERNELWIKI_SOURCE", "/tmp/forged-kernel")
	t.Setenv("PIKA_GO_TEST_NCU_REPORT_SOURCE", "/tmp/forged-ncu")

	sources := (Manager{}).configuredSources()
	if len(sources) != 2 || sources[0].Repository != symphony.KernelWikiRepository || sources[1].Repository != symphony.NCUReportSkillRepository {
		t.Fatalf("default production sources changed under test-like process state: %+v", sources)
	}
}

func TestSnapshotManifestUsesContractPathAndIdentityCoversResolvedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := t.TempDir()
	makeCleanupWritable(t, workspace)
	kernel := createSkillRepository(t, "master", "KernelWiki", "kernel-v1")
	ncu := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	when := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	manager := Manager{sources: []source{{Name: "KernelWiki", Repository: kernel, Branch: "master"}, {Name: "ncu-report-skill", Repository: ncu, Branch: "main"}}, Now: func() time.Time { return when }}
	snapshot, err := manager.Prepare(ctx, Request{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatal(err)
	}
	var published manifest
	if err := json.Unmarshal(snapshot.Input.Manifest, &published); err != nil {
		t.Fatal(err)
	}
	if len(published.Skills) != 2 || published.Skills[0].Path != "skills/KernelWiki" || published.Skills[1].Path != "skills/ncu-report-skill" {
		t.Fatalf("contract manifest paths = %+v", published.Skills)
	}
	if published.ValidationVersion != validationVersion {
		t.Fatalf("validation_version = %d", published.ValidationVersion)
	}
	identity, err := json.Marshal(canonicalManifest{SchemaVersion: published.SchemaVersion, ValidationVersion: published.ValidationVersion, ResolvedAt: published.ResolvedAt, Skills: published.Skills})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(identity)
	if got := hex.EncodeToString(digest[:]); snapshot.Input.SnapshotID != got {
		t.Fatalf("snapshot_id=%s want canonical manifest digest=%s", snapshot.Input.SnapshotID, got)
	}
	when = when.Add(time.Second)
	changed, err := manager.Prepare(ctx, Request{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Input.SnapshotID == snapshot.Input.SnapshotID {
		t.Fatal("resolved_at did not contribute to snapshot identity")
	}
}

func TestPublishedManifestUsesPublicDraft202012Schema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := t.TempDir()
	makeCleanupWritable(t, workspace)
	kernel := createSkillRepository(t, "master", "KernelWiki", "kernel-v1")
	ncu := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	manager := Manager{sources: []source{{Name: "KernelWiki", Repository: kernel, Branch: "master"}, {Name: "ncu-report-skill", Repository: ncu, Branch: "main"}}, Now: func() time.Time { return time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC) }}
	snapshot, err := manager.Prepare(ctx, Request{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatal(err)
	}
	schemaBytes, err := ManifestSchema()
	if err != nil || !bytes.Contains(schemaBytes, []byte("draft/2020-12")) {
		t.Fatalf("public manifest schema = %q, err=%v", schemaBytes, err)
	}
	compiler := jsonschema.NewCompiler()
	schemaValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("manifest.json", schemaValue); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(snapshot.Input.Manifest))
	if err != nil || compiled.Validate(value) != nil || ValidateManifest(snapshot.Input.Manifest) != nil {
		t.Fatalf("published canonical manifest was not accepted: decode=%v validate=%v", err, ValidateManifest(snapshot.Input.Manifest))
	}
	var invalid map[string]any
	if err := json.Unmarshal(snapshot.Input.Manifest, &invalid); err != nil {
		t.Fatal(err)
	}
	invalid["snapshot_id"] = "short"
	encoded, _ := json.Marshal(invalid)
	if err := ValidateManifest(encoded); err == nil {
		t.Fatal("manifest schema accepted short snapshot_id")
	}
	invalid["snapshot_id"] = snapshot.Input.SnapshotID
	delete(invalid, "validation_version")
	encoded, _ = json.Marshal(invalid)
	if err := ValidateManifest(encoded); err == nil {
		t.Fatal("manifest schema accepted missing validation_version")
	}
	invalid["validation_version"] = float64(validationVersion)
	invalid["skills"].([]any)[0].(map[string]any)["path"] = "skills/not-KernelWiki"
	encoded, _ = json.Marshal(invalid)
	if err := ValidateManifest(encoded); err == nil {
		t.Fatal("manifest schema accepted a non-canonical skill path")
	}
}

func TestPrepareFreezesHeadsAndCleansPartialFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := t.TempDir()
	makeCleanupWritable(t, workspace)
	kernel := createSkillRepository(t, "master", "KernelWiki", "kernel-v1")
	ncu := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	manager := Manager{sources: []source{
		{Name: "KernelWiki", Repository: kernel, Branch: "master"},
		{Name: "ncu-report-skill", Repository: ncu, Branch: "main"},
	}}
	first, err := manager.Prepare(ctx, Request{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, kernel, "change.txt", "kernel-v2")
	second, err := manager.Prepare(ctx, Request{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if first.Input.SnapshotID == second.Input.SnapshotID || first.Input.Entries[0].CommitSHA == second.Input.Entries[0].CommitSHA {
		t.Fatalf("branch movement did not create a new frozen snapshot: first=%+v second=%+v", first.Input, second.Input)
	}

	brokenWorkspace := t.TempDir()
	makeCleanupWritable(t, brokenWorkspace)
	broken := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	if err := os.Remove(filepath.Join(broken, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	commitAll(t, broken, "remove skill")
	_, err = (Manager{sources: []source{
		{Name: "KernelWiki", Repository: kernel, Branch: "master"},
		{Name: "ncu-report-skill", Repository: broken, Branch: "main"},
	}}).Prepare(ctx, Request{WorkspaceRoot: brokenWorkspace})
	if err == nil {
		t.Fatal("missing SKILL.md was accepted")
	}
	entries, readErr := os.ReadDir(filepath.Join(brokenWorkspace, "toolkits", "kda-skills"))
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed acquisition left published or staging state: entries=%v err=%v", entries, readErr)
	}
}

func TestRollbackPublicationRemovesOnlyNewOwnedSnapshot(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	makeCleanupWritable(t, workspace)
	kernel := createSkillRepository(t, "master", "KernelWiki", "kernel-v1")
	ncu := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	manager := Manager{
		sources: []source{{Name: "KernelWiki", Repository: kernel, Branch: "master"}, {Name: "ncu-report-skill", Repository: ncu, Branch: "main"}},
		Now:     func() time.Time { return time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC) },
	}
	first, err := manager.Prepare(context.Background(), Request{WorkspaceRoot: workspace})
	if err != nil || !first.Published {
		t.Fatalf("first Prepare = %+v, %v", first, err)
	}
	reused, err := manager.Prepare(context.Background(), Request{WorkspaceRoot: workspace})
	if err != nil || reused.Published {
		t.Fatalf("reused Prepare = %+v, %v", reused, err)
	}
	if err := reused.RollbackPublication(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Input.RootPath); err != nil {
		t.Fatalf("reused publication rollback removed durable snapshot: %v", err)
	}
	if err := first.RollbackPublication(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Input.RootPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new publication survived rollback: %v", err)
	}
}

func TestPrepareRejectsUnsafeSkillContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernel := createSkillRepository(t, "master", "KernelWiki", "kernel-v1")
	ncu := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	if err := os.Symlink("/etc/passwd", filepath.Join(kernel, "escaped")); err != nil {
		t.Fatal(err)
	}
	commitAll(t, kernel, "add unsafe link")
	_, err := (Manager{sources: []source{
		{Name: "KernelWiki", Repository: kernel, Branch: "master"},
		{Name: "ncu-report-skill", Repository: ncu, Branch: "main"},
	}}).Prepare(ctx, Request{WorkspaceRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("unsafe symlink error = %v", err)
	}
}

func TestValidateSkillRootAllowsOnlyContainedRelativeSymlinks(t *testing.T) {
	t.Parallel()
	writeSkill := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: KernelWiki\ndescription: test\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "docs"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "docs", "guide.md"), []byte("guide"), 0o600); err != nil {
			t.Fatal(err)
		}
		return root
	}

	safe := writeSkill(t)
	if err := os.Symlink("docs/guide.md", filepath.Join(safe, "guide.md")); err != nil {
		t.Fatal(err)
	}
	if err := validateSkillRoot(safe, "KernelWiki"); err != nil {
		t.Fatalf("contained relative symlink was rejected: %v", err)
	}

	escaping := writeSkill(t)
	if err := os.Symlink("../outside", filepath.Join(escaping, "escaped")); err != nil {
		t.Fatal(err)
	}
	if err := validateSkillRoot(escaping, "KernelWiki"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("escaping relative symlink error = %v", err)
	}

	circular := writeSkill(t)
	if err := os.Symlink("second", filepath.Join(circular, "first")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("first", filepath.Join(circular, "second")); err != nil {
		t.Fatal(err)
	}
	if err := validateSkillRoot(circular, "KernelWiki"); err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("circular symlink error = %v", err)
	}

	nestedGit := writeSkill(t)
	if err := os.Mkdir(filepath.Join(nestedGit, "docs", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateSkillRoot(nestedGit, "KernelWiki"); err == nil || !strings.Contains(err.Error(), "nested Git metadata") {
		t.Fatalf("nested .git error = %v", err)
	}

	gitVisible := writeSkill(t)
	if err := os.Mkdir(filepath.Join(gitVisible, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitVisible, ".git", "config"), []byte("mutable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git/config", filepath.Join(gitVisible, "visible-config")); err != nil {
		t.Fatal(err)
	}
	if err := validateSkillRoot(gitVisible, "KernelWiki"); err == nil || !strings.Contains(err.Error(), "Git metadata") {
		t.Fatalf("symlink into .git error = %v", err)
	}

	additionalSkill := writeSkill(t)
	if err := os.WriteFile(filepath.Join(additionalSkill, "docs", "SKILL.md"), []byte("---\nname: nested\ndescription: nested\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateSkillRoot(additionalSkill, "KernelWiki"); err == nil || !strings.Contains(err.Error(), "additional SKILL.md") {
		t.Fatalf("nested SKILL.md error = %v", err)
	}
}

func TestPrepareAllowsSkillGuidanceButRejectsProviderControlManifest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernel := createSkillRepository(t, "master", "KernelWiki", "kernel-v1")
	ncu := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	if err := os.WriteFile(filepath.Join(kernel, "CLAUDE.md"), []byte("# legitimate skill guidance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, kernel, "add skill guidance")
	manager := Manager{sources: []source{{Name: "KernelWiki", Repository: kernel, Branch: "master"}, {Name: "ncu-report-skill", Repository: ncu, Branch: "main"}}}
	workspace := t.TempDir()
	makeCleanupWritable(t, workspace)
	if _, err := manager.Prepare(ctx, Request{WorkspaceRoot: workspace}); err != nil {
		t.Fatalf("legitimate skill guidance was rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kernel, "mcp.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, kernel, "add hostile manifest")
	brokenWorkspace := t.TempDir()
	makeCleanupWritable(t, brokenWorkspace)
	if _, err := manager.Prepare(ctx, Request{WorkspaceRoot: brokenWorkspace}); err == nil || !strings.Contains(err.Error(), "provider control manifest") {
		t.Fatalf("provider control manifest error = %v", err)
	}
}

func TestVerifyFailsFastForMissingAndCorruptPublishedSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := t.TempDir()
	makeCleanupWritable(t, workspace)
	kernel := createSkillRepository(t, "master", "KernelWiki", "kernel-v1")
	ncu := createSkillRepository(t, "main", "ncu-report-skill", "ncu-v1")
	snapshot, err := (Manager{sources: []source{
		{Name: "KernelWiki", Repository: kernel, Branch: "master"},
		{Name: "ncu-report-skill", Repository: ncu, Branch: "main"},
	}}).Prepare(ctx, Request{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(snapshot.Input); err != nil {
		t.Fatalf("verify published snapshot: %v", err)
	}
	if err := os.Chmod(filepath.Join(snapshot.Input.RootPath, "skills", "KernelWiki", "SKILL.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot.Input.RootPath, "skills", "KernelWiki", "SKILL.md"), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(snapshot.Input); err == nil || !strings.Contains(err.Error(), "verify frozen skill snapshot") {
		t.Fatalf("corrupt snapshot verify error = %v", err)
	}
	missing := snapshot.Input
	missing.RootPath = filepath.Join(workspace, "does-not-exist")
	if err := Verify(missing); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing snapshot verify error = %v", err)
	}
}

func createSkillRepository(t *testing.T, branch, skillName, body string) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), skillName)
	runGit(t, "init", "--quiet", "--initial-branch="+branch, repository)
	runGit(t, "-C", repository, "config", "user.name", "Pika Test")
	runGit(t, "-C", repository, "config", "user.email", "pika@example.invalid")
	skill := "---\nname: " + skillName + "\ndescription: fixture skill\n---\n\n# " + skillName + "\n"
	if err := os.WriteFile(filepath.Join(repository, "SKILL.md"), []byte(skill), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "body.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repository, "initial")
	return repository
}

func commitFile(t *testing.T, repository, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repository, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repository, "update")
}

func commitAll(t *testing.T, repository, message string) {
	t.Helper()
	runGit(t, "-C", repository, "add", "-A")
	runGit(t, "-C", repository, "commit", "--quiet", "-m", message)
}

func runGit(t *testing.T, arguments ...string) {
	t.Helper()
	output, err := exec.Command("git", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func makeCleanupWritable(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil
			}
			mode := os.FileMode(0o600)
			if info.IsDir() {
				mode = 0o700
			}
			return os.Chmod(path, mode)
		})
	})
}
