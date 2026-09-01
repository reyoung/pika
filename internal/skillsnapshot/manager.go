// Package skillsnapshot acquires and validates the two frozen KDA skill
// repositories used by a flow-v2 Optimization. It deliberately owns all Git
// and publication mechanics so callers receive only immutable provenance.
package skillsnapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/reyoung/pika-go/internal/symphony"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const validationVersion = 1

//go:embed schemas/pika-snapshot-v1.schema.json
var manifestSchemaFS embed.FS

// ManifestSchema exposes the canonical Draft 2020-12 wire contract for the
// immutable pika-snapshot.json artifact. It is intentionally published beside
// the Context schema rather than hidden in Go structs.
func ManifestSchema() ([]byte, error) {
	contents, err := manifestSchemaFS.ReadFile("schemas/pika-snapshot-v1.schema.json")
	if err != nil {
		return nil, err
	}
	return append([]byte{}, contents...), nil
}

// ValidateManifest validates the public manifest wire format with the same
// standards-compliant Draft 2020-12 engine used by the KDA MCP contracts.
// The caller additionally verifies the snapshot_id digest relation.
func ValidateManifest(contents []byte) error {
	schemaBytes, err := ManifestSchema()
	if err != nil {
		return fmt.Errorf("read skill snapshot schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	schemaValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return fmt.Errorf("decode skill snapshot schema: %w", err)
	}
	if err := compiler.AddResource("pika-snapshot-v1.schema.json", schemaValue); err != nil {
		return fmt.Errorf("register skill snapshot schema: %w", err)
	}
	compiled, err := compiler.Compile("pika-snapshot-v1.schema.json")
	if err != nil {
		return fmt.Errorf("compile skill snapshot schema: %w", err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(contents))
	if err != nil {
		return fmt.Errorf("decode skill snapshot manifest: %w", err)
	}
	if err := compiled.Validate(value); err != nil {
		return fmt.Errorf("invalid skill snapshot manifest schema: %w", err)
	}
	return nil
}

type source struct {
	Name       string
	Repository string
	Branch     string
}

func productSources() []source {
	return []source{
		{Name: "KernelWiki", Repository: symphony.KernelWikiRepository, Branch: symphony.KernelWikiBranch},
		{Name: "ncu-report-skill", Repository: symphony.NCUReportSkillRepository, Branch: symphony.NCUReportSkillBranch},
	}
}

type Request struct {
	// WorkspaceRoot is the Optimization Workspace, not its source repository.
	// Published snapshots live only below its toolkit root.
	WorkspaceRoot string
}

type Snapshot struct {
	Input     symphony.SkillSnapshotInput
	Published bool
}

// RollbackPublication removes only a snapshot newly published by this Prepare
// call. Reused snapshots are durable provenance and are never removed.
func (snapshot Snapshot) RollbackPublication() error {
	if !snapshot.Published {
		return nil
	}
	if err := Verify(snapshot.Input); err != nil {
		return fmt.Errorf("refuse to rollback unverified skill snapshot: %w", err)
	}
	root := filepath.Clean(snapshot.Input.RootPath)
	if filepath.Base(root) != snapshot.Input.SnapshotID || filepath.Base(filepath.Dir(root)) != "kda-skills" {
		return errors.New("refuse to rollback skill snapshot outside owned publication root")
	}
	if err := makeWritable(root); err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("rollback published skill snapshot: %w", err)
	}
	return nil
}

// InputFromView reconstructs the exact immutable Init input that was already
// published. It is used only for an idempotent Init replay: resolving sources
// again would produce a new resolved_at timestamp and therefore a different
// snapshot_id under the canonical manifest contract.
func InputFromView(view symphony.SkillSnapshotView) (symphony.SkillSnapshotInput, error) {
	if err := VerifyView(view); err != nil {
		return symphony.SkillSnapshotInput{}, err
	}
	manifest, err := os.ReadFile(filepath.Join(view.RootPath, "pika-snapshot.json"))
	if err != nil {
		return symphony.SkillSnapshotInput{}, fmt.Errorf("read published skill snapshot manifest: %w", err)
	}
	input := symphony.SkillSnapshotInput{SchemaVersion: view.SchemaVersion, SnapshotID: view.SnapshotID, RootPath: view.RootPath,
		Manifest: bytesTrimSpace(manifest), ManifestSHA256: view.ManifestSHA256, Entries: append([]symphony.SkillSnapshotEntry{}, view.Entries...)}
	if err := Verify(input); err != nil {
		return symphony.SkillSnapshotInput{}, err
	}
	return input, nil
}

// Verify rechecks an already-persisted snapshot without contacting a remote.
// Recovery and every coding activation use this path, so a daemon restart can
// never silently pick up a moved upstream branch or repair corrupted content.
func Verify(input symphony.SkillSnapshotInput) error {
	if input.SchemaVersion != symphony.SkillSnapshotSchemaV1 || !isSHA256(input.SnapshotID) || !filepath.IsAbs(input.RootPath) || len(input.Manifest) == 0 || len(input.Entries) != 2 {
		return errors.New("frozen skill snapshot identity is incomplete")
	}
	digest := sha256.Sum256(input.Manifest)
	if hex.EncodeToString(digest[:]) != input.ManifestSHA256 {
		return errors.New("frozen skill snapshot manifest digest differs")
	}
	resolved, err := filepath.EvalSymlinks(input.RootPath)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(input.RootPath) {
		return errors.New("frozen skill snapshot root is unavailable or redirected")
	}
	if err := validatePublished(input.RootPath, input.SnapshotID, input.Entries, input.ManifestSHA256); err != nil {
		return fmt.Errorf("verify frozen skill snapshot: %w", err)
	}
	published, err := os.ReadFile(filepath.Join(input.RootPath, "pika-snapshot.json"))
	if err != nil || string(bytesTrimSpace(published)) != string(bytesTrimSpace(input.Manifest)) {
		return errors.New("persisted skill snapshot manifest differs from published manifest")
	}
	return nil
}

// VerifyView is the recovery-facing counterpart of Verify. SQLite deliberately
// stores only the manifest digest in normal projections; the manifest itself
// remains the immutable file below the snapshot root.
func VerifyView(snapshot symphony.SkillSnapshotView) error {
	if snapshot.SchemaVersion != symphony.SkillSnapshotSchemaV1 || !isSHA256(snapshot.SnapshotID) || !filepath.IsAbs(snapshot.RootPath) || len(snapshot.Entries) != 2 {
		return errors.New("frozen skill snapshot view is incomplete")
	}
	if err := validatePublished(snapshot.RootPath, snapshot.SnapshotID, snapshot.Entries, snapshot.ManifestSHA256); err != nil {
		return fmt.Errorf("verify frozen skill snapshot view: %w", err)
	}
	return nil
}

// Git is the internal seam used by deterministic fake-remotes tests. Every
// argument is passed as an argv element; neither the production implementation
// nor the Manager invokes a shell.
type Git interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type commandGit struct{}

func (commandGit) Run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type Manager struct {
	Git     Git
	sources []source // test-only dependency injection; not reachable by production callers.
	Now     func() time.Time
}

func (m Manager) Prepare(ctx context.Context, request Request) (Snapshot, error) {
	if request.WorkspaceRoot == "" || !filepath.IsAbs(request.WorkspaceRoot) {
		return Snapshot{}, errors.New("absolute Optimization Workspace root is required")
	}
	workspaceRoot, err := filepath.EvalSymlinks(request.WorkspaceRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve Optimization Workspace root: %w", err)
	}
	if info, err := os.Stat(workspaceRoot); err != nil || !info.IsDir() {
		return Snapshot{}, errors.New("Optimization Workspace root is not a directory")
	}
	sources := m.configuredSources()
	if err := validateSources(sources); err != nil {
		return Snapshot{}, err
	}
	git := m.Git
	if git == nil {
		git = commandGit{}
	}
	now := m.Now
	if now == nil {
		now = time.Now
	}

	resolved := make([]resolvedSource, 0, len(sources))
	for _, source := range sources {
		sha, err := resolveHead(ctx, git, source)
		if err != nil {
			return Snapshot{}, err
		}
		resolved = append(resolved, resolvedSource{source: source, CommitSHA: sha})
	}

	toolkits := filepath.Join(workspaceRoot, "toolkits", "kda-skills")
	if err := ensureDirectory(toolkits, 0o700); err != nil {
		return Snapshot{}, err
	}
	staging, err := os.MkdirTemp(toolkits, ".staging-")
	if err != nil {
		return Snapshot{}, fmt.Errorf("create skill snapshot staging directory: %w", err)
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		_ = os.RemoveAll(staging)
		return Snapshot{}, fmt.Errorf("protect skill snapshot staging directory: %w", err)
	}
	keepStaging := false
	defer func() {
		if !keepStaging {
			_ = os.RemoveAll(staging)
		}
	}()

	entries := make([]symphony.SkillSnapshotEntry, 0, len(resolved))
	for _, source := range resolved {
		destination := filepath.Join(staging, "skills", source.Name)
		if err := cloneDetached(ctx, git, source, destination); err != nil {
			return Snapshot{}, err
		}
		if err := validateSkillRoot(destination, source.Name); err != nil {
			return Snapshot{}, err
		}
		contentDigest, err := contentDigest(destination)
		if err != nil {
			return Snapshot{}, err
		}
		entries = append(entries, symphony.SkillSnapshotEntry{
			Name: source.Name, Repository: publishedRepository(source.source), Branch: source.Branch, CommitSHA: source.CommitSHA,
			RelativePath: filepath.ToSlash(filepath.Join("skills", source.Name)), ContentSHA256: contentDigest,
		})
	}

	resolvedAt := now().UTC().Format(time.RFC3339Nano)
	identity := canonicalManifest{SchemaVersion: symphony.SkillSnapshotSchemaV1, ValidationVersion: validationVersion, ResolvedAt: resolvedAt, Skills: manifestEntries(entries)}
	canonicalIdentity, err := json.Marshal(identity)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode skill snapshot identity: %w", err)
	}
	snapshotDigest := sha256.Sum256(canonicalIdentity)
	snapshotID := hex.EncodeToString(snapshotDigest[:])
	fullManifest, err := json.Marshal(manifest{SchemaVersion: symphony.SkillSnapshotSchemaV1, ValidationVersion: validationVersion, SnapshotID: snapshotID, ResolvedAt: resolvedAt, Skills: manifestEntries(entries)})
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode published skill snapshot manifest: %w", err)
	}
	if err := ValidateManifest(fullManifest); err != nil {
		return Snapshot{}, err
	}
	manifestDigest := sha256.Sum256(fullManifest)
	if err := writeFile(filepath.Join(staging, "pika-snapshot.json"), append(fullManifest, '\n'), 0o600); err != nil {
		return Snapshot{}, err
	}
	if err := writeFile(filepath.Join(staging, "plugin.json"), pluginManifest(), 0o600); err != nil {
		return Snapshot{}, err
	}
	if err := makeReadOnly(staging); err != nil {
		return Snapshot{}, err
	}

	finalRoot := filepath.Join(toolkits, snapshotID)
	if _, err := os.Lstat(finalRoot); err == nil {
		existing, err := readPublishedSnapshot(finalRoot, snapshotID, entries)
		if err != nil {
			return Snapshot{}, fmt.Errorf("existing frozen skill snapshot is invalid: %w", err)
		}
		return Snapshot{Input: existing}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, fmt.Errorf("inspect existing frozen skill snapshot: %w", err)
	} else if err := os.Rename(staging, finalRoot); err != nil {
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, os.ErrExist) {
			return Snapshot{}, fmt.Errorf("publish frozen skill snapshot: %w", err)
		}
		existing, err := readPublishedSnapshot(finalRoot, snapshotID, entries)
		if err != nil {
			return Snapshot{}, fmt.Errorf("existing frozen skill snapshot is invalid: %w", err)
		}
		return Snapshot{Input: existing}, nil
	} else {
		keepStaging = true
		if err := validatePublished(finalRoot, snapshotID, entries, hex.EncodeToString(manifestDigest[:])); err != nil {
			return Snapshot{}, fmt.Errorf("validate published skill snapshot: %w", err)
		}
	}
	return Snapshot{Published: true, Input: symphony.SkillSnapshotInput{
		SchemaVersion: symphony.SkillSnapshotSchemaV1, SnapshotID: snapshotID, RootPath: finalRoot,
		Manifest: fullManifest, ManifestSHA256: hex.EncodeToString(manifestDigest[:]), Entries: entries,
	}}, nil
}

