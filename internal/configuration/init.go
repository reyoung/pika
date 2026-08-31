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
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/provider"
)

type Initializer struct {
	Workspace                *optimizationworkspace.Workspace
	ConfigRoot               string
	StateRoot                string
	InstanceID               string
	CodexHome                string
	CursorMCPPath            string
	CursorHooksPath          string
	PikaExecutable           string
	CodexExecutable          string
	Providers                *provider.Registry
	ConfigurationTOML        *string
	RequireConfigurationTOML bool
	ProbeProviders           bool
	ProviderExecutables      map[string]string
}

const DefaultSummary = `Pika-Go initialization defaults:
  iteration agents: 1
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
	providers := i.Providers
	if providers == nil {
		providers = provider.DefaultRegistry()
	}
	var configDir, stateDir, configPath, instructionsRoot string
	workspaceMode := i.Workspace != nil
	if workspaceMode {
		if i.Workspace.Identity.SourceRepository != resolvedRepository {
			return nil, fmt.Errorf("Workspace source repository %s does not match init repository %s", i.Workspace.Identity.SourceRepository, resolvedRepository)
		}
		configDir, stateDir = i.Workspace.Root, i.Workspace.Root
		configPath, instructionsRoot = i.Workspace.ConfigPath, i.Workspace.InstructionsRoot
	} else {
		if i.ConfigRoot == "" || i.StateRoot == "" || i.InstanceID == "" {
			return nil, errors.New("config root, state root, and instance ID are required")
		}
		if !filepath.IsAbs(i.ConfigRoot) || !filepath.IsAbs(i.StateRoot) {
			return nil, errors.New("config root and state root must be absolute")
		}
		configDir = filepath.Join(i.ConfigRoot, "instances", i.InstanceID)
		stateDir = filepath.Join(i.StateRoot, "instances", i.InstanceID)
		configPath = filepath.Join(configDir, "config.toml")
		instructionsRoot = filepath.Join(configDir, "instructions")
	}
	_, configPathErr := os.Stat(configPath)
	configurationExists := configPathErr == nil
	if configPathErr != nil && !errors.Is(configPathErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect instance configuration: %w", configPathErr)
	}
	if configurationExists && i.ConfigurationTOML != nil {
		return nil, errors.New("configuration_toml cannot overwrite existing user configuration")
	}
	if !configurationExists && i.RequireConfigurationTOML && i.ConfigurationTOML == nil {
		return nil, errors.New("configuration_toml is required for a new instance")
	}
	contents := renderConfig(resolvedRepository)
	if i.ConfigurationTOML != nil {
		contents = *i.ConfigurationTOML
	}
	var providerKinds []string
	if !configurationExists {
		providerKinds, err = validateCompleteConfiguration(contents, resolvedRepository, providers)
		if err != nil {
			return nil, err
		}
	} else {
		providerKinds, err = validateConfigurationPath(configPath, resolvedRepository, providers)
		if err != nil {
			return nil, err
		}
	}
	if i.ProbeProviders {
		if err := probeProviders(ctx, providers, providerKinds, i.ProviderExecutables); err != nil {
			return nil, err
		}
	}
	_, configDirErr := os.Stat(configDir)
	configDirWasAbsent := !workspaceMode && errors.Is(configDirErr, os.ErrNotExist)
	if configDirErr != nil && !configDirWasAbsent {
		return nil, fmt.Errorf("inspect instance config directory: %w", configDirErr)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("create instance config directory: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect instance config directory: %w", err)
	}
	stateDirectories := []string{"logs", "contexts", "evidence", "worktrees"}
	if workspaceMode {
		stateDirectories = []string{"logs", "contexts", "evidence", "runtime"}
	}
	for _, name := range stateDirectories {
		path := filepath.Join(stateDir, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("create instance state directory %q: %w", name, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, fmt.Errorf("protect instance state directory %q: %w", name, err)
		}
	}
	if err := instructions.Install(instructionsRoot); err != nil {
		return nil, err
	}

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
	cursorRollback := func() error { return nil }
	if containsString(providerKinds, "cursor") {
		mcpRollback, installErr := provider.InstallCursorMCP(i.CursorMCPPath)
		if installErr == nil {
			var hooksRollback func() error
			hooksRollback, installErr = provider.InstallCursorHooks(i.CursorHooksPath)
			if installErr == nil {
				cursorRollback = func() error { return errors.Join(hooksRollback(), mcpRollback()) }
			} else {
				_ = mcpRollback()
			}
		}
		err = installErr
		if err != nil {
			if rollbackErr := configRollback(); rollbackErr != nil {
				return nil, fmt.Errorf("install Cursor MCP integration: %v; roll back instance configuration: %w", err, rollbackErr)
			}
			return nil, fmt.Errorf("install Cursor MCP integration: %w", err)
		}
	}
	if !containsString(providerKinds, "codex") {
		return func() error { return errors.Join(cursorRollback(), configRollback()) }, nil
	}
	profileRollback, err := codexprofile.Install(codexprofile.Options{
		CodexHome:       i.CodexHome,
		InstanceBin:     filepath.Join(stateDir, "runtime", "bin"),
		PikaExecutable:  i.PikaExecutable,
		CodexExecutable: i.CodexExecutable,
	})
	if err != nil {
		if rollbackErr := errors.Join(cursorRollback(), configRollback()); rollbackErr != nil {
			return nil, fmt.Errorf("install Codex integration: %v; roll back instance configuration: %w", err, rollbackErr)
		}
		return nil, fmt.Errorf("install Codex integration: %w", err)
	}
	return func() error {
		return errors.Join(profileRollback(), cursorRollback(), configRollback())
	}, nil
}

func probeProviders(ctx context.Context, providers *provider.Registry, kinds []string, executables map[string]string) error {
	for _, kind := range kinds {
		capabilities, err := providers.Probe(ctx, kind, provider.ProbeRequest{Executable: executables[kind]})
		if err != nil {
			return fmt.Errorf("probe %s provider: %w", kind, err)
		}
		if !capabilities.Compatible || !capabilities.Authenticated || !capabilities.Journal || !capabilities.TurnStop ||
			!capabilities.FollowUp || !capabilities.FullOutput || !capabilities.FreshSession || !capabilities.Interrupt {
			return fmt.Errorf("probe %s provider: required capabilities are unavailable", kind)
		}
	}
	return nil
}

// ProbeConfiguredProviders validates one complete persisted configuration and
// probes exactly the distinct provider kinds referenced by its configured Agents.
func ProbeConfiguredProviders(ctx context.Context, path string, providers *provider.Registry, executables map[string]string) error {
	identity, err := LoadIdentity(path)
	if err != nil {
		return err
	}
	repository, err := filepath.EvalSymlinks(identity.Repository)
	if err != nil {
		return fmt.Errorf("resolve configured repository: %w", err)
	}
	kinds, err := validateConfigurationPath(path, repository, providers)
	if err != nil {
		return err
	}
	return probeProviders(ctx, providers, kinds, executables)
}

func validateCompleteConfiguration(contents, repository string, providers *provider.Registry) ([]string, error) {
	temporary, err := os.CreateTemp("", "pika-go-config-*.toml")
	if err != nil {
		return nil, fmt.Errorf("create temporary configuration: %w", err)
	}
	path := temporary.Name()
	defer os.Remove(path)
	if _, err := temporary.WriteString(contents); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write temporary configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close temporary configuration: %w", err)
	}
	return validateConfigurationPath(path, repository, providers)
}

func validateConfigurationPath(path, repository string, providers *provider.Registry) ([]string, error) {
	identity, err := LoadIdentity(path)
	if err != nil {
		return nil, err
	}
	configuredRepository, err := filepath.EvalSymlinks(identity.Repository)
	if err != nil {
		return nil, fmt.Errorf("resolve configured repository: %w", err)
	}
	if filepath.Clean(configuredRepository) != filepath.Clean(repository) {
		return nil, fmt.Errorf("configured repository %s does not match init repository %s", identity.Repository, repository)
	}
	if _, err := LoadScheduler(path); err != nil {
		return nil, err
	}
	if _, err := LoadContext(path); err != nil {
		return nil, err
	}
	if _, err := LoadFollowUp(path); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var kinds []string
	for _, role := range []string{"baseline", "baseline_verify", "iteration", "integration", "follow_up"} {
		agents, err := LoadAgentsWithRegistry(path, role, providers)
		if err != nil {
			return nil, err
		}
		for _, agent := range agents {
			if !seen[agent.Kind] {
				seen[agent.Kind] = true
				kinds = append(kinds, agent.Kind)
			}
		}
	}
	return kinds, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
	contents, err := RenderConfiguration(repository, DefaultAgents())
	if err != nil {
		panic(err)
	}
	return contents
}

var AgentRoleOrder = []string{"baseline", "baseline_verify", "iteration", "integration", "follow_up"}

func DefaultAgents() map[string]Agent {
	return map[string]Agent{
		"baseline":        {Kind: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"},
		"baseline_verify": {Kind: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"},
		"iteration":       {Kind: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"},
		"integration":     {Kind: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"},
		"follow_up":       {Kind: "codex", Model: "gpt-5.6-terra", ReasoningEffort: "medium"},
	}
}

func RenderConfiguration(repository string, agents map[string]Agent, configuredIterationAgents ...Agent) (string, error) {
	if repository == "" || !filepath.IsAbs(repository) {
		return "", errors.New("optimization.repository must be absolute")
	}
	registry := provider.DefaultRegistry()
	iterationAgents := append([]Agent(nil), configuredIterationAgents...)
	if len(iterationAgents) == 0 {
		if agent, found := agents["iteration"]; found {
			iterationAgents = append(iterationAgents, agent)
		}
	}
	if len(iterationAgents) == 0 {
		return "", errors.New("at least one Iteration Agent configuration is required")
	}
	for _, role := range AgentRoleOrder {
		if role == "iteration" {
			continue
		}
		agent, found := agents[role]
		if !found {
			return "", fmt.Errorf("missing Agent configuration for %s", role)
		}
		adapter, err := registry.Resolve(agent.Kind)
		if err != nil {
			return "", err
		}
		if err := adapter.Validate(agent); err != nil {
			return "", fmt.Errorf("validate agents.%s: %w", role, err)
		}
	}
	for index, agent := range iterationAgents {
		adapter, err := registry.Resolve(agent.Kind)
		if err != nil {
			return "", err
		}
		if err := adapter.Validate(agent); err != nil {
			return "", fmt.Errorf("validate agents.iteration[%d]: %w", index, err)
		}
	}
	var builder strings.Builder
	builder.WriteString(`version = 1

[optimization]
repository = ` + strconv.Quote(repository) + `

[scheduler]
max_pending_attempts = 8

[context.iteration]
history_limit = 20

`)
	writeAgent := func(header string, agent Agent) {
		builder.WriteString(header + "\n")
		builder.WriteString("kind = " + strconv.Quote(agent.Kind) + "\n")
		builder.WriteString("model = " + strconv.Quote(agent.Model) + "\n")
		builder.WriteString("reasoning_effort = " + strconv.Quote(agent.ReasoningEffort) + "\n")
		if len(agent.Args) != 0 {
			builder.WriteString("args = [")
			for index, argument := range agent.Args {
				if index != 0 {
					builder.WriteString(", ")
				}
				builder.WriteString(strconv.Quote(argument))
			}
			builder.WriteString("]\n")
		}
		builder.WriteByte('\n')
	}
	for _, role := range AgentRoleOrder {
		if role == "iteration" {
			for _, agent := range iterationAgents {
				writeAgent("[[agents.iteration]]", agent)
			}
			continue
		}
		writeAgent("[agents."+role+"]", agents[role])
	}
	builder.WriteString(`[follow_up]
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
`)
	return builder.String(), nil
}

func RenderDefaults(repository string) string {
	return renderConfig(repository)
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
