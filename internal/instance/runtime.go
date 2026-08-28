package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reyoung/pika-go/internal/optimizationworkspace"
)

type RuntimeOptions struct {
	SocketPath    string
	WorkspaceRoot string
	ConfigRoot    string
	StateRoot     string
	InstanceID    string
}

type RuntimePaths struct {
	SocketPath       string
	Workspace        *optimizationworkspace.Workspace
	WorkspaceRoot    string
	ConfigRoot       string
	StateRoot        string
	InstanceID       string
	InstanceDir      string
	ConfigPath       string
	DatabasePath     string
	LockPath         string
	InstructionsRoot string
	WorktreeRoot     string
	RuntimeRoot      string
}

func ResolveRuntime(options RuntimeOptions) (RuntimePaths, error) {
	socketPath, err := ResolveSocket(options.SocketPath)
	if err != nil {
		return RuntimePaths{}, err
	}
	workspaceRoot := firstNonempty(options.WorkspaceRoot, os.Getenv("PIKA_GO_WORKSPACE"))
	if workspaceRoot != "" {
		workspace, err := optimizationworkspace.Open(workspaceRoot)
		if err != nil {
			return RuntimePaths{}, err
		}
		return RuntimePaths{
			SocketPath: socketPath, Workspace: &workspace, WorkspaceRoot: workspace.Root,
			InstanceID: workspace.Identity.ID, InstanceDir: workspace.Root, ConfigPath: workspace.ConfigPath,
			DatabasePath: workspace.DatabasePath, LockPath: workspace.LockPath, InstructionsRoot: workspace.InstructionsRoot,
			WorktreeRoot: workspace.Root, RuntimeRoot: workspace.RuntimeRoot,
		}, nil
	}
	paths := RuntimePaths{
		SocketPath: socketPath,
		ConfigRoot: firstNonempty(options.ConfigRoot, os.Getenv("HERDR_PLUGIN_CONFIG_DIR")),
		StateRoot:  firstNonempty(options.StateRoot, os.Getenv("HERDR_PLUGIN_STATE_DIR")),
		InstanceID: firstNonempty(options.InstanceID, os.Getenv("PIKA_GO_INSTANCE")),
	}
	if paths.ConfigRoot == "" && paths.StateRoot == "" {
		return paths, nil
	}
	if paths.ConfigRoot == "" || paths.StateRoot == "" {
		return RuntimePaths{}, errors.New("plugin config and state directories must be provided together")
	}
	if !filepath.IsAbs(paths.ConfigRoot) || !filepath.IsAbs(paths.StateRoot) {
		return RuntimePaths{}, errors.New("plugin config and state directories must be absolute")
	}
	paths.ConfigRoot = filepath.Clean(paths.ConfigRoot)
	paths.StateRoot = filepath.Clean(paths.StateRoot)
	if paths.InstanceID == "" {
		paths.InstanceID = strings.TrimSuffix(filepath.Base(paths.SocketPath), filepath.Ext(paths.SocketPath))
	}
	if err := ValidateID(paths.InstanceID); err != nil {
		return RuntimePaths{}, err
	}
	paths.InstanceDir = filepath.Join(paths.StateRoot, "instances", paths.InstanceID)
	paths.ConfigPath = filepath.Join(paths.ConfigRoot, "instances", paths.InstanceID, "config.toml")
	paths.DatabasePath = filepath.Join(paths.InstanceDir, "pika.db")
	paths.LockPath = filepath.Join(paths.InstanceDir, "daemon.lock")
	paths.InstructionsRoot = filepath.Join(paths.ConfigRoot, "instances", paths.InstanceID, "instructions")
	paths.WorktreeRoot = filepath.Join(paths.InstanceDir, "worktrees")
	paths.RuntimeRoot = filepath.Join(paths.InstanceDir, "runtime")
	return paths, nil
}

func ValidateID(value string) error {
	if value == "" {
		return errors.New("instance ID is required")
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return fmt.Errorf("instance ID contains unsupported character %q", character)
	}
	return nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