func (m Manager) configuredSources() []source {
	if len(m.sources) == 0 {
		return productSources()
	}
	return append([]source{}, m.sources...)
}

func readPublishedSnapshot(root, snapshotID string, entries []symphony.SkillSnapshotEntry) (symphony.SkillSnapshotInput, error) {
	contents, err := os.ReadFile(filepath.Join(root, "pika-snapshot.json"))
	if err != nil {
		return symphony.SkillSnapshotInput{}, err
	}
	manifestContents := bytesTrimSpace(contents)
	digest := sha256.Sum256(manifestContents)
	var published manifest
	if err := json.Unmarshal(manifestContents, &published); err != nil || published.SchemaVersion != symphony.SkillSnapshotSchemaV1 || published.ValidationVersion != validationVersion || published.SnapshotID != snapshotID || !sameManifestEntries(published.Skills, entries) {
		return symphony.SkillSnapshotInput{}, errors.New("published skill snapshot manifest is invalid")
	}
	if err := validatePublished(root, snapshotID, entries, hex.EncodeToString(digest[:])); err != nil {
		return symphony.SkillSnapshotInput{}, err
	}
	return symphony.SkillSnapshotInput{SchemaVersion: symphony.SkillSnapshotSchemaV1, SnapshotID: snapshotID, RootPath: root,
		Manifest: manifestContents, ManifestSHA256: hex.EncodeToString(digest[:]), Entries: entries}, nil
}

