package legacymigration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/instance"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type Roots struct {
	Config string
	State  string
}

type Instance struct {
	ID             string `json:"id"`
	Repository     string `json:"repository,omitempty"`
	ConfigPath     string `json:"config_path,omitempty"`
	DatabasePath   string `json:"database_path,omitempty"`
	SchemaVersion  int    `json:"schema_version,omitempty"`
	Status         string `json:"status,omitempty"`
	Revision       int64  `json:"revision,omitempty"`
	DaemonActive   bool   `json:"daemon_active"`
	Initialization string `json:"initialization"`
}

func DefaultRoots() (Roots, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Roots{}, fmt.Errorf("resolve home directory: %w", err)
	}
	configBase := os.Getenv("XDG_CONFIG_HOME")
	if configBase == "" {
		configBase = filepath.Join(home, ".config")
	}
	stateBase := os.Getenv("XDG_STATE_HOME")
	if stateBase == "" {
		stateBase = filepath.Join(home, ".local", "state")
	}
	return Roots{
		Config: filepath.Join(configBase, "herdr", "plugins", "config", "pika-go"),
		State:  filepath.Join(stateBase, "herdr", "plugins", "pika-go"),
	}, nil
}

func List(ctx context.Context, roots Roots) ([]Instance, error) {
	if err := validateRoots(roots); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, root := range []string{filepath.Join(roots.Config, "instances"), filepath.Join(roots.State, "instances")} {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list legacy instances at %s: %w", root, err)
		}
		for _, entry := range entries {
			if entry.IsDir() && instance.ValidateID(entry.Name()) == nil {
				ids[entry.Name()] = true
			}
		}
	}
	names := make([]string, 0, len(ids))
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)
	result := make([]Instance, 0, len(names))
	for _, id := range names {
		item := inspect(ctx, roots, id)
		result = append(result, item)
	}
	return result, nil
}

type ImportOptions struct {
	Roots         Roots
	InstanceID    string
	WorkspaceRoot string
}

