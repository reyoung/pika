package optimizationworkspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	ManifestName  = "workspace.json"
	ConfigName    = "pika.toml"
	formatVersion = 1
)

var (
	workspaceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	shaPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type Identity struct {
	Version            int    `json:"version"`
	ID                 string `json:"id"`
	Root               string `json:"root"`
	SourceRepository   string `json:"source_repository"`
	GitCommonDir       string `json:"git_common_dir"`
	GitCommonDirDevice uint64 `json:"git_common_dir_device"`
	GitCommonDirInode  uint64 `json:"git_common_dir_inode"`
	InitialSHA         string `json:"initial_sha"`
	Phase              string `json:"phase"`
}

type Workspace struct {
	Root             string
	Identity         Identity
	ManifestPath     string
	ConfigPath       string
	DatabasePath     string
	LockPath         string
	BaseRepository   string
	BestRepository   string
	InstructionsRoot string
	ContextsRoot     string
	EvidenceRoot     string
	LogsRoot         string
	RuntimeRoot      string
	HerdrRoot        string
	HerdrBindingPath string
}

type HerdrBinding struct {
	Version      int    `json:"version"`
	SocketPath   string `json:"socket_path"`
	WorkspaceID  string `json:"workspace_id"`
	TabID        string `json:"tab_id"`
	ControlPane  string `json:"control_pane"`
	DaemonPane   string `json:"daemon_pane"`
	DaemonSocket string `json:"daemon_socket"`
	UpdatedAt    string `json:"updated_at"`
}

func DefaultRoot(repository string) (string, error) {
	if repository == "" {
		return "", errors.New("source repository is required")
	}
	absolute, err := filepath.Abs(repository)
	if err != nil {
		return "", fmt.Errorf("resolve source repository: %w", err)
	}
	absolute = filepath.Clean(absolute)
	return filepath.Join(filepath.Dir(absolute), filepath.Base(absolute)+"-pika-workspace"), nil
}

func Discover(start string) (Workspace, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return Workspace{}, fmt.Errorf("resolve current directory: %w", err)
		}
	}
	absolute, err := filepath.Abs(start)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve Workspace discovery path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err == nil && !info.IsDir() {
		absolute = filepath.Dir(absolute)
	}
	for candidate := filepath.Clean(absolute); ; candidate = filepath.Dir(candidate) {
		if _, err := os.Stat(filepath.Join(candidate, ManifestName)); err == nil {
			return Open(candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Workspace{}, fmt.Errorf("inspect Workspace marker: %w", err)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
	}
	return Workspace{}, errors.New("Optimization Workspace was not found")
}

func Create(ctx context.Context, root, repository string) (Workspace, error) {
	return create(ctx, root, repository, true)
}

// CreateForImport creates a Workspace for an explicitly stopped legacy
// instance. Unlike normal creation, the source checkout may be dirty because
// ImportWorkingTree will reproduce that exact state in the new base worktree.
func CreateForImport(ctx context.Context, root, repository string) (Workspace, error) {
	return create(ctx, root, repository, false)
}

func create(ctx context.Context, root, repository string, requireClean bool) (Workspace, error) {
	if root == "" || repository == "" {
		return Workspace{}, errors.New("Workspace root and source repository are required")
	}
	absoluteRoot, err := canonicalPath(root)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve Workspace root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(absoluteRoot, ManifestName)); err == nil {
		workspace, openErr := Open(absoluteRoot)
		if openErr != nil {
			return Workspace{}, openErr
		}
		resolvedRepository, _, _, _, validateErr := inspectRepository(ctx, repository, false)
		if validateErr != nil {
			return Workspace{}, validateErr
		}
		if workspace.Identity.SourceRepository != resolvedRepository {
			return Workspace{}, fmt.Errorf("Workspace source repository is %s, not %s", workspace.Identity.SourceRepository, resolvedRepository)
		}
		if err := workspace.EnsureBase(ctx); err != nil {
			return Workspace{}, err
		}
		return workspace, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, fmt.Errorf("inspect Workspace marker: %w", err)
	}

	resolvedRepository, commonDir, device, inode, err := inspectRepository(ctx, repository, requireClean)
	if err != nil {
		return Workspace{}, err
	}
	if pathWithin(resolvedRepository, absoluteRoot) || pathWithin(absoluteRoot, resolvedRepository) {
		return Workspace{}, errors.New("Optimization Workspace must be outside the source repository")
	}
	if pathWithin(commonDir, absoluteRoot) || pathWithin(absoluteRoot, commonDir) {
		return Workspace{}, errors.New("Optimization Workspace must not overlap the Git common directory")
	}
	if entries, readErr := os.ReadDir(absoluteRoot); readErr == nil && len(entries) != 0 {
		return Workspace{}, fmt.Errorf("Workspace destination is not empty: %s", absoluteRoot)
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return Workspace{}, fmt.Errorf("inspect Workspace destination: %w", readErr)
	}
	if err := os.MkdirAll(absoluteRoot, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("create Workspace root: %w", err)
	}
	if err := os.Chmod(absoluteRoot, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("protect Workspace root: %w", err)
	}
	id, err := newID()
	if err != nil {
		return Workspace{}, err
	}
	initialSHA, err := git(ctx, resolvedRepository, "rev-parse", "HEAD")
	if err != nil || !shaPattern.MatchString(initialSHA) {
		return Workspace{}, fmt.Errorf("read source repository HEAD: %v", err)
	}
	identity := Identity{
		Version: formatVersion, ID: id, Root: absoluteRoot, SourceRepository: resolvedRepository,
		GitCommonDir: commonDir, GitCommonDirDevice: device, GitCommonDirInode: inode,
		InitialSHA: initialSHA, Phase: "initializing",
	}
	workspace := fromIdentity(identity)
	if err := writeJSONAtomic(workspace.ManifestPath, identity, 0o600); err != nil {
		return Workspace{}, err
	}
	if err := workspace.ensureDirectories(); err != nil {
		return Workspace{}, err
	}
	if err := workspace.EnsureBase(ctx); err != nil {
		return Workspace{}, err
	}
	workspace.Identity.Phase = "ready_for_init"
	return workspace, nil
}