type resolvedSource struct {
	source
	CommitSHA string
}

type manifest struct {
	SchemaVersion     int64           `json:"schema_version"`
	ValidationVersion int64           `json:"validation_version"`
	SnapshotID        string          `json:"snapshot_id,omitempty"`
	ResolvedAt        string          `json:"resolved_at"`
	Skills            []manifestEntry `json:"skills"`
}

// canonicalManifest is precisely the published manifest minus snapshot_id;
// its digest is the immutable snapshot identity described in contracts.md.
type canonicalManifest struct {
	SchemaVersion     int64           `json:"schema_version"`
	ValidationVersion int64           `json:"validation_version"`
	ResolvedAt        string          `json:"resolved_at"`
	Skills            []manifestEntry `json:"skills"`
}
type manifestEntry struct {
	Name          string `json:"name"`
	Repository    string `json:"repository"`
	Branch        string `json:"branch"`
	CommitSHA     string `json:"commit_sha"`
	Path          string `json:"path"`
	ContentSHA256 string `json:"content_sha256"`
}

func manifestEntries(entries []symphony.SkillSnapshotEntry) []manifestEntry {
	result := make([]manifestEntry, len(entries))
	for index, entry := range entries {
		result[index] = manifestEntry{Name: entry.Name, Repository: entry.Repository, Branch: entry.Branch, CommitSHA: entry.CommitSHA, Path: entry.RelativePath, ContentSHA256: entry.ContentSHA256}
	}
	return result
}