func Import(ctx context.Context, options ImportOptions) (optimizationworkspace.Workspace, error) {
	if err := validateRoots(options.Roots); err != nil {
		return optimizationworkspace.Workspace{}, err
	}
	if err := instance.ValidateID(options.InstanceID); err != nil {
		return optimizationworkspace.Workspace{}, err
	}
	if options.WorkspaceRoot == "" || !filepath.IsAbs(options.WorkspaceRoot) {
		return optimizationworkspace.Workspace{}, errors.New("legacy import Workspace path must be absolute")
	}
	configPath := legacyConfigPath(options.Roots, options.InstanceID)
	databasePath := legacyDatabasePath(options.Roots, options.InstanceID)
	identity, err := configuration.LoadIdentity(configPath)
	if err != nil {
		return optimizationworkspace.Workspace{}, fmt.Errorf("load legacy configuration: %w", err)
	}
	if _, err := os.Stat(databasePath); err != nil {
		return optimizationworkspace.Workspace{}, fmt.Errorf("inspect legacy database: %w", err)
	}
	lock, err := instance.AcquireLock(filepath.Join(options.Roots.State, "instances", options.InstanceID, "daemon.lock"))
	if err != nil {
		return optimizationworkspace.Workspace{}, fmt.Errorf("legacy instance must be stopped before import: %w", err)
	}
	defer lock.Close()

	workspace, err := optimizationworkspace.CreateForImport(ctx, options.WorkspaceRoot, identity.Repository)
	if err != nil {
		return optimizationworkspace.Workspace{}, err
	}
	if err := ensureImportProvenance(workspace, options.InstanceID, configPath, databasePath); err != nil {
		return optimizationworkspace.Workspace{}, err
	}
	if err := workspace.ImportWorkingTree(ctx, identity.Repository, workspace.BaseRepository); err != nil {
		return optimizationworkspace.Workspace{}, fmt.Errorf("import legacy base checkout: %w", err)
	}
	if err := copyFileIfSameOrAbsent(configPath, workspace.ConfigPath, 0o600); err != nil {
		return optimizationworkspace.Workspace{}, fmt.Errorf("import legacy configuration: %w", err)
	}
	legacyConfigDir := filepath.Dir(configPath)
	legacyStateDir := filepath.Dir(databasePath)
	for _, pair := range [][2]string{
		{filepath.Join(legacyConfigDir, "instructions"), workspace.InstructionsRoot},
		{filepath.Join(legacyStateDir, "contexts"), workspace.ContextsRoot},
		{filepath.Join(legacyStateDir, "evidence"), workspace.EvidenceRoot},
		{filepath.Join(legacyStateDir, "logs"), workspace.LogsRoot},
		{filepath.Join(legacyStateDir, "runtime"), workspace.RuntimeRoot},
	} {
		if err := copyDirectoryContents(pair[0], pair[1]); err != nil {
			return optimizationworkspace.Workspace{}, err
		}
	}
	importedWorktrees, err := importLegacyWorktrees(ctx, workspace, filepath.Join(legacyStateDir, "worktrees"))
	if err != nil {
		return optimizationworkspace.Workspace{}, err
	}
	if _, err := os.Stat(workspace.DatabasePath); errors.Is(err, os.ErrNotExist) {
		if err := vacuumInto(ctx, databasePath, workspace.DatabasePath); err != nil {
			return optimizationworkspace.Workspace{}, err
		}
	} else if err != nil {
		return optimizationworkspace.Workspace{}, fmt.Errorf("inspect imported database: %w", err)
	}
	engine, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		return optimizationworkspace.Workspace{}, fmt.Errorf("open imported database: %w", err)
	}
	defer engine.Close()
	manifest := workspace.Identity
	if err := engine.EnsureWorkspaceIdentity(ctx, symphony.WorkspaceIdentity{
		ID: manifest.ID, Root: manifest.Root, SourceRepository: manifest.SourceRepository, GitCommonDir: manifest.GitCommonDir,
		GitCommonDirDevice: manifest.GitCommonDirDevice, GitCommonDirInode: manifest.GitCommonDirInode, InitialSHA: manifest.InitialSHA,
	}); err != nil {
		return optimizationworkspace.Workspace{}, err
	}
	baseSHA, err := git(ctx, workspace.BaseRepository, "rev-parse", "HEAD")
	if err != nil {
		return optimizationworkspace.Workspace{}, err
	}
	importedWorktrees = append(importedWorktrees, symphony.GitWorktreeRecord{
		Role: "base", Branch: workspace.BaseBranch(), Repository: workspace.BaseRepository, HeadSHA: baseSHA, State: "active",
	})
	for _, record := range importedWorktrees {
		if err := engine.UpsertGitWorktree(ctx, record); err != nil {
			return optimizationworkspace.Workspace{}, err
		}
	}
	if err := engine.RetireActiveSessionsForRecovery(ctx); err != nil {
		return optimizationworkspace.Workspace{}, fmt.Errorf("retire imported Agent Sessions: %w", err)
	}
	return workspace, nil
}

func inspect(ctx context.Context, roots Roots, id string) Instance {
	item := Instance{ID: id, ConfigPath: legacyConfigPath(roots, id), DatabasePath: legacyDatabasePath(roots, id), Initialization: "uninitialized"}
	if identity, err := configuration.LoadIdentity(item.ConfigPath); err == nil {
		item.Repository = identity.Repository
	}
	item.DaemonActive = lockHeld(filepath.Join(roots.State, "instances", id, "daemon.lock"))
	if _, err := os.Stat(item.DatabasePath); err != nil {
		return item
	}
	db, err := sql.Open("sqlite", "file:"+item.DatabasePath+"?mode=ro")
	if err != nil {
		item.Initialization = "database_error"
		return item
	}
	defer db.Close()
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&item.SchemaVersion)
	if err := db.QueryRowContext(ctx, `SELECT status, revision FROM optimizations ORDER BY created_at LIMIT 1`).Scan(&item.Status, &item.Revision); err == nil {
		item.Initialization = "initialized"
	}
	return item
}

