package provider_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestCodexSkillInjectionIsScopedCleanAndOwned(t *testing.T) {
	t.Parallel()
	repository := newProviderRepository(t)
	snapshot := newFrozenSkills(t)
	launch, err := provider.NewCodexAdapter().PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "session", Repository: repository, Configuration: provider.AgentConfiguration{Kind: "codex"},
		SystemPrompt: []byte("system"), FrozenSkills: &snapshot,
	})
	if err != nil {
		t.Fatalf("prepare Codex with frozen skills: %v", err)
	}
	for _, expected := range []struct{ mount, target string }{
		{"pika-kda-kernelwiki", snapshot.Skills[0].Path},
		{"pika-kda-ncu-report", snapshot.Skills[1].Path},
	} {
		path := filepath.Join(repository, ".agents", "skills", expected.mount)
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("missing Codex discovery link %s: info=%v err=%v", path, info, statErr)
		}
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil || filepath.Clean(resolved) != filepath.Clean(expected.target) {
			t.Fatalf("Codex discovery link %s = %s, err=%v", expected.mount, resolved, resolveErr)
		}
	}
	if status := strings.TrimSpace(runProviderGit(t, repository, "status", "--porcelain=v1", "--untracked-files=all")); status != "" {
		t.Fatalf("skill mounts contaminated Git status: %q", status)
	}
	receipt := filepath.Join(repository, ".agents", "skills", ".pika-kda-ownership.json")
	if contents, err := os.ReadFile(receipt); err != nil || !strings.Contains(string(contents), `"agent_session_id":"session"`) {
		t.Fatalf("Codex ownership receipt = %q, %v", contents, err)
	}
	if launch.Cleanup == nil || launch.Cleanup() != nil {
		t.Fatal("Codex skill cleanup failed")
	}
	if _, err := os.Lstat(filepath.Join(repository, ".agents", "skills", "pika-kda-kernelwiki")); !os.IsNotExist(err) {
		t.Fatalf("Codex cleanup retained owned link: %v", err)
	}
}