func validateSources(sources []source) error {
	if len(sources) != 2 {
		return errors.New("exactly KernelWiki and ncu-report-skill sources are required")
	}
	for index, expected := range productSources() {
		if sources[index].Name != expected.Name || sources[index].Repository == "" || sources[index].Branch != expected.Branch {
			return fmt.Errorf("invalid skill source %d", index)
		}
	}
	return nil
}

// publishedRepository separates the test-only fetch seam from immutable
// provenance. Production fetches and manifest values are both the allowlisted
// product URL; tests may fetch a committed local stand-in but must still emit
// the canonical public contract.
func publishedRepository(source source) string {
	for _, expected := range productSources() {
		if source.Name == expected.Name && source.Branch == expected.Branch {
			return expected.Repository
		}
	}
	return source.Repository
}

func resolveHead(ctx context.Context, git Git, source source) (string, error) {
	output, err := git.Run(ctx, "", "ls-remote", source.Repository, "refs/heads/"+source.Branch)
	if err != nil {
		return "", fmt.Errorf("resolve %s %s: %w", source.Name, source.Branch, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[1] != "refs/heads/"+source.Branch || !isGitSHA(fields[0]) {
		return "", fmt.Errorf("resolve %s %s: remote did not return one branch head", source.Name, source.Branch)
	}
	return fields[0], nil
}

func isGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cloneDetached(ctx context.Context, git Git, source resolvedSource, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return fmt.Errorf("create %s checkout: %w", source.Name, err)
	}
	if _, err := git.Run(ctx, destination, "init"); err != nil {
		return fmt.Errorf("initialize %s checkout: %w", source.Name, err)
	}
	if _, err := git.Run(ctx, destination, "remote", "add", "origin", source.Repository); err != nil {
		return fmt.Errorf("configure %s origin: %w", source.Name, err)
	}
	if _, err := git.Run(ctx, destination, "fetch", "--depth=1", "origin", source.CommitSHA); err != nil {
		return fmt.Errorf("fetch exact %s commit %s: %w", source.Name, source.CommitSHA, err)
	}
	origin, err := git.Run(ctx, destination, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(string(origin)) != source.Repository {
		return fmt.Errorf("clone %s: origin does not match requested repository", source.Name)
	}
	if _, err := git.Run(ctx, destination, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("checkout %s at %s: %w", source.Name, source.CommitSHA, err)
	}
	head, err := git.Run(ctx, destination, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != source.CommitSHA {
		return fmt.Errorf("checkout %s: detached HEAD does not match resolved commit", source.Name)
	}
	status, err := git.Run(ctx, destination, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("checkout %s is not clean", source.Name)
	}
	// Seal the published checkout to the manifest's canonical remote even when
	// a test transport fetched its bytes from a local stand-in.
	if _, err := git.Run(ctx, destination, "remote", "set-url", "origin", publishedRepository(source.source)); err != nil {
		return fmt.Errorf("canonicalize %s published origin: %w", source.Name, err)
	}
	return nil
}

func validateSkillRoot(root, expectedName string) error {
	if err := validateSkillTree(root); err != nil {
		return fmt.Errorf("validate %s filesystem: %w", expectedName, err)
	}
	metadata, err := parseSkillMetadata(filepath.Join(root, "SKILL.md"))
	if err != nil {
		return fmt.Errorf("validate %s metadata: %w", expectedName, err)
	}
	if metadata.name != expectedName {
		return fmt.Errorf("validate %s metadata: name is %q", expectedName, metadata.name)
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." || relative == ".git" || strings.HasPrefix(relative, ".git"+string(filepath.Separator)) {
			return nil
		}
		if isRejectedControlPath(filepath.ToSlash(relative)) {
			return fmt.Errorf("provider control manifest is not allowed: %s", filepath.ToSlash(relative))
		}
		return nil
	})
}

func validateSkillTree(root string) error {
	cleanRoot := filepath.Clean(root)
	return filepath.WalkDir(cleanRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(cleanRoot, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if entry.Name() == ".git" {
			if relative == ".git" && entry.IsDir() {
				return filepath.SkipDir
			}
			return fmt.Errorf("nested Git metadata is not allowed: %s", filepath.ToSlash(relative))
		}
		if entry.Name() == "SKILL.md" && filepath.ToSlash(relative) != "SKILL.md" {
			return fmt.Errorf("additional SKILL.md is not allowed: %s", filepath.ToSlash(relative))
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			return fmt.Errorf("absolute symlink is not allowed: %s", filepath.ToSlash(relative))
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("invalid or circular symlink %s: %w", filepath.ToSlash(relative), err)
		}
		inside, err := filepath.Rel(cleanRoot, resolved)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return fmt.Errorf("escaping symlink is not allowed: %s", filepath.ToSlash(relative))
		}
		if inside == ".git" || strings.HasPrefix(inside, ".git"+string(filepath.Separator)) {
			return fmt.Errorf("symlink into Git metadata is not allowed: %s", filepath.ToSlash(relative))
		}
		return nil
	})
}

