package configuration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/provider"
)

func TestPrepareInitWritesAndRollsBackOnlyWorkspaceOwnedConfiguration(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"init", "--quiet", "--initial-branch=main"}, {"config", "user.name", "Pika Test"}, {"config", "user.email", "pika@example.invalid"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "README.md"}, {"commit", "--quiet", "-m", "initial"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	initializer := configuration.Initializer{
		Workspace: &workspace, CodexHome: filepath.Join(t.TempDir(), "codex"),
		PikaExecutable: "/bin/echo", CodexExecutable: "/bin/sh",
	}
	rollback, err := initializer.Prepare(context.Background(), workspace.Identity.SourceRepository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.ConfigPath); err != nil {
		t.Fatalf("Workspace pika.toml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.RuntimeRoot, "bin", "codex")); err != nil {
		t.Fatalf("Workspace Codex wrapper: %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("Workspace configuration remains after rollback: %v", err)
	}
	if _, err := os.Stat(workspace.ManifestPath); err != nil {
		t.Fatalf("rollback removed Workspace identity: %v", err)
	}
	if _, err := os.Stat(workspace.BaseRepository); err != nil {
		t.Fatalf("rollback removed base worktree: %v", err)
	}
}

func TestPrepareInitCommitsOwnedConfigurationAndCanRollback(t *testing.T) {
	t.Parallel()

	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	configRoot := filepath.Join(t.TempDir(), "config")
	stateRoot := filepath.Join(t.TempDir(), "state")
	codexHome := filepath.Join(t.TempDir(), "codex")
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: stateRoot, InstanceID: "instance-1",
		CodexHome: codexHome, PikaExecutable: "/bin/echo", CodexExecutable: "/bin/sh",
	}

	rollback, err := initializer.Prepare(context.Background(), repository)
	if err != nil {
		t.Fatalf("prepare init: %v", err)
	}
	configPath := filepath.Join(configRoot, "instances", "instance-1", "config.toml")
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	resolvedRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatalf("resolve repository: %v", err)
	}
	if !strings.Contains(string(contents), `repository = "`+resolvedRepository+`"`) || !strings.Contains(string(contents), `pane_idle_timeout = "5m"`) {
		t.Fatalf("config contents = %s", contents)
	}
	for _, directory := range []string{"logs", "contexts", "evidence", "worktrees"} {
		if info, err := os.Stat(filepath.Join(stateRoot, "instances", "instance-1", directory)); err != nil || !info.IsDir() {
			t.Fatalf("state directory %s: info=%v err=%v", directory, info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(codexHome, "pika-go-managed.config.toml")); err != nil {
		t.Fatalf("Codex profile was not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "instances", "instance-1", "runtime", "bin", "codex")); err != nil {
		t.Fatalf("Codex wrapper was not installed: %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config remains after rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "pika-go-managed.config.toml")); !os.IsNotExist(err) {
		t.Fatalf("Codex profile remains after rollback: %v", err)
	}
}

func TestPrepareInitRejectsNonGitRepositoryWithoutWriting(t *testing.T) {
	t.Parallel()

	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	configRoot := filepath.Join(t.TempDir(), "config")
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: filepath.Join(t.TempDir(), "state"), InstanceID: "instance-1",
		CodexHome: filepath.Join(t.TempDir(), "codex"), PikaExecutable: "/bin/echo", CodexExecutable: "/bin/sh",
	}
	if _, err := initializer.Prepare(context.Background(), repository); err == nil {
		t.Fatal("non-Git repository was accepted")
	}
	if _, err := os.Stat(filepath.Join(configRoot, "instances", "instance-1", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("configuration written after validation failure: %v", err)
	}
}

func TestPrepareInitRejectsBenchmarkAgentInConfigurationVersionOne(t *testing.T) {
	t.Parallel()
	repository := filepath.Join(t.TempDir(), "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	agents := configuration.DefaultAgents()
	agents["benchmark"] = configuration.Agent{Kind: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"}
	candidate, err := configuration.RenderConfiguration(repository, agents)
	if err != nil {
		t.Fatal(err)
	}
	candidate = strings.Replace(candidate, "version = 2", "version = 1", 1)
	configRoot := filepath.Join(t.TempDir(), "config")
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: filepath.Join(t.TempDir(), "state"), InstanceID: "invalid-v1",
		ConfigurationTOML: &candidate, RequireConfigurationTOML: true,
	}
	if _, err := initializer.Prepare(context.Background(), repository); err == nil || !strings.Contains(err.Error(), "agents.benchmark requires configuration version 2") {
		t.Fatalf("version-1 benchmark configuration error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "instances", "invalid-v1", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("invalid configuration was written: %v", err)
	}
}

func TestPrepareInitPreservesExistingUserConfiguration(t *testing.T) {
	t.Parallel()

	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	configRoot := filepath.Join(t.TempDir(), "config")
	stateRoot := filepath.Join(t.TempDir(), "state")
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: stateRoot, InstanceID: "instance-1",
		CodexHome: filepath.Join(t.TempDir(), "codex"), PikaExecutable: "/bin/echo", CodexExecutable: "/bin/sh",
	}
	firstRollback, err := initializer.Prepare(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configRoot, "instances", "instance-1", "config.toml")
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), "history_limit = 20", "history_limit = 7", 1))
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	secondRollback, err := initializer.Prepare(context.Background(), repository)
	if err != nil {
		t.Fatalf("prepare with user-owned configuration: %v", err)
	}
	if err := secondRollback(); err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("user configuration removed by rollback: %v", err)
	}
	if string(preserved) != string(contents) {
		t.Fatal("user configuration was overwritten")
	}
	if err := firstRollback(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareInitCursorOnlyDoesNotRequireOrInstallCodex(t *testing.T) {
	t.Parallel()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	resolved, _ := filepath.EvalSymlinks(repository)
	cursor := filepath.Join(t.TempDir(), "cursor-agent")
	if err := os.WriteFile(cursor, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 2026.08.31-4057e58; exit 0; fi\nif [ \"$1\" = status ]; then echo 'Logged in as test@example.com'; exit 0; fi\nif [ \"$1\" = --plugin-dir ] && [ \"$3\" = --help ]; then echo --plugin-dir; exit 0; fi\nif [ \"$1\" = --list-models ]; then printf 'Available models\\nauto - Auto (default)\\ngpt-5.6-sol-high - GPT-5.6 Sol High\\ngpt-5.6-terra-medium - GPT-5.6 Terra Medium\\n'; exit 0; fi\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := strings.ReplaceAll(configuration.RenderDefaults(resolved), `kind = "codex"`, `kind = "cursor"`)
	configRoot := filepath.Join(t.TempDir(), "config")
	stateRoot := filepath.Join(t.TempDir(), "state")
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: stateRoot, InstanceID: "cursor-only", PikaExecutable: "/bin/echo",
		CursorMCPPath: filepath.Join(t.TempDir(), ".cursor", "mcp.json"), CursorHooksPath: filepath.Join(t.TempDir(), ".cursor", "hooks.json"),
		ConfigurationTOML: &candidate, RequireConfigurationTOML: true, ProbeProviders: true,
		ProviderExecutables:   map[string]string{"cursor": cursor},
		ProviderModelRequests: map[string]provider.ModelRequest{"cursor": {Executable: cursor}},
	}
	if _, err := initializer.Prepare(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configRoot, "instances", "cursor-only", "config.toml")
	stored, err := os.ReadFile(configPath)
	if err != nil || string(stored) != candidate {
		t.Fatalf("stored candidate: err=%v equal=%v", err, string(stored) == candidate)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "instances", "cursor-only", "runtime", "bin", "codex")); !os.IsNotExist(err) {
		t.Fatalf("Cursor-only init installed Codex: %v", err)
	}
}

func TestPrepareInitRejectsCatalogUnavailableModelBeforeWritingConfiguration(t *testing.T) {
	t.Parallel()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(t.TempDir(), "cursor-agent")
	if err := os.WriteFile(cursor, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 2026.08.31-4057e58; exit 0; fi\nif [ \"$1\" = status ]; then echo 'Logged in as test@example.com'; exit 0; fi\nif [ \"$1\" = --plugin-dir ] && [ \"$3\" = --help ]; then echo --plugin-dir; exit 0; fi\nif [ \"$1\" = --list-models ]; then printf 'Available models\\nauto - Auto (default)\\n'; exit 0; fi\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := strings.ReplaceAll(configuration.RenderDefaults(resolved), `kind = "codex"`, `kind = "cursor"`)
	candidate = strings.ReplaceAll(candidate, `model = "gpt-5.6-sol"`, `model = "gpt-5.6-luna"`)
	candidate = strings.ReplaceAll(candidate, `model = "gpt-5.6-terra"`, `model = "gpt-5.6-luna"`)
	candidate = strings.ReplaceAll(candidate, `reasoning_effort = "high"`, `reasoning_effort = "medium"`)
	configRoot, stateRoot := filepath.Join(t.TempDir(), "config"), filepath.Join(t.TempDir(), "state")
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: stateRoot, InstanceID: "catalog-unavailable", PikaExecutable: "/bin/echo",
		CursorMCPPath: filepath.Join(t.TempDir(), ".cursor", "mcp.json"), CursorHooksPath: filepath.Join(t.TempDir(), ".cursor", "hooks.json"),
		ConfigurationTOML: &candidate, RequireConfigurationTOML: true, ProbeProviders: true,
		ProviderExecutables:   map[string]string{"cursor": cursor},
		ProviderModelRequests: map[string]provider.ModelRequest{"cursor": {Executable: cursor}},
	}
	if _, err := initializer.Prepare(context.Background(), repository); err == nil || !strings.Contains(err.Error(), `model "gpt-5.6-luna" is not available`) {
		t.Fatalf("catalog-unavailable configuration error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "instances", "catalog-unavailable", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("configuration was written after catalog rejection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "instances", "catalog-unavailable")); !os.IsNotExist(err) {
		t.Fatalf("state was written after catalog rejection: %v", err)
	}
}

func TestPrepareInitMixedProviderProbeFailureIsAtomic(t *testing.T) {
	t.Parallel()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	resolved, _ := filepath.EvalSymlinks(repository)
	cursor := filepath.Join(t.TempDir(), "cursor-agent")
	if err := os.WriteFile(cursor, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo wrong-version; exit 0; fi\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := strings.Replace(configuration.RenderDefaults(resolved), `kind = "codex"`, `kind = "cursor"`, 1)
	configRoot := filepath.Join(t.TempDir(), "config")
	stateRoot := filepath.Join(t.TempDir(), "state")
	initializer := configuration.Initializer{
		ConfigRoot: configRoot, StateRoot: stateRoot, InstanceID: "mixed", CodexHome: filepath.Join(t.TempDir(), "codex"),
		PikaExecutable: "/bin/echo", CodexExecutable: "/bin/echo", ConfigurationTOML: &candidate,
		RequireConfigurationTOML: true, ProbeProviders: true,
		ProviderExecutables: map[string]string{"cursor": cursor, "codex": "/bin/echo"},
	}
	if _, err := initializer.Prepare(context.Background(), repository); err == nil || !strings.Contains(err.Error(), "unsupported Cursor version") {
		t.Fatalf("mixed provider failure = %v", err)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "instances", "mixed", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("configuration was committed after failed preflight: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "instances", "mixed")); !os.IsNotExist(err) {
		t.Fatalf("state was committed after failed preflight: %v", err)
	}
}
