package provider_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestInstallCursorMCPPreservesOtherServersAndRollsBackExactly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".cursor", "mcp.json")
	original := []byte("{\n  \"other\": true,\n  \"mcpServers\": {\"existing\": {\"url\": \"https://example.test/mcp\"}}\n}\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	rollback, err := provider.InstallCursorMCP(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Other      bool                       `json:"other"`
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(contents, &root); err != nil {
		t.Fatal(err)
	}
	if !root.Other || len(root.MCPServers["existing"]) == 0 || len(root.MCPServers["pika_go"]) == 0 {
		t.Fatalf("installed configuration = %s", contents)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != string(original) {
		t.Fatalf("rollback changed original bytes: %q", restored)
	}
}

func TestInstallCursorMCPRejectsConflictingPikaServer(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"pika_go":{"command":"foreign"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.InstallCursorMCP(path); err == nil {
		t.Fatal("conflicting pika_go server was accepted")
	}
}

func TestInstallCursorMCPRemovesNewFileOnRollback(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".cursor", "mcp.json")
	rollback, err := provider.InstallCursorMCP(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("new MCP configuration remains: %v", err)
	}
}