func isRejectedControlPath(relative string) bool {
	base := filepath.Base(relative)
	// Skill content may legitimately include repository-local guidance such as
	// CLAUDE.md or AGENTS.md. They are data within the frozen skill, not a
	// provider installation surface. Only manifests that can alter provider
	// behavior outside the skill contract are disallowed here.
	if base == "plugin.json" || base == "mcp.json" || base == "hooks.json" {
		return true
	}
	return strings.HasPrefix(relative, ".codex-plugin/") || strings.HasPrefix(relative, ".cursor/") || strings.HasPrefix(relative, ".agents/")
}

type skillMetadata struct{ name, description string }

func parseSkillMetadata(path string) (skillMetadata, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return skillMetadata{}, err
	}
	lines := strings.Split(string(contents), "\n")
	if len(lines) < 4 || strings.TrimSpace(lines[0]) != "---" {
		return skillMetadata{}, errors.New("SKILL.md must begin with YAML frontmatter")
	}
	metadata := skillMetadata{}
	closed := false
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "---" {
			closed = true
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			metadata.name = value
		case "description":
			metadata.description = value
		}
	}
	if !closed || metadata.name == "" || metadata.description == "" {
		return skillMetadata{}, errors.New("SKILL.md requires name and description frontmatter")
	}
	return metadata, nil
}