func Open(root string) (Workspace, error) {
	if root == "" {
		return Workspace{}, errors.New("Workspace root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve Workspace root: %w", err)
	}
	absolute = filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve Workspace root: %w", err)
	}
	manifestPath := filepath.Join(resolved, ManifestName)
	file, err := os.Open(manifestPath)
	if err != nil {
		return Workspace{}, fmt.Errorf("open Workspace manifest: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var identity Identity
	if err := decoder.Decode(&identity); err != nil {
		return Workspace{}, fmt.Errorf("decode Workspace manifest: %w", err)
	}
	if identity.Version != formatVersion || !workspaceIDPattern.MatchString(identity.ID) || !shaPattern.MatchString(identity.InitialSHA) {
		return Workspace{}, errors.New("Workspace manifest has an unsupported identity")
	}
	if filepath.Clean(identity.Root) != resolved {
		return Workspace{}, fmt.Errorf("Optimization Workspace was moved from %s to %s; relocation is not supported", identity.Root, resolved)
	}
	if !filepath.IsAbs(identity.SourceRepository) || !filepath.IsAbs(identity.GitCommonDir) {
		return Workspace{}, errors.New("Workspace repository identity must use absolute paths")
	}
	workspace := fromIdentity(identity)
	if err := workspace.ensureDirectories(); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

func (w Workspace) BaseBranch() string      { return "pika/" + w.Identity.ID + "/base" }
func (w Workspace) BestBranch() string      { return "pika/" + w.Identity.ID + "/best" }
func (w Workspace) BranchNamespace() string { return "pika/" + w.Identity.ID }

func (w Workspace) AttemptBranch(attemptID string, round int64) string {
	return fmt.Sprintf("%s/attempt/%s/%d", w.BranchNamespace(), attemptID, round)
}

func (w Workspace) WriteHerdrBinding(binding HerdrBinding) error {
	if binding.WorkspaceID == "" || binding.TabID == "" || binding.ControlPane == "" || binding.DaemonPane == "" || binding.DaemonSocket == "" {
		return errors.New("complete Herdr binding is required")
	}
	binding.Version = 1
	if binding.UpdatedAt == "" {
		return errors.New("Herdr binding updated_at is required")
	}
	return writeJSONAtomic(w.HerdrBindingPath, binding, 0o600)
}

func (w Workspace) ReadHerdrBinding() (HerdrBinding, error) {
	file, err := os.Open(w.HerdrBindingPath)
	if err != nil {
		return HerdrBinding{}, fmt.Errorf("open Herdr binding: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var binding HerdrBinding
	if err := decoder.Decode(&binding); err != nil {
		return HerdrBinding{}, fmt.Errorf("decode Herdr binding: %w", err)
	}
	if binding.Version != 1 || binding.WorkspaceID == "" || binding.DaemonSocket == "" {
		return HerdrBinding{}, errors.New("Herdr binding is incomplete or unsupported")
	}
	return binding, nil
}

func (w Workspace) EnsureBase(ctx context.Context) error {
	if err := w.ValidateSource(ctx); err != nil {
		return err
	}
	if _, err := os.Stat(w.BaseRepository); err == nil {
		branch, branchErr := git(ctx, w.BaseRepository, "branch", "--show-current")
		commonDir, commonErr := resolvedCommonDir(ctx, w.BaseRepository)
		if branchErr != nil || commonErr != nil || branch != w.BaseBranch() || commonDir != w.Identity.GitCommonDir {
			return fmt.Errorf("base worktree path has a different Git identity: %s", w.BaseRepository)
		}
		return w.markPhase("ready_for_init")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect base worktree: %w", err)
	}
	if _, err := git(ctx, w.Identity.SourceRepository, "show-ref", "--verify", "--quiet", "refs/heads/"+w.BaseBranch()); err == nil {
		if _, err := git(ctx, w.Identity.SourceRepository, "worktree", "add", w.BaseRepository, w.BaseBranch()); err != nil {
			return fmt.Errorf("restore base worktree: %w", err)
		}
	} else {
		if _, err := git(ctx, w.Identity.SourceRepository, "worktree", "add", "-b", w.BaseBranch(), w.BaseRepository, w.Identity.InitialSHA); err != nil {
			return fmt.Errorf("create base worktree: %w", err)
		}
	}
	branch, err := git(ctx, w.BaseRepository, "branch", "--show-current")
	if err != nil || branch != w.BaseBranch() {
		return errors.New("created base worktree has the wrong branch")
	}
	return w.markPhase("ready_for_init")
}

func (w Workspace) ValidateSource(ctx context.Context) error {
	resolvedRepository, commonDir, device, inode, err := inspectRepository(ctx, w.Identity.SourceRepository, false)
	if err != nil {
		return err
	}
	if resolvedRepository != w.Identity.SourceRepository || commonDir != w.Identity.GitCommonDir || device != w.Identity.GitCommonDirDevice || inode != w.Identity.GitCommonDirInode {
		return errors.New("source repository or Git common directory identity changed")
	}
	return nil
}

func (w Workspace) markPhase(phase string) error {
	if w.Identity.Phase == phase {
		return nil
	}
	w.Identity.Phase = phase
	return writeJSONAtomic(w.ManifestPath, w.Identity, 0o600)
}

func (w Workspace) ensureDirectories() error {
	for _, path := range []string{w.InstructionsRoot, w.ContextsRoot, w.EvidenceRoot, w.LogsRoot, w.RuntimeRoot, w.HerdrRoot, filepath.Dir(w.BestRepository), filepath.Join(w.Root, "attempts")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create Workspace directory %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("protect Workspace directory %s: %w", path, err)
		}
	}
	return nil
}

func fromIdentity(identity Identity) Workspace {
	root := identity.Root
	return Workspace{
		Root: root, Identity: identity, ManifestPath: filepath.Join(root, ManifestName), ConfigPath: filepath.Join(root, ConfigName),
		DatabasePath: filepath.Join(root, "pika.db"), LockPath: filepath.Join(root, ".pika.lock"), BaseRepository: filepath.Join(root, "repo"),
		BestRepository: filepath.Join(root, "best", "repo"), InstructionsRoot: filepath.Join(root, "instructions"), ContextsRoot: filepath.Join(root, "contexts"),
		EvidenceRoot: filepath.Join(root, "evidence"), LogsRoot: filepath.Join(root, "logs"), RuntimeRoot: filepath.Join(root, "runtime"),
		HerdrRoot: filepath.Join(root, "herdr"), HerdrBindingPath: filepath.Join(root, "herdr", "binding.json"),
	}
}

func inspectRepository(ctx context.Context, repository string, requireClean bool) (string, string, uint64, uint64, error) {
	absolute, err := filepath.Abs(repository)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("resolve source repository: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("resolve source repository: %w", err)
	}
	top, err := git(ctx, resolved, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", 0, 0, errors.New("source repository is not a Git worktree")
	}
	resolvedTop, err := filepath.EvalSymlinks(top)
	if err != nil || filepath.Clean(resolvedTop) != filepath.Clean(resolved) {
		return "", "", 0, 0, fmt.Errorf("source repository must be the Git worktree root: %s", top)
	}
	if requireClean {
		status, statusErr := git(ctx, resolved, "status", "--porcelain=v1", "--untracked-files=all")
		if statusErr != nil {
			return "", "", 0, 0, fmt.Errorf("inspect source repository status: %w", statusErr)
		}
		if status != "" {
			return "", "", 0, 0, errors.New("source repository must be clean before creating an Optimization Workspace")
		}
	}
	commonDir, err := resolvedCommonDir(ctx, resolved)
	if err != nil {
		return "", "", 0, 0, err
	}
	info, err := os.Stat(commonDir)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("inspect Git common directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", 0, 0, errors.New("read Git common directory filesystem identity")
	}
	return resolved, commonDir, uint64(stat.Dev), uint64(stat.Ino), nil
}

func resolvedCommonDir(ctx context.Context, repository string) (string, error) {
	commonDir, err := git(ctx, repository, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repository, commonDir)
	}
	commonDir, err = filepath.EvalSymlinks(filepath.Clean(commonDir))
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	return commonDir, nil
}

func git(ctx context.Context, repository string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Workspace manifest: %w", err)
	}
	contents = append(contents, '\n')
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".workspace-*.tmp")
	if err != nil {
		return fmt.Errorf("create Workspace manifest temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("protect Workspace manifest: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write Workspace manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync Workspace manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Workspace manifest: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install Workspace manifest: %w", err)
	}
	remove = false
	return nil
}

func newID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate Workspace ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	candidate := absolute
	var suffix []string
	for {
		if _, err := os.Lstat(candidate); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return absolute, nil
		}
		suffix = append(suffix, filepath.Base(candidate))
		candidate = parent
	}
}