func importLegacyWorktrees(ctx context.Context, workspace optimizationworkspace.Workspace, legacyRoot string) ([]symphony.GitWorktreeRecord, error) {
	var records []symphony.GitWorktreeRecord
	best := filepath.Join(legacyRoot, "best", "repo")
	if _, err := os.Stat(filepath.Join(best, ".git")); err == nil {
		if err := workspace.ImportLinkedWorktree(ctx, best, workspace.BestRepository, workspace.BestBranch()); err != nil {
			return nil, fmt.Errorf("import legacy Best worktree: %w", err)
		}
		head, err := git(ctx, workspace.BestRepository, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		records = append(records, symphony.GitWorktreeRecord{Role: "best", Branch: workspace.BestBranch(), Repository: workspace.BestRepository, HeadSHA: head, State: "active"})
	}
	attemptRoot := filepath.Join(legacyRoot, "attempts")
	err := filepath.WalkDir(attemptRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || entry.Name() != "repo" {
			return nil
		}
		relative, err := filepath.Rel(attemptRoot, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 4 || parts[1] != "rounds" || instance.ValidateID(parts[0]) != nil {
			return nil
		}
		round, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || round < 1 {
			return nil
		}
		target := filepath.Join(workspace.Root, "attempts", parts[0], "rounds", parts[2], "repo")
		branch := workspace.AttemptBranch(parts[0], round)
		if err := workspace.ImportLinkedWorktree(ctx, path, target, branch); err != nil {
			return fmt.Errorf("import legacy Attempt %s round %d: %w", parts[0], round, err)
		}
		head, err := git(ctx, target, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		records = append(records, symphony.GitWorktreeRecord{Role: "attempt", AttemptID: parts[0], IterationRound: round, Branch: branch, Repository: target, HeadSHA: head, State: "active"})
		return filepath.SkipDir
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("walk legacy worktrees: %w", err)
	}
	return records, nil
}

func vacuumInto(ctx context.Context, source, target string) error {
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return fmt.Errorf("open legacy database for backup: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("configure legacy database backup: %w", err)
	}
	quoted := strings.ReplaceAll(target, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return fmt.Errorf("create consistent imported database: %w", err)
	}
	return os.Chmod(target, 0o600)
}

func copyDirectoryContents(source, target string) error {
	entries, err := os.ReadDir(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy directory %s: %w", source, err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyPath(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(source)
		if err != nil {
			return err
		}
		_ = os.Remove(target)
		return os.Symlink(link, target)
	}
	if info.IsDir() {
		if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
			return err
		}
		return copyDirectoryContents(source, target)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported legacy artifact: %s", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(target), ".legacy-import-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, target)
}

func copyFileIfSameOrAbsent(source, target string, mode os.FileMode) error {
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if existing, err := os.ReadFile(target); err == nil {
		if string(existing) != string(contents) {
			return errors.New("import target already contains different user configuration")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".legacy-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, target)
}

type importProvenance struct {
	Version      int    `json:"version"`
	InstanceID   string `json:"instance_id"`
	ConfigPath   string `json:"config_path"`
	DatabasePath string `json:"database_path"`
}

func ensureImportProvenance(workspace optimizationworkspace.Workspace, instanceID, configPath, databasePath string) error {
	path := filepath.Join(workspace.Root, "legacy-import.json")
	want := importProvenance{Version: 1, InstanceID: instanceID, ConfigPath: configPath, DatabasePath: databasePath}
	if contents, err := os.ReadFile(path); err == nil {
		var got importProvenance
		if json.Unmarshal(contents, &got) != nil || got != want {
			return errors.New("Workspace belongs to a different legacy import")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read legacy import provenance: %w", err)
	}
	if _, err := os.Stat(workspace.DatabasePath); err == nil {
		return errors.New("Workspace already contains a database without matching legacy import provenance")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Workspace database before legacy import: %w", err)
	}
	contents, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(workspace.Root, ".legacy-import-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func lockHeld(path string) bool {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return true
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return false
}

func validateRoots(roots Roots) error {
	if roots.Config == "" || roots.State == "" || !filepath.IsAbs(roots.Config) || !filepath.IsAbs(roots.State) {
		return errors.New("absolute legacy config and state roots are required")
	}
	return nil
}

func legacyConfigPath(roots Roots, id string) string {
	return filepath.Join(roots.Config, "instances", id, "config.toml")
}

func legacyDatabasePath(roots Roots, id string) string {
	return filepath.Join(roots.State, "instances", id, "pika.db")
}

func git(ctx context.Context, repository string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
