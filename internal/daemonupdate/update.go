package daemonupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
)

const HandoffProtocol = 1

type Probe struct {
	Version         string `json:"version"`
	HandoffProtocol int    `json:"handoff_protocol"`
	ControlProtocol int    `json:"control_protocol"`
	WorkspaceFormat int    `json:"workspace_format"`
	SQLiteSchema    int    `json:"sqlite_schema"`
	OperatingSystem string `json:"operating_system"`
	Architecture    string `json:"architecture"`
}

type Candidate struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Probe  Probe  `json:"probe"`
}

type Generation struct {
	Path        string `json:"path"`
	Digest      string `json:"digest"`
	Version     string `json:"version"`
	ActivatedAt string `json:"activated_at"`
}

type State string

const (
	StatePreparing   State = "preparing"
	StateQuiescing   State = "quiescing"
	StateActivating  State = "activating"
	StateStabilizing State = "stabilizing"
	StateCommitted   State = "committed"
	StateRollingBack State = "rolling_back"
	StateRolledBack  State = "rolled_back"
	StateFailed      State = "failed"
)

type Status struct {
	ID        string     `json:"id"`
	State     State      `json:"state"`
	From      Generation `json:"from"`
	To        Generation `json:"to"`
	StartedAt string     `json:"started_at"`
	UpdatedAt string     `json:"updated_at"`
	Failure   string     `json:"failure,omitempty"`
}

func CurrentProbe(version string) Probe {
	return Probe{
		Version: version, HandoffProtocol: HandoffProtocol, ControlProtocol: protocol.Version,
		WorkspaceFormat: optimizationworkspace.FormatVersion, SQLiteSchema: symphony.CurrentSchemaVersion(),
		OperatingSystem: runtime.GOOS, Architecture: runtime.GOARCH,
	}
}

func (p Probe) ValidateCompatible(current Probe) error {
	if strings.TrimSpace(p.Version) == "" {
		return errors.New("candidate version is empty")
	}
	checks := []struct {
		label string
		got   any
		want  any
	}{
		{"handoff protocol", p.HandoffProtocol, current.HandoffProtocol},
		{"control protocol", p.ControlProtocol, current.ControlProtocol},
		{"Workspace format", p.WorkspaceFormat, current.WorkspaceFormat},
		{"SQLite schema", p.SQLiteSchema, current.SQLiteSchema},
		{"operating system", p.OperatingSystem, current.OperatingSystem},
		{"architecture", p.Architecture, current.Architecture},
	}
	for _, check := range checks {
		if fmt.Sprint(check.got) != fmt.Sprint(check.want) {
			return fmt.Errorf("candidate %s is %v, current daemon requires %v; use the cold backup/shutdown/resume upgrade path", check.label, check.got, check.want)
		}
	}
	return nil
}

func Stage(ctx context.Context, workspaceRoot, source string) (Candidate, error) {
	destination, digest, err := stageFile(workspaceRoot, source)
	if err != nil {
		return Candidate{}, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, destination, "update-probe").Output()
	if err != nil {
		return Candidate{}, fmt.Errorf("probe candidate binary: %w", err)
	}
	var probe Probe
	if err := json.Unmarshal(output, &probe); err != nil {
		return Candidate{}, fmt.Errorf("decode candidate compatibility probe: %w", err)
	}
	if err := probe.ValidateCompatible(CurrentProbe(probe.Version)); err != nil {
		return Candidate{}, err
	}
	return Candidate{Path: destination, Digest: digest, Probe: probe}, nil
}

