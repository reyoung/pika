package candidatepolicy_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/candidatepolicy"
)

func TestValidateNewDefinitionAcceptsCanonicalPolicy(t *testing.T) {
	schema := candidatepolicy.Schema()
	properties := schema["properties"].(map[string]any)
	paths := properties["protected_validation_paths"].(map[string]any)
	if got := paths["minItems"]; got != float64(1) && got != 1 {
		t.Fatalf("protected_validation_paths minItems = %v, want 1", got)
	}

	definition := json.RawMessage(`{"candidate_change_policy":{"schema_version":1,"protected_validation_paths":[{"path":"tests/case.json","kind":"unit_test"},{"path":"bench/run.sh","kind":"benchmark_harness"},{"path":"oracle/output.json","kind":"correctness_oracle"},{"path":"fixtures/input.bin","kind":"fixture"},{"path":"cases.json","kind":"case_manifest"},{"path":"metrics.json","kind":"metric_contract"}]}}`)
	if err := candidatepolicy.ValidateNewDefinition(definition); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	policy, present, err := candidatepolicy.Parse(definition)
	if err != nil || !present || len(policy.ProtectedValidationPaths) != 6 {
		t.Fatalf("Parse() = policy=%+v present=%v err=%v", policy, present, err)
	}
}

func TestValidateNewDefinitionRejectsInvalidCanonicalPolicy(t *testing.T) {
	valid := `{"candidate_change_policy":{"schema_version":1,"protected_validation_paths":[{"path":"tests/case.json","kind":"unit_test"}]}}`
	tests := map[string]string{
		"missing policy":             `{}`,
		"wrong schema version":       strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1),
		"missing paths":              `{"candidate_change_policy":{"schema_version":1}}`,
		"wrong paths type":           `{"candidate_change_policy":{"schema_version":1,"protected_validation_paths":"tests/case.json"}}`,
		"wrong path type":            strings.Replace(valid, `"tests/case.json"`, `42`, 1),
		"wrong kind type":            strings.Replace(valid, `"unit_test"`, `42`, 1),
		"invalid kind":               strings.Replace(valid, `"unit_test"`, `"implementation"`, 1),
		"duplicate":                  strings.Replace(valid, `{"path":"tests/case.json","kind":"unit_test"}`, `{"path":"tests/case.json","kind":"unit_test"},{"path":"tests/case.json","kind":"fixture"}`, 1),
		"glob":                       strings.Replace(valid, `tests/case.json`, `tests/*.json`, 1),
		"absolute":                   strings.Replace(valid, `tests/case.json`, `/tests/case.json`, 1),
		"traversal":                  strings.Replace(valid, `tests/case.json`, `../tests/case.json`, 1),
		"git":                        strings.Replace(valid, `tests/case.json`, `.git/config`, 1),
		"unknown policy field":       `{"candidate_change_policy":{"schema_version":1,"protected_validation_paths":[],"implementation_paths":[]}}`,
		"unknown path field":         `{"candidate_change_policy":{"schema_version":1,"protected_validation_paths":[{"path":"tests/case.json","kind":"unit_test","extra":true}]}}`,
		"empty paths":                `{"candidate_change_policy":{"schema_version":1,"protected_validation_paths":[]}}`,
		"historical allowlist":       `{"optimization_contract":{"allowed_candidate_surface":["kernel.cu"]},"candidate_change_policy":{"schema_version":1,"protected_validation_paths":[{"path":"tests/case.json","kind":"unit_test"}]}}`,
		"top-level allowlist bypass": `{"allowed_candidate_surface":["kernel.cu"],"candidate_change_policy":{"schema_version":1,"protected_validation_paths":[{"path":"tests/case.json","kind":"unit_test"}]}}`,
	}
	for name, definition := range tests {
		t.Run(name, func(t *testing.T) {
			if err := candidatepolicy.ValidateNewDefinition(json.RawMessage(definition)); err == nil {
				t.Fatal("invalid policy was accepted")
			}
		})
	}
}

func TestParseMissingPolicyIsLegacyCompatible(t *testing.T) {
	policy, present, err := candidatepolicy.Parse(json.RawMessage(`{"target":"kernel","optimization_contract":{"allowed_candidate_surface":["kernel.cu"]}}`))
	if err != nil || present || policy != nil {
		t.Fatalf("legacy Parse() = policy=%+v present=%v err=%v", policy, present, err)
	}
}

func TestValidateRepositoryPathsRequiresTrackedRegularFiles(t *testing.T) {
	repository := t.TempDir()
	git(t, repository, "init", "--quiet", "--initial-branch=main")
	git(t, repository, "config", "user.name", "Pika Test")
	git(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "tracked.json"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "directory", "child"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "untracked"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.json", filepath.Join(repository, "symlink")); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "add", "tracked.json", "directory/child", "symlink")
	git(t, repository, "commit", "-m", "baseline")
	snapshotSHA := strings.TrimSpace(git(t, repository, "rev-parse", "HEAD"))
	for name, item := range map[string]string{
		"legal":     `{"path":"tracked.json","kind":"unit_test"}`,
		"directory": `{"path":"directory","kind":"fixture"}`,
		"untracked": `{"path":"untracked","kind":"fixture"}`,
		"symlink":   `{"path":"symlink","kind":"fixture"}`,
	} {
		t.Run(name, func(t *testing.T) {
			policy := candidatepolicy.Policy{SchemaVersion: 1}
			if err := json.Unmarshal([]byte(`{"protected_validation_paths":[`+item+`]}`), &policy); err != nil {
				t.Fatal(err)
			}
			err := policy.ValidateRepositoryPaths(context.Background(), repository, snapshotSHA)
			if name == "legal" && err != nil {
				t.Fatalf("legal path rejected: %v", err)
			}
			if name != "legal" && err == nil {
				t.Fatal("invalid repository path accepted")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked.json"), []byte("changed after snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "add", "tracked.json")
	policy := candidatepolicy.Policy{SchemaVersion: 1, ProtectedValidationPaths: []candidatepolicy.ProtectedPath{{Path: "tracked.json", Kind: "unit_test"}}}
	if err := policy.ValidateRepositoryPaths(context.Background(), repository, snapshotSHA); err != nil {
		t.Fatalf("snapshot-bound regular file rejected after worktree/index mutation: %v", err)
	}
}

func git(t *testing.T, repository string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"git", "-C", repository}, args...)
	output, err := exec.Command(commandArgs[0], commandArgs[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
