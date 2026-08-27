package gitworkspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/reyoung/pika-go/internal/symphony"
)

const bestBranch = "pika/best"

var (
	identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	shaPattern      = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

type Workspace struct {
	Repository string
	Root       string
}

type Best struct {
	Branch     string `json:"branch"`
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
}

type Round struct {
	AttemptID  string `json:"attempt_id"`
	Round      int64  `json:"round"`
	Branch     string `json:"branch"`
	Repository string `json:"repository"`
	BaseSHA    string `json:"base_sha"`
	HeadSHA    string `json:"head_sha"`
}

type Intent struct {
	ID              string `json:"id"`
	ExpectedBestSHA string `json:"expected_best_sha"`
	CandidateSHA    string `json:"candidate_sha"`
	BestRepository  string `json:"best_repository"`
}

type CommitResult struct {
	CommitSHA string `json:"commit_sha"`
	Clean     bool   `json:"clean"`
	Status    string `json:"status,omitempty"`
}

type RuntimePreparer struct {
	Root string
}

func (p RuntimePreparer) PrepareWork(ctx context.Context, work symphony.RuntimeWork) (symphony.RuntimeWork, error) {
	if work.Work.Role != symphony.RoleIteration && work.Work.Role != symphony.RoleIntegration {
		return work, nil
	}
	workspace := Workspace{Repository: work.OptimizationRepository, Root: p.Root}
	if work.Work.Role == symphony.RoleIteration {
		var round Round
		var err error
		if work.IterationKind == "initial" {
			round, err = workspace.CreateAttempt(ctx, work.Work.AttemptID, work.Work.IterationRound, work.BaseSHA)
		} else {
			round, err = workspace.RefreshFromBest(ctx, work.Work.AttemptID, work.Work.IterationRound, work.CandidateSHA, work.BaseSHA)
		}
		if err != nil {
			return symphony.RuntimeWork{}, err
		}
		work.Repository = round.Repository
		return work, nil
	}
	repository := workspace.roundRepository(work.Work.AttemptID, work.Work.IterationRound)
	if _, err := os.Stat(filepath.Join(repository, ".git")); err != nil {
		return symphony.RuntimeWork{}, fmt.Errorf("Integration worktree is unavailable: %w", err)
	}
	work.Repository = repository
	return work, nil
}

func (w Workspace) SourceHEAD(ctx context.Context) (string, error) {
	if err := w.validate(); err != nil {
		return "", err
	}
	output, err := w.git(ctx, w.Repository, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read source HEAD: %w", err)
	}
	sha := strings.TrimSpace(output)
	if err := validateSHA(sha); err != nil {
		return "", err
	}
	return sha, nil
}

// SourceSnapshot returns the repository HEAD together with the complete
// worktree status used to decide whether that HEAD is a reproducible snapshot.
func (w Workspace) SourceSnapshot(ctx context.Context) (CommitResult, error) {
	if err := w.validate(); err != nil {
		return CommitResult{}, err
	}
	return w.commitResult(ctx)
}

// CommitChanges stages an explicit set of repository-relative literal paths
// and creates an idempotent Pika-owned commit. It exists so sandboxed coding
// agents never need direct write access to Git metadata.
func (w Workspace) CommitChanges(ctx context.Context, scope, idempotencyKey, message string, paths []string) (CommitResult, error) {
	if err := w.validate(); err != nil {
		return CommitResult{}, err
	}
	scope = strings.TrimSpace(scope)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	message = strings.TrimSpace(message)
	if scope == "" || idempotencyKey == "" || message == "" || len(paths) == 0 {
		return CommitResult{}, errors.New("commit scope, idempotency_key, message, and paths are required")
	}

	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" || strings.ContainsRune(path, '\x00') || filepath.IsAbs(path) {
			return CommitResult{}, errors.New("commit paths must be non-empty repository-relative paths")
		}
		clean := filepath.Clean(path)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".git" || strings.HasPrefix(clean, ".git"+string(os.PathSeparator)) {
			return CommitResult{}, errors.New("commit path is outside the assigned repository or targets Git metadata")
		}
		if !pathWithin(w.Repository, filepath.Join(w.Repository, clean)) {
			return CommitResult{}, errors.New("commit path is outside the assigned repository")
		}
		clean = filepath.ToSlash(clean)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		normalized = append(normalized, clean)
	}
	sort.Strings(normalized)
	keyHash := sha256.Sum256([]byte(scope + "\x00" + idempotencyKey))
	requestHash := sha256.Sum256([]byte(message + "\x00" + strings.Join(normalized, "\x00")))
	keyDigest, requestDigest := fmt.Sprintf("%x", keyHash), fmt.Sprintf("%x", requestHash)

	lastKey, _ := w.git(ctx, w.Repository, "show", "-s", "--format=%(trailers:key=Pika-Commit-Key,valueonly)", "HEAD")
	if strings.TrimSpace(lastKey) == keyDigest {
		lastRequest, err := w.git(ctx, w.Repository, "show", "-s", "--format=%(trailers:key=Pika-Commit-Request,valueonly)", "HEAD")
		if err != nil || strings.TrimSpace(lastRequest) != requestDigest {
			return CommitResult{}, errors.New("commit idempotency key was reused with a different request")
		}
		return w.commitResult(ctx)
	}
	staged, err := w.gitBytes(ctx, w.Repository, nil, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return CommitResult{}, fmt.Errorf("inspect pre-existing staged changes: %w", err)
	}
	for _, path := range splitNULPaths(staged) {
		if !authorizedCommitPath(path, normalized) {
			return CommitResult{}, fmt.Errorf("pre-existing staged path %q is outside the authorized paths", path)
		}
	}

	args := []string{"add", "--"}
	for _, path := range normalized {
		args = append(args, ":(top,literal)"+path)
	}
	if _, err := w.git(ctx, w.Repository, args...); err != nil {
		return CommitResult{}, fmt.Errorf("stage scoped changes: %w", err)
	}
	stagedNames, err := w.git(ctx, w.Repository, "diff", "--cached", "--name-only")
	if err != nil {
		return CommitResult{}, fmt.Errorf("inspect staged changes: %w", err)
	}
	if strings.TrimSpace(stagedNames) == "" {
		return CommitResult{}, errors.New("scoped commit has no staged changes")
	}
	trailers := "Pika-Commit-Key: " + keyDigest + "\nPika-Commit-Request: " + requestDigest
	if _, err := w.git(ctx, w.Repository, "commit", "-m", message, "-m", trailers); err != nil {
		return CommitResult{}, fmt.Errorf("commit scoped changes: %w", err)
	}
	return w.commitResult(ctx)
}

