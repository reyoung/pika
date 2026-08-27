package configuration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/configuration"
)

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
	contents = []byte(strings.Replace(string(contents), "iteration_concurrency = 4", "iteration_concurrency = 2", 1))
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
