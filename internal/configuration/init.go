package configuration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/reyoung/pika-go/internal/codexprofile"
	"github.com/reyoung/pika-go/internal/instructions"
)

type Initializer struct {
	ConfigRoot      string
	StateRoot       string
	InstanceID      string
	CodexHome       string
	PikaExecutable  string
	CodexExecutable string
}

const DefaultSummary = `Pika-Go initialization defaults:
  iteration concurrency: 4
  max pending attempts: 8
  follow-up inactivity timeout: 5m
  follow-up generator concurrency: 1
  core agent: codex / gpt-5.6-sol / high
  follow-up agent: codex / gpt-5.6-terra / medium`

func (i Initializer) Prepare(ctx context.Context, repository string) (func() error, error) {
	resolvedRepository, err := validateRepository(ctx, repository)
	if err != nil {
		return nil, err
	}
	if i.ConfigRoot == "" || i.StateRoot == "" || i.InstanceID == "" {
		return nil, errors.New("config root, state root, and instance ID are required")
	}
	if !filepath.IsAbs(i.ConfigRoot) || !filepath.IsAbs(i.StateRoot) {
		return nil, errors.New("config root and state root must be absolute")
	}

	configDir := filepath.Join(i.ConfigRoot, "instances", i.InstanceID)
	stateDir := filepath.Join(i.StateRoot, "instances", i.InstanceID)
	_, configDirErr := os.Stat(configDir)
	configDirWasAbsent := errors.Is(configDirErr, os.ErrNotExist)
	if configDirErr != nil && !configDirWasAbsent {
		return nil, fmt.Errorf("inspect instance config directory: %w", configDirErr)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("create instance config directory: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect instance config directory: %w", err)
	}
	for _, name := range []string{"logs", "contexts", "evidence", "worktrees"} {
		path := filepath.Join(stateDir, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("create instance state directory %q: %w", name, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, fmt.Errorf("protect instance state directory %q: %w", name, err)
		}
	}
	if err := instructions.Install(filepath.Join(configDir, "instructions")); err != nil {
		return nil, err
	}

	contents := renderConfig(resolvedRepository)
	configPath := filepath.Join(configDir, "config.toml")
	configRollback := func() error { return nil }
	_, err = os.ReadFile(configPath)
	if err == nil {
		identity, identityErr := LoadIdentity(configPath)
		if identityErr != nil {
			return nil, identityErr
		}
		configuredRepository, resolveErr := filepath.EvalSymlinks(identity.Repository)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve configured repository: %w", resolveErr)
		}
		if filepath.Clean(configuredRepository) != filepath.Clean(resolvedRepository) {
			return nil, fmt.Errorf("configured repository %s does not match init repository %s", identity.Repository, resolvedRepository)
		}
		if err := os.Chmod(configPath, 0o600); err != nil {
			return nil, fmt.Errorf("protect instance configuration: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read existing instance configuration: %w", err)
	} else {
		if err := writeFileAtomic(configPath, []byte(contents), 0o600); err != nil {
			return nil, err
		}
		configRollback = func() error {
			if configDirWasAbsent {
				if err := os.RemoveAll(configDir); err != nil {
					return fmt.Errorf("roll back instance configuration directory: %w", err)
				}
				return nil
			}
			if err := os.Remove(configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("roll back instance configuration: %w", err)
			}
			return nil
		}
	}
	profileRollback, err := codexprofile.Install(codexprofile.Options{
		CodexHome:       i.CodexHome,
		InstanceBin:     filepath.Join(stateDir, "runtime", "bin"),
		PikaExecutable:  i.PikaExecutable,
		CodexExecutable: i.CodexExecutable,
	})
	if err != nil {
		if rollbackErr := configRollback(); rollbackErr != nil {
			return nil, fmt.Errorf("install Codex integration: %v; roll back instance configuration: %w", err, rollbackErr)
		}
		return nil, fmt.Errorf("install Codex integration: %w", err)
	}
	return func() error {
		return errors.Join(profileRollback(), configRollback())
	}, nil
}

func validateRepository(ctx context.Context, repository string) (string, error) {
	if repository == "" || !filepath.IsAbs(repository) {
		return "", errors.New("repository must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return "", fmt.Errorf("resolve repository: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("repository is not a directory: %s", repository)
	}
	command := exec.CommandContext(ctx, "git", "-C", resolved, "rev-parse", "--show-toplevel")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("repository is not a Git worktree: %s", repository)
	}
	root := strings.TrimSpace(string(output))
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve Git worktree root: %w", err)
	}
	if filepath.Clean(resolvedRoot) != filepath.Clean(resolved) {
		return "", fmt.Errorf("repository must be the Git worktree root: got %s, root is %s", repository, root)
	}
	return resolved, nil
}

func renderConfig(repository string) string {
	return `version = 1

[optimization]
repository = ` + strconv.Quote(repository) + `

[scheduler]
iteration_concurrency = 4
max_pending_attempts = 8

[agents.baseline]
kind = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agents.baseline_verify]
kind = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agents.iteration]
kind = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agents.integration]
kind = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agents.follow_up]
kind = "codex"
model = "gpt-5.6-terra"
reasoning_effort = "medium"

[follow_up]
pane_idle_timeout = "5m"
generator_concurrency = 1

[follow_up.baseline_verify]
max_messages = 8
generator_max_attempts = 3

[follow_up.iteration]
max_messages = 5
generator_max_attempts = 3

[follow_up.integration]
max_messages = 8
generator_max_attempts = 3
`
}

func writeFileAtomic(path string, contents []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary configuration permissions: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write temporary configuration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary configuration: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit instance configuration: %w", err)
	}
	removeTemporary = false
	return nil
}
