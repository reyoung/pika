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

func ReadStable(root, relativePath string, maxBytes int64) ([]byte, symphony.ArtifactInput, error) {
	if root == "" || !filepath.IsAbs(root) || relativePath == "" || filepath.IsAbs(relativePath) {
		return nil, symphony.ArtifactInput{}, errors.New("absolute root and relative evidence path are required")
	}
	clean := filepath.Clean(filepath.FromSlash(relativePath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, symphony.ArtifactInput{}, errors.New("evidence path escapes the Work root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, symphony.ArtifactInput{}, fmt.Errorf("resolve Work root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, clean))
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
		RelativePath: filepath.ToSlash(clean), ByteSize: int64(len(contents)), ContentSHA256: hex.EncodeToString(digest[:]), ContractVersion: 1,
	}, nil
}
