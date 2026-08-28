package provider_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestCodexAdapterIsSelectedThroughRegistry(t *testing.T) {
	t.Parallel()
	registry, err := provider.NewRegistry(provider.NewCodexAdapter())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := registry.Resolve("codex")
	if err != nil {
		t.Fatal(err)
	}
	configuration := provider.AgentConfiguration{Kind: "codex", Model: "gpt-test", ReasoningEffort: "xhigh"}
	if err := adapter.Validate(configuration); err != nil {
		t.Fatalf("validate Codex configuration: %v", err)
	}
	launch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		Configuration: configuration,
		SystemPrompt:  []byte("frozen system prompt"),
		Environment:   map[string]string{"PIKA_SESSION_ID": "pika-session"},
	})
	if err != nil {
		t.Fatalf("prepare Codex Session: %v", err)
	}
	if launch.AgentKind != "codex" || launch.Environment["PIKA_SESSION_ID"] != "pika-session" ||
		launch.Environment["PIKA_AGENT_MODEL"] != "gpt-test" || launch.Environment["PIKA_AGENT_REASONING_EFFORT"] != "xhigh" ||
		launch.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"] != `"frozen system prompt"` {
		t.Fatalf("launch = %+v", launch)
	}
	raw := json.RawMessage(`{
		"tool_response":{"output":"12.4 ms","exit_code":0},
		"tool_input":{"command":"make benchmark"},
		"tool_use_id":"tool-1",
		"tool_name":"Bash",
		"hook_event_name":"PostToolUse",
		"turn_id":"turn-1",
		"session_id":"codex-session"
	}`)
	events, err := adapter.Normalize(provider.SessionBinding{AgentSessionID: "pika-session"}, raw)
	if err != nil {
		t.Fatalf("normalize Codex hook: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	event := events[0]
	if event.Provider != "codex" || event.ProviderSessionID != "codex-session" || event.ProviderTurnID != "turn-1" ||
		event.Kind != provider.JournalToolCompleted || event.Tool == nil || event.Tool.Name != "Bash" || event.Tool.ID != "tool-1" ||
		string(event.Tool.Input) != `{"command":"make benchmark"}` || string(event.Tool.Output) != `{"exit_code":0,"output":"12.4 ms"}` {
		t.Fatalf("event = %+v", event)
	}
}

func TestCodexAdapterAcceptsProviderDefaultModel(t *testing.T) {
	t.Parallel()
	adapter := provider.NewCodexAdapter()
	configuration := provider.AgentConfiguration{Kind: "codex"}
	if err := adapter.Validate(configuration); err != nil {
		t.Fatalf("validate Codex default configuration: %v", err)
	}
	launch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		Configuration: configuration, SystemPrompt: []byte("frozen system prompt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := launch.Environment["PIKA_AGENT_MODEL"]; present {
		t.Fatalf("Codex default unexpectedly overrides model: %+v", launch.Environment)
	}
	if _, present := launch.Environment["PIKA_AGENT_REASONING_EFFORT"]; present {
		t.Fatalf("Codex default unexpectedly overrides reasoning effort: %+v", launch.Environment)
	}
}