func stageFile(workspaceRoot, source string) (string, string, error) {
	if workspaceRoot == "" || !filepath.IsAbs(workspaceRoot) {
		return "", "", errors.New("absolute Workspace root is required")
	}
	absoluteSource, err := filepath.Abs(source)
	if err != nil {
		return "", "", fmt.Errorf("resolve candidate binary: %w", err)
	}
	info, err := os.Stat(absoluteSource)
	if err != nil {
		return "", "", fmt.Errorf("inspect candidate binary: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", "", errors.New("candidate binary must be a regular executable file")
	}

	root := generationsRoot(workspaceRoot)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", fmt.Errorf("create daemon generations directory: %w", err)
	}
	sourceFile, err := os.Open(absoluteSource)
	if err != nil {
		return "", "", fmt.Errorf("open candidate binary: %w", err)
	}
	defer sourceFile.Close()
	temporary, err := os.CreateTemp(root, ".candidate-*.tmp")
	if err != nil {
		return "", "", fmt.Errorf("create staged candidate: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temporary, hash), sourceFile); err != nil {
		_ = temporary.Close()
		return "", "", fmt.Errorf("copy candidate binary: %w", err)
	}
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return "", "", fmt.Errorf("protect staged candidate: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", "", fmt.Errorf("sync staged candidate: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", "", fmt.Errorf("close staged candidate: %w", err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	directory := filepath.Join(root, digest)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", fmt.Errorf("create candidate generation: %w", err)
	}
	destination := filepath.Join(directory, "pika-go")
	if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(temporaryPath, destination); err != nil {
			return "", "", fmt.Errorf("commit candidate generation: %w", err)
		}
		if err := syncDirectory(directory); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", fmt.Errorf("inspect candidate generation: %w", err)
	}
	if err := verifyDigest(destination, digest); err != nil {
		return "", "", err
	}
	return destination, digest, nil
}

func ValidateCandidate(workspaceRoot string, candidate Candidate, current Probe) error {
	if candidate.Path == "" || !filepath.IsAbs(candidate.Path) || candidate.Digest == "" {
		return errors.New("complete staged candidate identity is required")
	}
	wantRoot := generationsRoot(workspaceRoot)
	relative, err := filepath.Rel(wantRoot, candidate.Path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return errors.New("candidate binary is outside the Workspace generation store")
	}
	if filepath.Clean(candidate.Path) != filepath.Join(wantRoot, candidate.Digest, "pika-go") {
		return errors.New("candidate path does not match its digest identity")
	}
	if err := verifyDigest(candidate.Path, candidate.Digest); err != nil {
		return err
	}
	return candidate.Probe.ValidateCompatible(current)
}

func ReadCurrent(workspaceRoot string) (Generation, error) {
	var generation Generation
	if err := readJSON(currentPath(workspaceRoot), &generation); err != nil {
		return Generation{}, err
	}
	return generation, nil
}

func CommitCurrent(workspaceRoot string, candidate Candidate, now time.Time) (Generation, error) {
	generation := Generation{Path: candidate.Path, Digest: candidate.Digest, Version: candidate.Probe.Version, ActivatedAt: now.UTC().Format(time.RFC3339Nano)}
	if err := writeJSONAtomic(currentPath(workspaceRoot), generation); err != nil {
		return Generation{}, err
	}
	return generation, nil
}

func RestoreCurrent(workspaceRoot string, generation Generation) error {
	return writeJSONAtomic(currentPath(workspaceRoot), generation)
}

func EnsureCurrent(ctx context.Context, workspaceRoot, executable, version string) (Generation, error) {
	current, readErr := ReadCurrent(workspaceRoot)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return Generation{}, readErr
	}
	digest, err := FileDigest(executable)
	if err != nil {
		return Generation{}, err
	}
	if readErr == nil && current.Digest == digest {
		return current, nil
	}
	destination, digest, err := stageFile(workspaceRoot, executable)
	if err != nil {
		return Generation{}, err
	}
	candidate := Candidate{Path: destination, Digest: digest, Probe: CurrentProbe(version)}
	return CommitCurrent(workspaceRoot, candidate, time.Now())
}

func WriteStatus(workspaceRoot string, status Status) error {
	status.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return writeJSONAtomic(statusPath(workspaceRoot), status)
}

func ReadStatus(workspaceRoot string) (Status, error) {
	var status Status
	if err := readJSON(statusPath(workspaceRoot), &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

func NewStatus(id string, from Generation, candidate Candidate) Status {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return Status{ID: id, State: StatePreparing, From: from, To: Generation{Path: candidate.Path, Digest: candidate.Digest, Version: candidate.Probe.Version}, StartedAt: now, UpdatedAt: now}
}

func StatusPath(workspaceRoot string) string { return statusPath(workspaceRoot) }

func generationsRoot(workspaceRoot string) string {
	return filepath.Join(filepath.Clean(workspaceRoot), "runtime", "daemon", "generations")
}

func currentPath(workspaceRoot string) string {
	return filepath.Join(filepath.Clean(workspaceRoot), "runtime", "daemon", "current.json")
}

func statusPath(workspaceRoot string) string {
	return filepath.Join(filepath.Clean(workspaceRoot), "runtime", "daemon", "update.json")
}

func verifyDigest(path, want string) error {
	got, err := FileDigest(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("candidate digest mismatch: got %s, want %s", got, want)
	}
	return nil
}

func FileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open staged candidate: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash staged candidate: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readJSON(path string, output any) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(contents, output); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create daemon update directory: %w", err)
	}
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode daemon update state: %w", err)
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".update-*.tmp")
	if err != nil {
		return fmt.Errorf("create daemon update state: %w", err)
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit daemon update state: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open daemon update directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync daemon update directory: %w", err)
	}
	return nil
}
