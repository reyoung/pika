package maintenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type FileStore struct {
	path string
}

func NewFileStore(workspaceRoot string) *FileStore {
	return &FileStore{path: filepath.Join(filepath.Clean(workspaceRoot), "runtime", "daemon", "maintenance.json")}
}

func (s *FileStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *FileStore) Read() (Status, error) {
	if s == nil || s.path == "" {
		return Status{}, errors.New("maintenance state path is required")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return Status{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var status Status
	if err := decoder.Decode(&status); err != nil {
		return Status{}, fmt.Errorf("decode maintenance state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Status{}, errors.New("maintenance state contains a second JSON value")
		}
		return Status{}, fmt.Errorf("decode maintenance state trailing data: %w", err)
	}
	if err := validateStatus(status); err != nil {
		return Status{}, err
	}
	return status, nil
}

func (s *FileStore) Write(status Status) error {
	if s == nil || s.path == "" {
		return errors.New("maintenance state path is required")
	}
	if err := validateStatus(status); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("encode maintenance state: %w", err)
	}
	contents = append(contents, '\n')
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create maintenance state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".maintenance-*.tmp")
	if err != nil {
		return fmt.Errorf("create maintenance state temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect maintenance state: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write maintenance state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync maintenance state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close maintenance state: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("commit maintenance state: %w", err)
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open maintenance state directory: %w", err)
	}
	defer directoryFile.Close()
	if err := directoryFile.Sync(); err != nil {
		return fmt.Errorf("sync maintenance state directory: %w", err)
	}
	return nil
}

func ValidateGeneration(generation Generation) error {
	if err := ValidateDigest(generation.Digest); err != nil {
		return err
	}
	if strings.ContainsAny(generation.Version, "\n\r") {
		return errors.New("generation version is invalid")
	}
	return nil
}

func ValidateDigest(digest string) error {
	if len(digest) != 64 {
		return errors.New("generation digest must be a 64-character SHA-256 hex")
	}
	for _, character := range digest {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("generation digest must be lowercase hexadecimal")
		}
	}
	return nil
}

func validateStatus(status Status) error {
	if strings.TrimSpace(status.ID) == "" || strings.TrimSpace(status.RequestID) == "" {
		return errors.New("maintenance id and request_id are required")
	}
	if err := ValidateGeneration(status.FromGeneration); err != nil {
		return fmt.Errorf("from_generation: %w", err)
	}
	if err := ValidateGeneration(status.ToGeneration); err != nil {
		return fmt.Errorf("to_generation: %w", err)
	}
	for _, target := range status.Targets {
		if target.WorkID == "" || target.SessionID == "" || target.WorkGeneration < 1 {
			return errors.New("maintenance target identity is incomplete")
		}
	}
	if status.StartedAt == "" || status.UpdatedAt == "" {
		return errors.New("maintenance started_at and updated_at are required")
	}
	lowerFailure := strings.ToLower(status.Failure)
	for _, forbidden := range []string{"credential", "password", "token=", "prompt=", "repository="} {
		if strings.Contains(lowerFailure, forbidden) {
			return errors.New("maintenance failure contains forbidden sensitive detail")
		}
	}
	switch status.State {
	case StateQuiescing:
		if status.ReadyAt != "" || status.HoldingAt != "" || status.ResumedAt != "" || status.Failure != "" || status.HoldingGeneration.Digest != "" || status.ResumeRequestID != "" {
			return errors.New("quiescing state contains stale later-state fields")
		}
	case StateReady:
		if status.ReadyAt == "" || status.HoldingAt != "" || status.ResumedAt != "" || status.Failure != "" || status.HoldingGeneration.Digest != "" || status.ResumeRequestID != "" {
			return errors.New("ready state requires ready_at and no stale later-state fields")
		}
	case StateHolding:
		if err := ValidateGeneration(status.HoldingGeneration); err != nil {
			return fmt.Errorf("holding_generation: %w", err)
		}
		if status.ReadyAt == "" || status.HoldingAt == "" || status.ResumedAt != "" || status.Failure != "" {
			return errors.New("holding state requires holding_at")
		}
	case StateResumed:
		if err := ValidateGeneration(status.HoldingGeneration); err != nil {
			return fmt.Errorf("holding_generation: %w", err)
		}
		if status.ReadyAt == "" || status.HoldingAt == "" || status.ResumeRequestID == "" || status.ResumedAt == "" || status.Failure != "" {
			return errors.New("resumed state requires resume request identity")
		}
	case StateFailed:
		if strings.TrimSpace(status.Failure) == "" || status.HoldingGeneration.Digest != "" || status.HoldingAt != "" || status.ResumedAt != "" || status.ResumeRequestID != "" {
			return errors.New("failed state requires a failure reason and no later-state fields")
		}
	default:
		return fmt.Errorf("unsupported maintenance state %q", status.State)
	}
	return nil
}
