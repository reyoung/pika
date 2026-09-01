package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/reyoung/pika-go/internal/symphony"
)

const DefaultMaxBytes int64 = 16 << 20

type WorkScope string

const (
	DiagnosisScope WorkScope = "diagnoses"
	IterationScope WorkScope = "iterations"
)

// EnsureWorkRoot is the canonical owner of Pika-managed evidence paths. Both
// Context publication and MCP stable reads call this function so an Agent
// cannot be shown one directory while the control plane verifies another.
func EnsureWorkRoot(base string, scope WorkScope, workID string) (string, error) {
	if base == "" || !filepath.IsAbs(base) {
		return "", errors.New("absolute Pika evidence root is required")
	}
	if scope != DiagnosisScope && scope != IterationScope {
		return "", errors.New("recognized evidence scope is required")
	}
	if workID == "" || workID == "." || workID == ".." || filepath.IsAbs(workID) || filepath.Base(workID) != workID || strings.ContainsAny(workID, `/\\`) {
		return "", errors.New("plain Work ID is required for evidence root")
	}
	for _, character := range workID {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return "", errors.New("safe Work ID is required for evidence root")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create Pika evidence root: %w", err)
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve Pika evidence root: %w", err)
	}
	root := filepath.Join(base, string(scope), workID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create Work evidence root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve Work evidence root: %w", err)
	}
	relative, err := filepath.Rel(resolvedBase, resolvedRoot)
	if err != nil || relative != filepath.Join(string(scope), workID) {
		return "", errors.New("Work evidence root escapes the Pika evidence directory")
	}
	if err := os.Chmod(resolvedRoot, 0o700); err != nil {
		return "", fmt.Errorf("protect Work evidence root: %w", err)
	}
	return resolvedRoot, nil
}

func ReadStable(root, evidencePath string, maxBytes int64) ([]byte, symphony.ArtifactInput, error) {
	if root == "" || !filepath.IsAbs(root) || evidencePath == "" {
		return nil, symphony.ArtifactInput{}, errors.New("absolute root and non-empty evidence path are required")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, symphony.ArtifactInput{}, fmt.Errorf("resolve Work root: %w", err)
	}
	candidate := filepath.Clean(filepath.FromSlash(evidencePath))
	if !filepath.IsAbs(candidate) {
		if candidate == "." || candidate == ".." || strings.HasPrefix(candidate, ".."+string(filepath.Separator)) {
			return nil, symphony.ArtifactInput{}, errors.New("evidence path escapes the Work root")
		}
		candidate = filepath.Join(resolvedRoot, candidate)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return nil, symphony.ArtifactInput{}, fmt.Errorf("resolve evidence path: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, symphony.ArtifactInput{}, errors.New("evidence symlink escapes the Work root")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, symphony.ArtifactInput{}, fmt.Errorf("open evidence: %w", err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nil, symphony.ArtifactInput{}, errors.New("evidence must be a regular file")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if before.Size() > maxBytes {
		return nil, symphony.ArtifactInput{}, fmt.Errorf("evidence exceeds %d bytes", maxBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, symphony.ArtifactInput{}, fmt.Errorf("read evidence: %w", err)
	}
	if int64(len(contents)) > maxBytes {
		return nil, symphony.ArtifactInput{}, fmt.Errorf("evidence exceeds %d bytes", maxBytes)
	}
	after, err := file.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, symphony.ArtifactInput{}, errors.New("evidence changed while being read")
	}
	digest := sha256.Sum256(contents)
	return contents, symphony.ArtifactInput{
		RelativePath: filepath.ToSlash(rel), ByteSize: int64(len(contents)), ContentSHA256: hex.EncodeToString(digest[:]), ContractVersion: 1,
	}, nil
}
