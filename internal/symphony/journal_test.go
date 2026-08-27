package symphony_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/symphony"
)

func TestCodexHookJournalDeduplicatesAndCorrelatesSessionTurnAndTool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	session := symphony.AgentSession{ID: "pika-session", WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "tab", PaneID: "pane", TerminalID: "terminal"}); err != nil {
		t.Fatal(err)
	}
	events := []json.RawMessage{
		json.RawMessage(`{"session_id":"codex-session","hook_event_name":"SessionStart","cwd":"/repo","model":"gpt-test","source":"startup"}`),
		json.RawMessage(`{"session_id":"codex-session","turn_id":"turn-1","hook_event_name":"UserPromptSubmit","cwd":"/repo","prompt":"optimize this"}`),
		json.RawMessage(`{"session_id":"codex-session","turn_id":"turn-1","hook_event_name":"PostToolUse","cwd":"/repo","tool_name":"Bash","tool_use_id":"tool-1","tool_input":{"command":"make benchmark"},"tool_response":{"output":"12.4 ms","exit_code":0}}`),
		json.RawMessage(`{"session_id":"codex-session","turn_id":"turn-1","hook_event_name":"Stop","cwd":"/repo","stop_hook_active":false,"last_assistant_message":"benchmark completed"}`),
	}
	for _, event := range events {
		if err := engine.IngestProviderEvent(ctx, "codex", session.ID, event); err != nil {
			t.Fatalf("ingest %s: %v", event, err)
		}
	}
	if err := engine.IngestProviderEvent(ctx, "codex", session.ID, events[2]); err != nil {
		t.Fatalf("replay tool hook: %v", err)
	}
	journal, err := engine.ConversationJournal(ctx, view.Works[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.ProviderSessionID != "codex-session" || len(journal.Events) != 4 || len(journal.Turns) != 1 || len(journal.Tools) != 1 {
		t.Fatalf("journal = %+v", journal)
	}
	turn := journal.Turns[0]
	if turn.ProviderTurnID != "turn-1" || turn.UserMessage != "optimize this" || turn.AssistantMessage != "benchmark completed" || turn.Status != "stopped" {
		t.Fatalf("turn = %+v", turn)
	}
	tool := journal.Tools[0]
	if tool.ToolName != "Bash" || string(tool.Input) != `{"command":"make benchmark"}` || string(tool.Output) != `{"exit_code":0,"output":"12.4 ms"}` {
		t.Fatalf("tool = %+v", tool)
	}
}

func TestProviderEventCannotRebindCodexSessionAcrossPikaSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	for _, id := range []string{"pika-one", "pika-two"} {
		if err := engine.EnsureAgentSession(ctx, symphony.AgentSession{ID: id, WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: id, Status: symphony.AgentSessionStarting}); err != nil {
			if id == "pika-two" {
				// The domain already rejects two current sessions for one Work; use a historical replacement identity.
				continue
			}
			t.Fatal(err)
		}
	}
	event := json.RawMessage(`{"session_id":"codex-session","hook_event_name":"SessionStart","cwd":"/repo","source":"startup"}`)
	if err := engine.IngestProviderEvent(ctx, "codex", "pika-one", event); err != nil {
		t.Fatal(err)
	}
	if err := engine.IngestProviderEvent(ctx, "codex", "missing-pika-session", event); err == nil {
		t.Fatal("provider session rebound to another Pika identity")
	}
}
