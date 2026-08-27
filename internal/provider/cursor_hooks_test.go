package provider_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestInstallCursorHooksPreservesExistingHooksAndIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".cursor", "hooks.json")
	original := []byte("{\n  \"version\": 1,\n  \"hooks\": {\"sessionStart\": [{\"command\": \"herdr-hook\"}]}\n}\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	rollback, err := provider.InstallCursorHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := os.ReadFile(path)
	var root struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(contents, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Hooks["sessionStart"]) != 2 || len(root.Hooks["beforeMCPExecution"]) != 1 || !strings.Contains(string(root.Hooks["sessionStart"][1]), "cursor sessionStart") {
		t.Fatalf("installed hooks = %s", contents)
	}
	if _, err := provider.InstallCursorHooks(path); err != nil {
		t.Fatal(err)
	}
	idempotent, _ := os.ReadFile(path)
	if string(idempotent) != string(contents) {
		t.Fatal("idempotent install changed hooks")
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != string(original) {
		t.Fatalf("rollback changed original bytes: %q", restored)
	}
}
