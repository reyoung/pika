package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

var cursorPikaMCPDefinition = json.RawMessage(`{
  "type": "stdio",
  "command": "${env:PIKA_GO_EXECUTABLE}",
  "args": ["mcp-proxy"],
  "env": {
    "PIKA_GO_SOCKET": "${env:PIKA_GO_SOCKET}",
    "PIKA_MCP_GRANT": "${env:PIKA_MCP_GRANT}",
    "PIKA_SESSION_ID": "${env:PIKA_SESSION_ID}"
  }
}`)

// InstallCursorMCP installs one process-parameterized user MCP definition.
// Dynamic Session identity and authorization stay in each Cursor process
// environment, so concurrent Pika Sessions never rewrite this shared file.
func InstallCursorMCP(path string) (func() error, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("Cursor MCP configuration path must be absolute")
	}
	original, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read Cursor MCP configuration: %w", err)
	}
	root := map[string]json.RawMessage{}
	if existed && len(bytes.TrimSpace(original)) != 0 {
		if err := json.Unmarshal(original, &root); err != nil {
			return nil, fmt.Errorf("parse Cursor MCP configuration: %w", err)
		}
	}
	servers := map[string]json.RawMessage{}
	if raw := root["mcpServers"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, errors.New("parse Cursor MCP servers: mcpServers must be an object")
		}
	}
	if current := servers["pika_go"]; len(current) != 0 {
		if !jsonEquivalent(current, cursorPikaMCPDefinition) {
			return nil, errors.New("Cursor MCP server name pika_go is already configured differently")
		}
		return func() error { return nil }, nil
	}
	servers["pika_go"] = cursorPikaMCPDefinition
	encodedServers, err := json.Marshal(servers)
	if err != nil {
		return nil, fmt.Errorf("encode Cursor MCP servers: %w", err)
	}
	root["mcpServers"] = encodedServers
	contents, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Cursor MCP configuration: %w", err)
	}
	contents = append(contents, '\n')
	if err := writeProviderFile(path, contents, 0o600); err != nil {
		return nil, err
	}
	return func() error {
		if existed {
			return writeProviderFile(path, original, 0o600)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove Cursor MCP configuration during rollback: %w", err)
		}
		return nil
	}, nil
}

func jsonEquivalent(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}
