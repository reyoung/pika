package contextbundle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/contextbundle"
	"github.com/reyoung/pika-go/internal/symphony"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestMaterializeFreezesCompleteCrossProviderHistoryAndValidSchemas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	engine, err := symphony.Open(ctx, filepath.Join(root, "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	workID := view.Works[0].ID
	first := symphony.AgentSession{ID: "codex-session", WorkID: workID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "codex-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	largeOutput := strings.Repeat("full-output-", 2048)
	for _, event := range []json.RawMessage{
		json.RawMessage(`{"session_id":"native-codex","turn_id":"codex-turn","hook_event_name":"UserPromptSubmit","prompt":"measure everything"}`),
		json.RawMessage(`{"session_id":"native-codex","turn_id":"codex-turn","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"codex-tool","tool_input":{"command":"benchmark"},"tool_response":{"output":` + mustJSON(t, largeOutput) + `}}`),
		json.RawMessage(`{"session_id":"native-codex","turn_id":"codex-turn","hook_event_name":"Stop","last_assistant_message":"codex answer"}`),
	} {
		if err := engine.IngestProviderEvent(ctx, "codex", first.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.MarkAgentSessionEnded(ctx, first.ID, symphony.AgentSessionExited); err != nil {
		t.Fatal(err)
	}
	second := symphony.AgentSession{ID: "cursor-session", WorkID: workID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "cursor", AgentName: "cursor-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, second); err != nil {
		t.Fatal(err)
	}
	for _, event := range []json.RawMessage{
		json.RawMessage(`{"conversation_id":"native-cursor","generation_id":"cursor-turn","hook_event_name":"beforeSubmitPrompt","prompt":"continue"}`),
		json.RawMessage(`{"conversation_id":"native-cursor","generation_id":"cursor-turn","hook_event_name":"postToolUse","tool_name":"Shell","tool_use_id":"cursor-tool","tool_input":{"command":"run"},"tool_output":"{\"exit_code\":0}"}`),
		json.RawMessage(`{"conversation_id":"native-cursor","generation_id":"cursor-turn","hook_event_name":"afterShellExecution","command":"run","output":"complete shell output","duration":17}`),
		json.RawMessage(`{"conversation_id":"native-cursor","generation_id":"cursor-turn","hook_event_name":"stop","status":"completed"}`),
	} {
		if err := engine.IngestProviderEvent(ctx, "cursor", second.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.MarkAgentSessionEnded(ctx, second.ID, symphony.AgentSessionExited); err != nil {
		t.Fatal(err)
	}
	current := symphony.AgentSession{ID: "current-session", WorkID: workID, Generation: 2, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "current-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, current); err != nil {
		t.Fatal(err)
	}
	materializer := contextbundle.Materializer{Store: engine, Root: filepath.Join(root, "contexts")}
	bundle, err := materializer.Materialize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	contextBytes, err := os.ReadFile(bundle.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	messagesBytes, err := os.ReadFile(bundle.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.MessageRecords != 2 || !bytes.Contains(messagesBytes, []byte(largeOutput)) || !bytes.Contains(messagesBytes, []byte("complete shell output")) || bytes.Contains(messagesBytes, []byte(`"events"`)) {
		t.Fatalf("incomplete or raw history: records=%d bytes=%d", bundle.MessageRecords, len(messagesBytes))
	}
	contextSchemaBytes, messageSchemaBytes, err := contextbundle.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	contextSchema := compileSchema(t, "context-schema.json", contextSchemaBytes)
	messageSchema := compileSchema(t, "message-schema.json", messageSchemaBytes)
	if err := contextSchema.Validate(unmarshalJSON(t, contextBytes)); err != nil {
		t.Fatalf("context.json does not match embedded schema: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(messagesBytes), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("messages lines = %d", len(lines))
	}
	for _, line := range lines {
		if err := messageSchema.Validate(unmarshalJSON(t, line)); err != nil {
			t.Fatalf("messages.jsonl record does not match embedded schema: %v\n%s", err, line)
		}
	}

	beforeContext, beforeMessages := append([]byte(nil), contextBytes...), append([]byte(nil), messagesBytes...)
	if err := engine.IngestProviderEvent(ctx, "codex", current.ID, json.RawMessage(`{"session_id":"native-current","turn_id":"late-turn","hook_event_name":"UserPromptSubmit","prompt":"late"}`)); err != nil {
		t.Fatal(err)
	}
	retried, err := materializer.Materialize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	afterContext, _ := os.ReadFile(retried.ContextPath)
	afterMessages, _ := os.ReadFile(retried.MessagesPath)
	if !bytes.Equal(beforeContext, afterContext) || !bytes.Equal(beforeMessages, afterMessages) {
		t.Fatal("same Agent Session did not reuse byte-identical Context Bundle")
	}
}

func TestMaterializeRejectsTamperedFrozenFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	engine, err := symphony.Open(ctx, filepath.Join(root, "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	session := symphony.AgentSession{ID: "session", WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	materializer := contextbundle.Materializer{Store: engine, Root: filepath.Join(root, "contexts")}
	bundle, err := materializer.Materialize(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bundle.ContextPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle.ContextPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(ctx, session); err == nil || !strings.Contains(err.Error(), "digest does not match") {
		t.Fatalf("tampered Context Bundle was accepted: %v", err)
	}
}

func compileSchema(t *testing.T, name string, contents []byte) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(name, unmarshalJSON(t, contents)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func unmarshalJSON(t *testing.T, contents []byte) any {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