func splitNULPaths(value []byte) []string {
	parts := bytes.Split(value, []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) != 0 {
			paths = append(paths, filepath.ToSlash(string(part)))
		}
	}
	return paths
}

func authorizedCommitPath(path string, scopes []string) bool {
	for _, scope := range scopes {
		if path == scope || strings.HasPrefix(path, scope+"/") {
			return true
		}
	}
	return false
}

func (w Workspace) commitResult(ctx context.Context) (CommitResult, error) {
	head, err := w.git(ctx, w.Repository, "rev-parse", "HEAD")
	if err != nil {
		return CommitResult{}, fmt.Errorf("read scoped commit HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	if err := validateSHA(head); err != nil {
		return CommitResult{}, err
	}
	status, err := w.git(ctx, w.Repository, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return CommitResult{}, fmt.Errorf("read scoped commit status: %w", err)
	}
	status = strings.TrimSpace(status)
	return CommitResult{CommitSHA: head, Clean: status == "", Status: status}, nil
}

func (w Workspace) CurrentBest(ctx context.Context) (string, error) {
	if err := w.validate(); err != nil {
		return "", err
	}
	return w.ref(ctx, bestBranch)
}

func (w Workspace) AttemptRepository(attemptID string, round int64) (string, error) {
	if !identityPattern.MatchString(attemptID) || round < 1 {
		return "", errors.New("invalid Attempt Round identity")
	}
	if err := w.validate(); err != nil {
		return "", err
	}
	return w.roundRepository(attemptID, round), nil
}

func (w Workspace) VerifyCandidate(ctx context.Context, repository, baseSHA, candidateSHA string) error {
	if err := w.validate(); err != nil {
		return err
	}
	if err := validateSHA(baseSHA); err != nil {
		return fmt.Errorf("candidate base: %w", err)
	}
	if err := validateSHA(candidateSHA); err != nil {
		return fmt.Errorf("candidate HEAD: %w", err)
	}
	root, err := filepath.Abs(repository)
	if err != nil || !pathWithin(w.Root, root) {
		return errors.New("candidate repository is outside the Pika worktree root")
	}
	head, err := w.git(ctx, root, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != candidateSHA {
		return errors.New("candidate SHA is not the assigned worktree HEAD")
	}
	if _, err := w.git(ctx, root, "merge-base", "--is-ancestor", baseSHA, candidateSHA); err != nil {
		return errors.New("candidate is not descended from its declared base")
	}
	status, err := w.git(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || strings.TrimSpace(status) != "" {
		return errors.New("candidate worktree is not clean")
	}
	return nil
}

func (w Workspace) EnsureBest(ctx context.Context, initialSHA string) (Best, error) {
	if err := w.validate(); err != nil {
		return Best{}, err
	}
	if err := validateSHA(initialSHA); err != nil {
		return Best{}, fmt.Errorf("initial Best: %w", err)
	}
	if _, err := w.git(ctx, w.Repository, "rev-parse", "--verify", initialSHA+"^{commit}"); err != nil {
		return Best{}, fmt.Errorf("resolve initial Best: %w", err)
	}
	current, err := w.ref(ctx, bestBranch)
	if err != nil && !errors.Is(err, errRefMissing) {
		return Best{}, err
	}
	if errors.Is(err, errRefMissing) {
		if _, err := w.git(ctx, w.Repository, "update-ref", "refs/heads/"+bestBranch, initialSHA, strings.Repeat("0", 40)); err != nil {
			return Best{}, fmt.Errorf("create %s: %w", bestBranch, err)
		}
		current = initialSHA
	} else if current != initialSHA {
		return Best{}, fmt.Errorf("%s already exists at %s, not requested initial SHA %s", bestBranch, current, initialSHA)
	}
	repository, err := w.ensureBestWorktree(ctx)
	if err != nil {
		return Best{}, err
	}
	return Best{Branch: bestBranch, Repository: repository, SHA: current}, nil
}

func (w Workspace) CreateAttempt(ctx context.Context, attemptID string, round int64, baseSHA string) (Round, error) {
	if err := w.validateRound(attemptID, round, baseSHA); err != nil {
		return Round{}, err
	}
	branch := attemptBranch(attemptID, round)
	repository := w.roundRepository(attemptID, round)
	if err := os.MkdirAll(filepath.Dir(repository), 0o700); err != nil {
		return Round{}, fmt.Errorf("create Attempt directory: %w", err)
	}
	if _, err := os.Stat(repository); err == nil {
		head, headErr := w.git(ctx, repository, "rev-parse", "HEAD")
		branchName, branchErr := w.git(ctx, repository, "branch", "--show-current")
		if headErr == nil && branchErr == nil && strings.TrimSpace(branchName) == branch {
			return Round{AttemptID: attemptID, Round: round, Branch: branch, Repository: repository, BaseSHA: baseSHA, HeadSHA: strings.TrimSpace(head)}, nil
		}
		return Round{}, fmt.Errorf("Attempt worktree path already exists with different identity: %s", repository)
	} else if !os.IsNotExist(err) {
		return Round{}, fmt.Errorf("inspect Attempt worktree: %w", err)
	}
	if _, err := w.git(ctx, w.Repository, "worktree", "add", "-b", branch, repository, baseSHA); err != nil {
		return Round{}, fmt.Errorf("create Attempt worktree: %w", err)
	}
	return Round{AttemptID: attemptID, Round: round, Branch: branch, Repository: repository, BaseSHA: baseSHA, HeadSHA: baseSHA}, nil
}

func (w Workspace) RefreshFromBest(ctx context.Context, attemptID string, round int64, candidateSHA, bestSHA string) (Round, error) {
	if err := validateSHA(candidateSHA); err != nil {
		return Round{}, fmt.Errorf("refresh candidate: %w", err)
	}
	created, err := w.CreateAttempt(ctx, attemptID, round, candidateSHA)
	if err != nil {
		return Round{}, err
	}
	if err := validateSHA(bestSHA); err != nil {
		return Round{}, fmt.Errorf("refresh Best: %w", err)
	}
	if _, err := w.git(ctx, created.Repository, "merge", "--no-edit", "--no-ff", bestSHA); err != nil {
		return Round{}, fmt.Errorf("merge Best into stale Attempt; worktree preserved for recovery: %w", err)
	}
	head, err := w.git(ctx, created.Repository, "rev-parse", "HEAD")
	if err != nil {
		return Round{}, fmt.Errorf("read refreshed Attempt HEAD: %w", err)
	}
	created.HeadSHA = strings.TrimSpace(head)
	return created, nil
}

func (w Workspace) PrepareBestUpdate(ctx context.Context, intentID, expectedBestSHA, candidateSHA string) (Intent, error) {
	if !identityPattern.MatchString(intentID) {
		return Intent{}, errors.New("Git intent ID contains unsafe characters")
	}
	if err := w.validate(); err != nil {
		return Intent{}, err
	}
	if err := validateSHA(expectedBestSHA); err != nil {
		return Intent{}, fmt.Errorf("expected Best: %w", err)
	}
	if err := validateSHA(candidateSHA); err != nil {
		return Intent{}, fmt.Errorf("candidate: %w", err)
	}
	current, err := w.ref(ctx, bestBranch)
	if err != nil {
		return Intent{}, err
	}
	if current != expectedBestSHA {
		return Intent{}, fmt.Errorf("Best changed: observed %s, expected %s", current, expectedBestSHA)
	}
	if _, err := w.git(ctx, w.Repository, "merge-base", "--is-ancestor", expectedBestSHA, candidateSHA); err != nil {
		return Intent{}, fmt.Errorf("candidate is not based on expected Best: %w", err)
	}
	bestRepository, err := w.ensureBestWorktree(ctx)
	if err != nil {
		return Intent{}, err
	}
	return Intent{ID: intentID, ExpectedBestSHA: expectedBestSHA, CandidateSHA: candidateSHA, BestRepository: bestRepository}, nil
}

func (w Workspace) ApplyBestUpdate(ctx context.Context, intent Intent, message string) (string, error) {
	if err := w.validateIntent(intent); err != nil {
		return "", err
	}
	current, err := w.ref(ctx, bestBranch)
	if err != nil {
		return "", err
	}
	if current != intent.ExpectedBestSHA {
		if err := w.VerifyBestUpdate(ctx, intent, current); err == nil {
			return current, nil
		}
		return "", fmt.Errorf("Best changed before intent %s could be applied", intent.ID)
	}
	unstaged, err := w.git(ctx, intent.BestRepository, "diff", "--quiet")
	if err != nil || strings.TrimSpace(unstaged) != "" {
		return "", errors.New("Best worktree contains uncommitted unstaged changes")
	}
	candidateTree, err := w.tree(ctx, intent.CandidateSHA)
	if err != nil {
		return "", err
	}
	indexTree, indexErr := w.git(ctx, intent.BestRepository, "write-tree")
	if indexErr != nil {
		return "", fmt.Errorf("inspect Best index: %w", indexErr)
	}
	if strings.TrimSpace(indexTree) != candidateTree {
		patch, err := w.gitBytes(ctx, w.Repository, nil, "diff", "--binary", intent.ExpectedBestSHA, intent.CandidateSHA)
		if err != nil {
			return "", fmt.Errorf("create authorized patch: %w", err)
		}
		if len(patch) == 0 {
			return "", errors.New("candidate does not change Best")
		}
		if _, err := w.gitBytes(ctx, intent.BestRepository, patch, "apply", "--index", "--binary", "-"); err != nil {
			return "", fmt.Errorf("apply authorized patch: %w", err)
		}
		indexTree, err = w.git(ctx, intent.BestRepository, "write-tree")
		if err != nil || strings.TrimSpace(indexTree) != candidateTree {
			return "", errors.New("staged Best tree does not equal candidate tree")
		}
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "Apply Pika optimization"
	}
	if _, err := w.git(ctx, intent.BestRepository, "commit", "-m", message, "-m", "Pika-Intent: "+intent.ID); err != nil {
		return "", fmt.Errorf("commit Best update: %w", err)
	}
	applied, err := w.git(ctx, intent.BestRepository, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read applied Best: %w", err)
	}
	applied = strings.TrimSpace(applied)
	if err := w.VerifyBestUpdate(ctx, intent, applied); err != nil {
		return "", err
	}
	return applied, nil
}

// ApplyAuthorizedBestUpdate resolves an already-authorized intent against the
// current Best and applies it idempotently. If a previous process committed the
// exact authorized tree but lost its response, the Git postcondition recovers
// that commit instead of creating a second mutation.
func (w Workspace) ApplyAuthorizedBestUpdate(ctx context.Context, intentID, expectedBestSHA, candidateSHA, message string) (string, error) {
	prepared, err := w.PrepareBestUpdate(ctx, intentID, expectedBestSHA, candidateSHA)
	if err != nil {
		candidate := Intent{
			ID: intentID, ExpectedBestSHA: expectedBestSHA, CandidateSHA: candidateSHA,
			BestRepository: filepath.Join(w.Root, "best", "repo"),
		}
		current, currentErr := w.CurrentBest(ctx)
		if currentErr != nil || w.VerifyBestUpdate(ctx, candidate, current) != nil {
			return "", err
		}
		prepared = candidate
	}
	return w.ApplyBestUpdate(ctx, prepared, message)
}

func (w Workspace) VerifyBestUpdate(ctx context.Context, intent Intent, appliedSHA string) error {
	if err := w.validateIntent(intent); err != nil {
		return err
	}
	if err := validateSHA(appliedSHA); err != nil {
		return fmt.Errorf("applied Best: %w", err)
	}
	current, err := w.ref(ctx, bestBranch)
	if err != nil {
		return err
	}
	if current != appliedSHA {
		return fmt.Errorf("pika/best is %s, not applied SHA %s", current, appliedSHA)
	}
	parents, err := w.git(ctx, w.Repository, "show", "-s", "--format=%P", appliedSHA)
	if err != nil {
		return fmt.Errorf("read applied Best parent: %w", err)
	}
	parentList := strings.Fields(parents)
	if len(parentList) != 1 || parentList[0] != intent.ExpectedBestSHA {
		return fmt.Errorf("applied Best parent is not expected Best %s", intent.ExpectedBestSHA)
	}
	appliedTree, err := w.tree(ctx, appliedSHA)
	if err != nil {
		return err
	}
	candidateTree, err := w.tree(ctx, intent.CandidateSHA)
	if err != nil {
		return err
	}
	if appliedTree != candidateTree {
		return errors.New("applied Best tree does not equal authorized candidate tree")
	}
	trailers, err := w.git(ctx, w.Repository, "show", "-s", "--format=%(trailers:key=Pika-Intent,valueonly)", appliedSHA)
	if err != nil || strings.TrimSpace(trailers) != intent.ID {
		return fmt.Errorf("applied Best is missing Pika intent trailer %s", intent.ID)
	}
	return nil
}

func (w Workspace) validate() error {
	if !filepath.IsAbs(w.Repository) || !filepath.IsAbs(w.Root) {
		return errors.New("Git repository and worktree root must be absolute paths")
	}
	return nil
}

func (w Workspace) validateRound(attemptID string, round int64, baseSHA string) error {
	if err := w.validate(); err != nil {
		return err
	}
	if !identityPattern.MatchString(attemptID) {
		return errors.New("Attempt ID contains unsafe characters")
	}
	if round < 1 {
		return errors.New("Iteration Round must be positive")
	}
	if err := validateSHA(baseSHA); err != nil {
		return fmt.Errorf("Attempt base: %w", err)
	}
	return nil
}

func (w Workspace) validateIntent(intent Intent) error {
	if !identityPattern.MatchString(intent.ID) {
		return errors.New("Git intent ID contains unsafe characters")
	}
	if err := validateSHA(intent.ExpectedBestSHA); err != nil {
		return err
	}
	if err := validateSHA(intent.CandidateSHA); err != nil {
		return err
	}
	want := filepath.Clean(filepath.Join(w.Root, "best", "repo"))
	if filepath.Clean(intent.BestRepository) != want {
		return errors.New("Git intent targets a different Best worktree")
	}
	return nil
}

func validateSHA(sha string) error {
	if !shaPattern.MatchString(sha) {
		return errors.New("Git SHA must be exactly 40 hexadecimal characters")
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func attemptBranch(attemptID string, round int64) string {
	return "pika/attempt/" + attemptID + "/" + strconv.FormatInt(round, 10)
}

func (w Workspace) roundRepository(attemptID string, round int64) string {
	return filepath.Join(w.Root, "attempts", attemptID, "rounds", strconv.FormatInt(round, 10), "repo")
}

func (w Workspace) ensureBestWorktree(ctx context.Context) (string, error) {
	repository := filepath.Join(w.Root, "best", "repo")
	if err := os.MkdirAll(filepath.Dir(repository), 0o700); err != nil {
		return "", fmt.Errorf("create Best directory: %w", err)
	}
	if _, err := os.Stat(repository); err == nil {
		branch, branchErr := w.git(ctx, repository, "branch", "--show-current")
		if branchErr == nil && strings.TrimSpace(branch) == bestBranch {
			return repository, nil
		}
		return "", fmt.Errorf("Best worktree path already exists with different identity: %s", repository)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect Best worktree: %w", err)
	}
	if _, err := w.git(ctx, w.Repository, "worktree", "add", repository, bestBranch); err != nil {
		return "", fmt.Errorf("create Best worktree: %w", err)
	}
	return repository, nil
}

var errRefMissing = errors.New("Git ref does not exist")

func (w Workspace) ref(ctx context.Context, branch string) (string, error) {
	output, err := w.git(ctx, w.Repository, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return "", errRefMissing
	}
	return strings.TrimSpace(output), nil
}

func (w Workspace) tree(ctx context.Context, commit string) (string, error) {
	output, err := w.git(ctx, w.Repository, "rev-parse", commit+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("read Git tree for %s: %w", commit, err)
	}
	return strings.TrimSpace(output), nil
}

func (w Workspace) git(ctx context.Context, repository string, args ...string) (string, error) {
	output, err := w.gitBytes(ctx, repository, nil, args...)
	return string(output), err
}

func (w Workspace) gitBytes(ctx context.Context, repository string, stdin []byte, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", repository}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