func contentDigest(root string) (string, error) {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if relative != "." && (entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0) {
			paths = append(paths, filepath.ToSlash(relative))
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk skill content: %w", err)
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, relative := range paths {
		if _, err := io.WriteString(hash, relative+"\x00"); err != nil {
			return "", err
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			if _, err := io.WriteString(hash, "symlink\x00"+filepath.ToSlash(target)); err != nil {
				return "", err
			}
		} else {
			contents, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			if _, err := hash.Write(contents); err != nil {
				return "", err
			}
		}
		if _, err := io.WriteString(hash, "\x00"); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func pluginManifest() []byte {
	return []byte(`{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "pika-kda-skills",
  "description": "Frozen kernel optimization skills for one Pika Optimization",
  "version": "1.0.0",
  "author": {"name": "pika-go"}
}
`)
}

func validatePublished(root, snapshotID string, entries []symphony.SkillSnapshotEntry, manifestDigest string) error {
	contents, err := os.ReadFile(filepath.Join(root, "pika-snapshot.json"))
	if err != nil {
		return err
	}
	trimmed := bytesTrimSpace(contents)
	if err := ValidateManifest(trimmed); err != nil {
		return err
	}
	digest := sha256.Sum256(trimmed)
	if hex.EncodeToString(digest[:]) != manifestDigest {
		return errors.New("published manifest digest differs")
	}
	var value manifest
	if err := json.Unmarshal(trimmed, &value); err != nil || value.SchemaVersion != symphony.SkillSnapshotSchemaV1 || value.ValidationVersion != validationVersion || value.SnapshotID != snapshotID || !sameManifestEntries(value.Skills, entries) {
		return errors.New("published manifest is invalid")
	}
	canonical, err := json.Marshal(canonicalManifest{SchemaVersion: value.SchemaVersion, ValidationVersion: value.ValidationVersion, ResolvedAt: value.ResolvedAt, Skills: value.Skills})
	if err != nil {
		return err
	}
	id := sha256.Sum256(canonical)
	if hex.EncodeToString(id[:]) != snapshotID {
		return errors.New("published snapshot_id does not cover canonical manifest fields")
	}
	for _, entry := range entries {
		rootPath := filepath.Join(root, filepath.FromSlash(entry.RelativePath))
		if err := validateSkillRoot(rootPath, entry.Name); err != nil {
			return err
		}
		if err := validatePublishedSkillGit(rootPath, entry); err != nil {
			return err
		}
		digest, err := contentDigest(rootPath)
		if err != nil || digest != entry.ContentSHA256 {
			return errors.New("published skill content digest differs")
		}
	}
	if contents, err := os.ReadFile(filepath.Join(root, "plugin.json")); err != nil || string(contents) != string(pluginManifest()) {
		return errors.New("published plugin manifest differs")
	}
	return nil
}

func sameManifestEntries(values []manifestEntry, entries []symphony.SkillSnapshotEntry) bool {
	if len(values) != len(entries) {
		return false
	}
	for index, entry := range entries {
		if values[index] != (manifestEntry{Name: entry.Name, Repository: entry.Repository, Branch: entry.Branch, CommitSHA: entry.CommitSHA, Path: entry.RelativePath, ContentSHA256: entry.ContentSHA256}) {
			return false
		}
	}
	return true
}

func validatePublishedSkillGit(root string, entry symphony.SkillSnapshotEntry) error {
	run := func(arguments ...string) (string, error) {
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s", strings.Join(arguments, " "), strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read published skill %s commit: %w", entry.Name, err)
	}
	if head != entry.CommitSHA {
		return fmt.Errorf("published skill %s has a different commit", entry.Name)
	}
	origin, err := run("remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("read published skill %s origin: %w", entry.Name, err)
	}
	if origin != entry.Repository {
		return fmt.Errorf("published skill %s has a different origin", entry.Name)
	}
	status, err := run("status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || status != "" {
		if err != nil {
			return fmt.Errorf("inspect published skill %s cleanliness: %w", entry.Name, err)
		}
		return fmt.Errorf("published skill %s is not clean", entry.Name)
	}
	return nil
}

func bytesTrimSpace(value []byte) []byte { return []byte(strings.TrimSpace(string(value))) }

func ensureDirectory(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("create toolkit root: %w", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("protect toolkit root: %w", err)
	}
	return nil
}

func writeFile(path string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

func makeReadOnly(root string) error {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(paths, func(left, right int) bool { return len(paths[left]) > len(paths[right]) })
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// The tracked skill content and published manifests are immutable. Keep
		// Git's administrative directory writable: it is excluded from the
		// content digest, and this lets normal workspace cleanup remove a
		// published snapshot without mutating any agent-visible skill file.
		if relative == ".git" || strings.HasPrefix(relative, ".git"+string(filepath.Separator)) ||
			strings.HasSuffix(relative, string(filepath.Separator)+".git") ||
			strings.Contains(relative, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		// Read-only publication must not change a tracked file's executable bit:
		// Git treats that metadata change as a dirty checkout. Preserve execute
		// permission while removing every write bit so both immutability and the
		// recorded clean detached checkout remain true on recovery.
		mode := os.FileMode(0o444)
		if info.IsDir() {
			mode = 0o555
		} else if info.Mode().Perm()&0o111 != 0 {
			mode = 0o555
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("protect published skill path %s: %w", path, err)
		}
	}
	return nil
}

func makeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() | 0o200
		if info.IsDir() {
			mode |= 0o700
		}
		return os.Chmod(path, mode)
	})
}
