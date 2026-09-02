package provider_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestCursorAdapterNormalizesConversationAndFullToolEvidence(t *testing.T) {
	t.Parallel()
	adapter := provider.NewCursorAdapter()
	tests := []struct {
		name  string
		raw   string
		want  provider.JournalEventKind
		check func(*testing.T, provider.JournalEvent)
	}{
		{
			name:  "session-start",
			raw:   `{"conversation_id":"conversation","hook_event_name":"sessionStart"}`,
			want:  provider.JournalSessionStarted,
			check: func(*testing.T, provider.JournalEvent) {},
		},
		{
			name:  "configured-session-start",
			raw:   `{"session_id":"conversation"}`,
			want:  provider.JournalSessionStarted,
			check: func(*testing.T, provider.JournalEvent) {},
		},
		{
			name: "prompt",
			raw:  `{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"beforeSubmitPrompt","prompt":"measure it"}`,
			want: provider.JournalUserMessage,
			check: func(t *testing.T, event provider.JournalEvent) {
				if event.UserMessage != "measure it" {
					t.Fatalf("event = %+v", event)
				}
			},
		},
		{
			name: "response",
			raw:  `{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"afterAgentResponse","text":"done"}`,
			want: provider.JournalAssistantMessage,
			check: func(t *testing.T, event provider.JournalEvent) {
				if event.AssistantMessage != "done" {
					t.Fatalf("event = %+v", event)
				}
			},
		},
		{
			name: "successful-tool",
			raw:  `{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"postToolUse","tool_name":"Shell","tool_use_id":"tool-ok","tool_input":{"command":"true"},"tool_output":{"exit_code":0},"duration":7}`,
			want: provider.JournalToolCompleted,
			check: func(t *testing.T, event provider.JournalEvent) {
				if event.Tool == nil || event.Tool.Status != "completed" || event.Tool.ID != "tool-ok" || string(event.Tool.Output) != `{"exit_code":0}` {
					t.Fatalf("event = %+v", event)
				}
			},
		},
		{
			name: "camel-case-tool",
			raw:  `{"conversationId":"conversation","generationId":"generation","hookEventName":"postToolUse","toolName":"Shell","toolUseId":"tool-camel","toolInput":{"command":"true"},"toolOutput":{"exit_code":0},"duration":7}`,
			want: provider.JournalToolCompleted,
			check: func(t *testing.T, event provider.JournalEvent) {
				if event.Tool == nil || event.Tool.ID != "tool-camel" || event.Tool.Name != "Shell" || string(event.Tool.Output) != `{"exit_code":0}` {
					t.Fatalf("event = %+v", event)
				}
			},
		},
		{
			name: "failed-tool",
			raw:  `{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"postToolUseFailure","tool_name":"Shell","tool_use_id":"tool","tool_input":{"command":"false"},"error_message":"interrupted","failure_type":"error","duration":42,"is_interrupt":true}`,
			want: provider.JournalToolFailed,
			check: func(t *testing.T, event provider.JournalEvent) {
				if event.Tool == nil || event.Tool.Status != "failed" || event.Tool.ErrorMessage != "interrupted" || event.Tool.FailureType != "error" || event.Tool.DurationMS != 42 || !event.Tool.Interrupted {
					t.Fatalf("event = %+v", event)
				}
			},
		},
		{
			name: "shell-output",
			raw:  `{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"afterShellExecution","command":"make bench","output":"all output\n","duration":1234.75,"sandbox":true}`,
			want: provider.JournalToolSupplement,
			check: func(t *testing.T, event provider.JournalEvent) {
				if event.Supplement == nil || event.Supplement.Kind != "shell" || string(event.Supplement.Output) != `"all output\n"` || event.Supplement.DurationMS != 1234 {
					t.Fatalf("event = %+v", event)
				}
			},
		},
		{
			name: "mcp-output",
			raw:  `{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"afterMCPExecution","tool_name":"finish_iteration","tool_input":"{\"scope\":\"work\"}","mcp_server_name":"pika_go","result_json":"{\"ok\":true}","duration":15}`,
			want: provider.JournalToolSupplement,
			check: func(t *testing.T, event provider.JournalEvent) {
				if event.Supplement == nil || event.Supplement.Kind != "mcp" || event.Supplement.ServerName != "pika_go" || string(event.Supplement.Input) != `{"scope":"work"}` || string(event.Supplement.Output) != `{"ok":true}` {
					t.Fatalf("event = %+v", event)
				}
			},
		},
		{
			name: "stop",
			raw:  `{"conversation_id":"conversation","generation_id":"generation","hook_event_name":"stop","status":"aborted","loop_count":0}`,
			want: provider.JournalTurnStopped,
			check: func(t *testing.T, event provider.JournalEvent) {
				if event.TurnStatus != "aborted" {
					t.Fatalf("event = %+v", event)
				}
			},
		},
		{
			name: "session-end",
			raw:  `{"session_id":"conversation","hook_event_name":"sessionEnd","reason":"completed"}`,
			want: provider.JournalSessionEnded,
			check: func(t *testing.T, event provider.JournalEvent) {
				if !event.ProviderSessionEnded {
					t.Fatalf("event = %+v", event)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding := provider.SessionBinding{AgentSessionID: "pika-session"}
			if test.name == "configured-session-start" {
				binding.HookEventName = "sessionStart"
			}
			events, err := adapter.Normalize(binding, json.RawMessage(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Provider != "cursor" || events[0].ProviderSessionID != "conversation" || events[0].Kind != test.want {
				t.Fatalf("events = %+v", events)
			}
			test.check(t, events[0])
		})
	}
}

func TestCursorProbeRejectsVersionAndAuthenticationFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		wantErr string
	}{
		{name: "compatible"},
		{name: "authentication-status", wantErr: "authentication"},
		{name: "version", wantErr: "unsupported Cursor version"},
		{name: "authentication", wantErr: "authentication"},
		{name: "empty-authentication-error", wantErr: "exit status 1"},
		{name: "plugin-directory", wantErr: "--plugin-dir"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable := cursorFixtureExecutable(t, test.name)
			capabilities, err := provider.NewCursorAdapter().Probe(context.Background(), provider.ProbeRequest{Executable: executable, RequireSkillInjection: true})
			if test.wantErr == "" {
				if err != nil || !capabilities.Compatible || !capabilities.Authenticated || capabilities.Version != provider.CursorCandidateVersion {
					t.Fatalf("capabilities=%+v err=%v", capabilities, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
