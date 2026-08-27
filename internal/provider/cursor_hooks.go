package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var cursorHookEvents = []string{
	"sessionStart", "sessionEnd", "beforeSubmitPrompt", "afterAgentResponse",
	"postToolUse", "postToolUseFailure", "afterShellExecution", "afterMCPExecution", "stop",
	"beforeMCPExecution",
}

// InstallCursorHooks adds static, environment-parameterized Pika commands to
// the user's Cursor hook file while preserving all existing hook definitions.
func InstallCursorHooks(path string) (func() error, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("Cursor hooks configuration path must be absolute")
	}
	original, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read Cursor hooks configuration: %w", err)
	}
	root := map[string]json.RawMessage{}
	if existed && len(original) != 0 {
		if err := json.Unmarshal(original, &root); err != nil {
			return nil, fmt.Errorf("parse Cursor hooks configuration: %w", err)
		}
	}
	hooks := map[string][]json.RawMessage{}
	if raw := root["hooks"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, errors.New("parse Cursor hooks configuration: hooks must be an object of arrays")
		}
	}
	changed := false
	for _, event := range cursorHookEvents {
		definition := map[string]any{
			"command": `"$PIKA_GO_EXECUTABLE" hook cursor ` + event,
			"timeout": 3,
		}
		if event == "beforeMCPExecution" {
			definition["failClosed"] = true
		}
		encoded, err := json.Marshal(definition)
		if err != nil {
			return nil, err
		}
		found := false
		for _, existing := range hooks[event] {
			if jsonEquivalent(existing, encoded) {
				found = true
				break
			}
		}
		if !found {
			hooks[event] = append(hooks[event], encoded)
			changed = true
		}
	}
	if !changed {
		return func() error { return nil }, nil
	}
	encodedHooks, err := json.Marshal(hooks)
	if err != nil {
		return nil, fmt.Errorf("encode Cursor hooks: %w", err)
	}
	root["version"] = json.RawMessage("1")
	root["hooks"] = encodedHooks
	contents, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Cursor hooks configuration: %w", err)
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
			return fmt.Errorf("remove Cursor hooks configuration during rollback: %w", err)
		}
		return nil
	}, nil
}