func TestCodexSkillCleanupRequiresUnchangedOwnershipReceipt(t *testing.T) {
	t.Parallel()
	repository := newProviderRepository(t)
	snapshot := newFrozenSkills(t)
	launch, err := provider.NewCodexAdapter().PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "session", Repository: repository, Configuration: provider.AgentConfiguration{Kind: "codex"},
		SystemPrompt: []byte("system"), FrozenSkills: &snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(repository, ".agents", "skills", ".pika-kda-ownership.json")
	if err := os.WriteFile(receipt, []byte(`{"owner":"someone-else"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := launch.Cleanup(); err == nil || !strings.Contains(err.Error(), "ownership receipt") {
		t.Fatalf("cleanup with changed receipt error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repository, ".agents", "skills", "pika-kda-kernelwiki")); err != nil {
		t.Fatalf("cleanup removed link without ownership receipt: %v", err)
	}
}

func TestCodexSkillInjectionReclaimsVerifiedReceiptAfterSessionReplacement(t *testing.T) {
	t.Parallel()
	repository := newProviderRepository(t)
	snapshot := newFrozenSkills(t)
	stale, err := provider.NewCodexAdapter().PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "stale-session", Repository: repository, Configuration: provider.AgentConfiguration{Kind: "codex"},
		SystemPrompt: []byte("system"), FrozenSkills: &snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := provider.NewCodexAdapter().PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "replacement-session", Repository: repository, Configuration: provider.AgentConfiguration{Kind: "codex"},
		SystemPrompt: []byte("system"), FrozenSkills: &snapshot,
	})
	if err != nil {
		t.Fatalf("reclaim verified stale mount: %v", err)
	}
	receipt := filepath.Join(repository, ".agents", "skills", ".pika-kda-ownership.json")
	if contents, err := os.ReadFile(receipt); err != nil || !strings.Contains(string(contents), `"agent_session_id":"replacement-session"`) {
		t.Fatalf("replacement receipt = %q, %v", contents, err)
	}
	if err := stale.Cleanup(); err == nil {
		t.Fatal("stale Session cleanup removed replacement-owned mounts")
	}
	if err := replacement.Cleanup(); err != nil {
		t.Fatalf("replacement cleanup: %v", err)
	}
}

func TestCodexSkillInjectionRejectsHostileDiscoveryPath(t *testing.T) {
	t.Parallel()
	repository := newProviderRepository(t)
	snapshot := newFrozenSkills(t)
	hostile := filepath.Join(repository, ".agents", "skills", "pika-kda-kernelwiki")
	if err := os.MkdirAll(hostile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewCodexAdapter().PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "session", Repository: repository, Configuration: provider.AgentConfiguration{Kind: "codex"},
		SystemPrompt: []byte("system"), FrozenSkills: &snapshot,
	}); err == nil || !strings.Contains(err.Error(), "not Pika-owned") {
		t.Fatalf("hostile discovery path error = %v", err)
	}
}

func TestCodexSkillInjectionRejectsSymlinkedParentDirectories(t *testing.T) {
	t.Parallel()
	for _, parent := range []string{".agents", filepath.Join(".agents", "skills")} {
		t.Run(parent, func(t *testing.T) {
			repository := newProviderRepository(t)
			outside := t.TempDir()
			link := filepath.Join(repository, parent)
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			snapshot := newFrozenSkills(t)
			_, err := provider.NewCodexAdapter().PrepareSession(context.Background(), provider.SessionActivation{
				AgentSessionID: "session", Repository: repository, Configuration: provider.AgentConfiguration{Kind: "codex"},
				SystemPrompt: []byte("system"), FrozenSkills: &snapshot,
			})
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlinked parent error = %v", err)
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("mount escaped through parent symlink: entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestCodexSkillInjectionPreservesExistingDirectoryMode(t *testing.T) {
	t.Parallel()
	repository := newProviderRepository(t)
	skillsRoot := filepath.Join(repository, ".agents", "skills")
	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(skillsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := newFrozenSkills(t)
	launch, err := provider.NewCodexAdapter().PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "session", Repository: repository, Configuration: provider.AgentConfiguration{Kind: "codex"},
		SystemPrompt: []byte("system"), FrozenSkills: &snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(skillsRoot); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o755 {
		t.Fatalf("pre-existing skills mode = %v", info.Mode().Perm())
	}
	if err := launch.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(skillsRoot); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("cleanup changed pre-existing skills directory: %v, %v", info, err)
	}
}

func TestCodexSkillInjectionRollsBackPartialMountOnly(t *testing.T) {
	t.Parallel()
	repository := newProviderRepository(t)
	skillsRoot := filepath.Join(repository, ".agents", "skills")
	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := filepath.Join(skillsRoot, "pika-kda-ncu-report")
	if err := os.WriteFile(hostile, []byte("user-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := newFrozenSkills(t)
	_, err := provider.NewCodexAdapter().PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "session", Repository: repository, Configuration: provider.AgentConfiguration{Kind: "codex"},
		SystemPrompt: []byte("system"), FrozenSkills: &snapshot,
	})
	if err == nil || !strings.Contains(err.Error(), "not Pika-owned") {
		t.Fatalf("partial mount error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(skillsRoot, "pika-kda-kernelwiki")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial mount retained first Pika link: %v", err)
	}
	if contents, err := os.ReadFile(hostile); err != nil || string(contents) != "user-owned" {
		t.Fatalf("partial cleanup modified user path: %q, %v", contents, err)
	}
	if _, err := os.Lstat(filepath.Join(skillsRoot, ".pika-kda-ownership.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial mount retained ownership receipt: %v", err)
	}
}

func TestCursorSkillInjectionAppendsOnePluginDirWithoutShellEvaluation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repository := newProviderRepository(t)
	snapshot := newFrozenSkills(t)
	cursorExecutable := executableFixture(t, root, "cursor-agent")
	pikaExecutable := executableFixture(t, root, "pika-go")
	adapter := provider.NewCursorAdapter(provider.CursorOptions{
		Executable: cursorExecutable, RuntimeRoot: filepath.Join(root, "runtime"), InstanceBin: filepath.Join(root, "bin"), PikaExecutable: pikaExecutable,
	})
	marker := filepath.Join(root, "must-not-exist")
	launch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: "cursor-session", Repository: repository,
		Configuration: provider.AgentConfiguration{Kind: "cursor", Model: "auto", Args: []string{"--header", "$(touch " + marker + ")"}},
		SystemPrompt:  []byte("system"), FrozenSkills: &snapshot,
	})
	if err != nil {
		t.Fatalf("prepare Cursor with frozen skills: %v", err)
	}
	arguments, err := os.ReadFile(launch.Environment["PIKA_CURSOR_ARGS_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"--header", "$(touch " + marker + ")", "--plugin-dir", snapshot.RootPath}, "\n")
	if strings.TrimSpace(string(arguments)) != want || filepath.Clean(launch.Environment["PIKA_CURSOR_WORKSPACE"]) != filepath.Clean(repository) {
		t.Fatalf("Cursor argv/workspace = %q / %q, want %q / %q", arguments, launch.Environment["PIKA_CURSOR_WORKSPACE"], want, repository)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Cursor argv was shell-evaluated: %v", err)
	}
	if launch.Cleanup == nil || launch.Cleanup() != nil {
		t.Fatal("Cursor cleanup failed")
	}
}

func newFrozenSkills(t *testing.T) provider.FrozenSkillSnapshot {
	t.Helper()
	root := filepath.Join(t.TempDir(), "snapshot")
	specifications := []struct{ name, path string }{{"KernelWiki", "KernelWiki"}, {"ncu-report-skill", "ncu-report-skill"}}
	skills := make([]provider.FrozenSkillReference, 0, len(specifications))
	for _, specification := range specifications {
		path := filepath.Join(root, "skills", specification.path)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: "+specification.name+"\ndescription: test skill\n---\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		skills = append(skills, provider.FrozenSkillReference{Name: specification.name, Path: path, CommitSHA: strings.Repeat("a", 40), ContentSHA: strings.Repeat("b", 64)})
	}
	return provider.FrozenSkillSnapshot{SnapshotID: "snapshot", RootPath: root, Skills: skills}
}

func newProviderRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	runProviderGit(t, "", "init", "--quiet", "--initial-branch=main", repository)
	runProviderGit(t, repository, "config", "user.name", "Pika Test")
	runProviderGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runProviderGit(t, repository, "add", "README.md")
	runProviderGit(t, repository, "commit", "--quiet", "-m", "fixture")
	return repository
}

func runProviderGit(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	if repository != "" {
		arguments = append([]string{"-C", repository}, arguments...)
	}
	output, err := exec.Command("git", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}

func executableFixture(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
