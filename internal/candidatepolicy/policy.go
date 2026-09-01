// Package candidatepolicy defines the canonical validation assets that are
// protected while a Candidate is evaluated.
package candidatepolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	pathpkg "path"
	"strings"
)

const SchemaVersion = 1

var validKinds = map[string]struct{}{
	"unit_test":          {},
	"benchmark_harness":  {},
	"correctness_oracle": {},
	"fixture":            {},
	"case_manifest":      {},
	"metric_contract":    {},
}

type ProtectedPath struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type Policy struct {
	SchemaVersion            int             `json:"schema_version"`
	ProtectedValidationPaths []ProtectedPath `json:"protected_validation_paths"`
}

// Schema returns the discoverable JSON Schema for the canonical policy.
func Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"schema_version": map[string]any{"type": "integer", "const": SchemaVersion},
			"protected_validation_paths": map[string]any{
				"type": "array", "minItems": 1, "items": map[string]any{
					"type": "object", "properties": map[string]any{
						"path": map[string]any{"type": "string", "minLength": 1},
						"kind": map[string]any{"type": "string", "enum": []string{"unit_test", "benchmark_harness", "correctness_oracle", "fixture", "case_manifest", "metric_contract"}},
					}, "required": []string{"path", "kind"}, "additionalProperties": false,
				},
			},
		},
		"required":             []string{"schema_version", "protected_validation_paths"},
		"additionalProperties": false,
	}
}

// Parse returns the canonical policy when present. An absent policy is the
// legacy representation and returns (nil, false, nil).
func Parse(definition json.RawMessage) (*Policy, bool, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(definition, &object); err != nil || object == nil {
		return nil, false, errors.New("definition must be a JSON object")
	}
	raw, present := object["candidate_change_policy"]
	if !present {
		return nil, false, nil
	}
	policy, err := parsePolicy(raw)
	if err != nil {
		return nil, true, err
	}
	return &policy, true, nil
}

// ValidateNewDefinition validates the canonical policy required for a new
// Baseline submission. Historical implementation allowlists are rejected.
func ValidateNewDefinition(definition json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(definition, &object); err != nil || object == nil {
		return errors.New("definition must be a JSON object")
	}
	if containsHistoricalAllowlist(object) {
		return errors.New("allowed_candidate_surface is not accepted; use candidate_change_policy")
	}
	if _, present, err := Parse(definition); err != nil {
		return fmt.Errorf("invalid candidate_change_policy: %w", err)
	} else if !present {
		return errors.New("candidate_change_policy is required for new Baseline submissions")
	}
	return nil
}

func containsHistoricalAllowlist(value any) bool {
	switch typed := value.(type) {
	case map[string]json.RawMessage:
		for key, raw := range typed {
			if key == "allowed_candidate_surface" {
				return true
			}
			var nested any
			if json.Unmarshal(raw, &nested) == nil && containsHistoricalAllowlist(nested) {
				return true
			}
		}
	case map[string]any:
		for key, nested := range typed {
			if key == "allowed_candidate_surface" || containsHistoricalAllowlist(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsHistoricalAllowlist(nested) {
				return true
			}
		}
	}
	return false
}

func parsePolicy(raw json.RawMessage) (Policy, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Policy{}, errors.New("must be an object")
	}
	for field := range fields {
		if field != "schema_version" && field != "protected_validation_paths" {
			return Policy{}, fmt.Errorf("unsupported field %q", field)
		}
	}
	var policy Policy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return Policy{}, fmt.Errorf("invalid shape: %w", err)
	}
	if policy.SchemaVersion != SchemaVersion {
		return Policy{}, fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	pathsRaw, present := fields["protected_validation_paths"]
	if !present || string(pathsRaw) == "null" {
		return Policy{}, errors.New("protected_validation_paths is required")
	}
	if err := validatePaths(policy.ProtectedValidationPaths, pathsRaw); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func validatePaths(paths []ProtectedPath, raw json.RawMessage) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return errors.New("protected_validation_paths must be an array")
	}
	if len(entries) != len(paths) {
		return errors.New("protected_validation_paths must contain objects")
	}
	if len(entries) == 0 {
		return errors.New("protected_validation_paths must contain at least one validation asset")
	}
	seen := make(map[string]struct{}, len(paths))
	for index, itemRaw := range entries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(itemRaw, &fields); err != nil || fields == nil {
			return fmt.Errorf("protected_validation_paths[%d] must be an object", index)
		}
		for field := range fields {
			if field != "path" && field != "kind" {
				return fmt.Errorf("protected_validation_paths[%d] has unsupported field %q", index, field)
			}
		}
		item := paths[index]
		if item.Path == "" || item.Kind == "" {
			return fmt.Errorf("protected_validation_paths[%d] requires path and kind", index)
		}
		if err := validateRepoPath(item.Path); err != nil {
			return fmt.Errorf("protected_validation_paths[%d].path: %w", index, err)
		}
		if _, ok := validKinds[item.Kind]; !ok {
			return fmt.Errorf("protected_validation_paths[%d].kind %q is unsupported", index, item.Kind)
		}
		if _, ok := seen[item.Path]; ok {
			return fmt.Errorf("protected_validation_paths contains duplicate %q", item.Path)
		}
		seen[item.Path] = struct{}{}
	}
	return nil
}

func validateRepoPath(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") || strings.ContainsAny(value, "*?[]{}") {
		return errors.New("must be an exact repository-relative file path")
	}
	if pathpkg.IsAbs(value) || value != pathpkg.Clean(value) || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return errors.New("must be an exact repository-relative file path")
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".git" {
			return errors.New("must not target .git")
		}
	}
	return nil
}

// ValidateRepositoryPaths proves that every canonical path is a regular,
// non-symlink file in the supplied immutable Git tree. It intentionally does
// not inspect the mutable index or worktree.
func (p Policy) ValidateRepositoryPaths(ctx context.Context, repository, snapshotSHA string) error {
	for _, item := range p.ProtectedValidationPaths {
		if err := validateRepoPath(item.Path); err != nil {
			return fmt.Errorf("%s: %w", item.Path, err)
		}
		output, err := runGit(ctx, repository, "ls-tree", "-z", snapshotSHA, "--", ":(top,literal)"+item.Path)
		if err != nil || !snapshotRegularPath(output, item.Path) {
			return fmt.Errorf("protected validation path %q must be a regular file in snapshot %s", item.Path, snapshotSHA)
		}
	}
	return nil
}

func runGit(ctx context.Context, repository string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...).Output()
}

func snapshotRegularPath(output []byte, wanted string) bool {
	for _, record := range strings.Split(string(output), "\x00") {
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\t", 2)
		if len(fields) != 2 || fields[1] != wanted {
			continue
		}
		metadata := strings.Fields(fields[0])
		if len(metadata) == 3 && metadata[1] == "blob" && strings.HasPrefix(metadata[0], "100") {
			return true
		}
	}
	return false
}

func (p Policy) Protects(path string) bool {
	for _, item := range p.ProtectedValidationPaths {
		if item.Path == path || strings.HasPrefix(path, item.Path+"/") || strings.HasPrefix(item.Path, path+"/") {
			return true
		}
	}
	return false
}
