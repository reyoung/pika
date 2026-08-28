package optimizationworkspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ImportWorkingTree reproduces the source checkout's HEAD, private index, and
// working files inside the Workspace base worktree. The source checkout is
// read-only; the only removals occur inside the newly created base worktree.
func (w Workspace) ImportWorkingTree(ctx context.Context, source, target string) error {
	if err := w.ValidateSource(ctx); err != nil {
		return err
	}
	resolvedSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve imported source worktree: %w", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("resolve imported target worktree: %w", err)
	}
	if filepath.Clean(resolvedTarget) != filepath.Clean(w.BaseRepository) {
		return errors.New("import target must be the Workspace base worktree")
	}
	return mirrorWorkingTree(ctx, resolvedSource, resolvedTarget)
}

// ImportLinkedWorktree recreates one stopped legacy Pika worktree under the
// new Workspace namespace and then preserves its index and working files.
func (w Workspace) ImportLinkedWorktree(ctx context.Context, source, target, branch string) error {
	if err := w.ValidateSource(ctx); err != nil {
		return err
	}
	if !strings.HasPrefix(branch, w.BranchNamespace()+"/") {
		return errors.New("imported branch is outside the Workspace namespace")
	}
	absoluteTarget, err := filepath.Abs(target)
	if err != nil || !pathWithin(w.Root, absoluteTarget) || filepath.Clean(absoluteTarget) == filepath.Clean(w.Root) {
		return errors.New("imported linked worktree target is outside the Optimization Workspace")
	}
	resolvedSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve imported linked worktree: %w", err)
	}
	commonDir, err := resolvedCommonDir(ctx, resolvedSource)
	if err != nil || commonDir != w.Identity.GitCommonDir {
		return errors.New("imported linked worktree has a different Git common directory")
	}
	head, err := git(ctx, resolvedSource, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read imported linked worktree HEAD: %w", err)
	}
	if _, err := os.Stat(absoluteTarget); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(absoluteTarget), 0o700); err != nil {
			return fmt.Errorf("create imported worktree parent: %w", err)
		}
		if _, branchErr := git(ctx, w.Identity.SourceRepository, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); branchErr == nil {
			if _, err := git(ctx, w.Identity.SourceRepository, "worktree", "add", absoluteTarget, branch); err != nil {
				return fmt.Errorf("restore imported linked worktree: %w", err)
			}
		} else if _, err := git(ctx, w.Identity.SourceRepository, "worktree", "add", "-b", branch, absoluteTarget, head); err != nil {
			return fmt.Errorf("create imported linked worktree: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect imported linked worktree target: %w", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(absoluteTarget)
	if err != nil {
		return fmt.Errorf("resolve imported linked worktree target: %w", err)
	}
	targetBranch, err := git(ctx, resolvedTarget, "branch", "--show-current")
	if err != nil || targetBranch != branch {
		return errors.New("imported linked worktree target has a different branch")
	}
	return mirrorWorkingTree(ctx, resolvedSource, resolvedTarget)
}

func mirrorWorkingTree(ctx context.Context, resolvedSource, resolvedTarget string) error {
	sourceHead, err := git(ctx, resolvedSource, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read imported source HEAD: %w", err)
	}
	targetHead, err := git(ctx, resolvedTarget, "rev-parse", "HEAD")
	if err != nil || targetHead != sourceHead {
		return fmt.Errorf("import target HEAD does not match source HEAD %s", sourceHead)
	}
	before, err := gitBytes(ctx, resolvedSource, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("read imported source status: %w", err)
	}
	entries, err := os.ReadDir(resolvedTarget)
	if err != nil {
		return fmt.Errorf("read import target: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(resolvedTarget, entry.Name())); err != nil {
			return fmt.Errorf("clear imported target entry %s: %w", entry.Name(), err)
		}
	}
	sourceEntries, err := os.ReadDir(resolvedSource)
	if err != nil {
		return fmt.Errorf("read imported source: %w", err)
	}
	for _, entry := range sourceEntries {
		if entry.Name() == ".git" {
			continue
		}
		if err := copyTree(filepath.Join(resolvedSource, entry.Name()), filepath.Join(resolvedTarget, entry.Name())); err != nil {
			return err
		}
	}
	sourceIndex, err := git(ctx, resolvedSource, "rev-parse", "--git-path", "index")
	if err != nil {
		return fmt.Errorf("resolve imported source index: %w", err)
	}
	targetIndex, err := git(ctx, resolvedTarget, "rev-parse", "--git-path", "index")
	if err != nil {
		return fmt.Errorf("resolve imported target index: %w", err)
	}
	sourceIndex = gitPath(resolvedSource, sourceIndex)
	targetIndex = gitPath(resolvedTarget, targetIndex)
	if err := copyRegularFile(sourceIndex, targetIndex, 0o600); err != nil {
		return fmt.Errorf("copy imported Git index: %w", err)
	}
	after, err := gitBytes(ctx, resolvedTarget, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("verify imported target status: %w", err)
	}
	if !bytes.Equal(before, after) {
		return errors.New("imported working tree status does not match the legacy checkout")
	}
	return nil
}

func gitBytes(ctx context.Context, repository string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", repository}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	return command.CombinedOutput()
}

func gitPath(repository, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(repository, strings.TrimSpace(path)))
}

func copyTree(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect imported path %s: %w", source, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(source)
		if err != nil {
			return fmt.Errorf("read imported symlink %s: %w", source, err)
		}
		if err := os.Symlink(link, target); err != nil {
			return fmt.Errorf("create imported symlink %s: %w", target, err)
		}
		return nil
	case info.IsDir():
		if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
			return fmt.Errorf("create imported directory %s: %w", target, err)
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return fmt.Errorf("read imported directory %s: %w", source, err)
		}
		for _, entry := range entries {
			if err := copyTree(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(target, info.Mode().Perm())
	case info.Mode().IsRegular():
		return copyRegularFile(source, target, info.Mode().Perm())
	default:
		return fmt.Errorf("unsupported imported filesystem entry: %s", source)
	}
}

func copyRegularFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}
