package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type CodexAdapter struct{}

func NewCodexAdapter() CodexAdapter { return CodexAdapter{} }

func (CodexAdapter) Kind() string { return "codex" }

var agentValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func (CodexAdapter) Validate(configuration AgentConfiguration) error {
	if configuration.Kind != "codex" {
		return fmt.Errorf("Codex Adapter cannot validate Agent kind %q", configuration.Kind)
	}
	if configuration.Model == "" && configuration.ReasoningEffort == "" {
		if len(configuration.Args) != 0 {
			return errors.New("Codex launch args are not supported")
		}
		return nil
	}
	if !agentValuePattern.MatchString(configuration.Model) {
		return errors.New("invalid Codex model")
	}
	switch configuration.ReasoningEffort {
	case "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("invalid Codex reasoning_effort %q", configuration.ReasoningEffort)
	}
	if len(configuration.Args) != 0 {
		return errors.New("Codex launch args are not supported")
	}
	return nil
}

func (CodexAdapter) Probe(ctx context.Context, request ProbeRequest) (Capabilities, error) {
	executable := request.Executable
	if executable == "" {
		var err error
		executable, err = exec.LookPath("codex")
		if err != nil {
			return Capabilities{}, fmt.Errorf("resolve Codex executable: %w", err)
		}
	}
	output, err := exec.CommandContext(ctx, executable, "--version").CombinedOutput()
	if err != nil {
		return Capabilities{}, fmt.Errorf("probe Codex version: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if version == "" {
		return Capabilities{}, errors.New("probe Codex version: empty output")
	}
	if statusOutput, statusErr := exec.CommandContext(ctx, executable, "login", "status").CombinedOutput(); statusErr != nil {
		return Capabilities{}, fmt.Errorf("probe Codex authentication: %s", strings.TrimSpace(string(statusOutput)))
	}
	return Capabilities{
		Kind: "codex", Executable: executable, Version: version, Authenticated: true,
		Journal: true, TurnStop: true, FollowUp: true, FullOutput: true, FreshSession: true, Compatible: true,
	}, nil
}

func (adapter CodexAdapter) PrepareSession(_ context.Context, activation SessionActivation) (Launch, error) {
	if activation.Configuration.Kind != "codex" {
		return Launch{}, fmt.Errorf("Codex Adapter cannot prepare Agent kind %q", activation.Configuration.Kind)
	}
	if activation.Configuration.Model != "" || activation.Configuration.ReasoningEffort != "" || len(activation.Configuration.Args) != 0 {
		if err := adapter.Validate(activation.Configuration); err != nil {
			return Launch{}, err
		}
	}
	if len(activation.SystemPrompt) == 0 {
		return Launch{}, errors.New("frozen System Prompt is required")
	}
	environment := make(map[string]string, len(activation.Environment)+3)
	for key, value := range activation.Environment {
		environment[key] = value
	}
	environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"] = strconv.Quote(string(activation.SystemPrompt))
	if activation.Configuration.Model != "" {
		environment["PIKA_AGENT_MODEL"] = activation.Configuration.Model
	}
	if activation.Configuration.ReasoningEffort != "" {
		environment["PIKA_AGENT_REASONING_EFFORT"] = activation.Configuration.ReasoningEffort
	}
	return Launch{AgentKind: "codex", Environment: environment, Cleanup: func() error { return nil }}, nil
}

type codexHookEnvelope struct {
	SessionID            string          `json:"session_id"`
	TurnID               string          `json:"turn_id"`
	HookEventName        string          `json:"hook_event_name"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	ToolName             string          `json:"tool_name"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
}

func (CodexAdapter) Normalize(binding SessionBinding, raw json.RawMessage) ([]JournalEvent, error) {
	if binding.AgentSessionID == "" || len(raw) == 0 {
		return nil, errors.New("Pika Agent Session and provider event are required")
	}
	var envelope codexHookEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.SessionID == "" || envelope.HookEventName == "" {
		return nil, errors.New("provider hook envelope is invalid")
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return nil, errors.New("provider hook envelope must be JSON")
	}
	event := JournalEvent{
		Provider:          "codex",
		ProviderSessionID: envelope.SessionID,
		ProviderTurnID:    envelope.TurnID,
		HookEventName:     envelope.HookEventName,
		Kind:              JournalObserved,
		Raw:               canonical,
	}
	switch envelope.HookEventName {
	case "SessionStart":
		event.Kind = JournalSessionStarted
	case "SessionEnd":
		event.Kind = JournalSessionEnded
		event.ProviderSessionEnded = true
	case "UserPromptSubmit":
		event.Kind = JournalUserMessage
		event.UserMessage = envelope.Prompt
	case "PostToolUse":
		if envelope.TurnID == "" || envelope.ToolUseID == "" || envelope.ToolName == "" || len(envelope.ToolInput) == 0 {
			return nil, errors.New("PostToolUse identity and input are required")
		}
		input, err := canonicalJSON(envelope.ToolInput)
		if err != nil {
			return nil, errors.New("tool input must be valid JSON")
		}
		var output json.RawMessage
		if len(envelope.ToolResponse) != 0 && string(envelope.ToolResponse) != "null" {
			output, err = canonicalJSON(envelope.ToolResponse)
			if err != nil {
				return nil, errors.New("tool response must be valid JSON")
			}
		}
		event.Kind = JournalToolCompleted
		event.Tool = &ToolEvent{ID: envelope.ToolUseID, Name: envelope.ToolName, Input: input, Output: output}
	case "Stop":
		event.Kind = JournalTurnStopped
		event.AssistantMessage = envelope.LastAssistantMessage
	}
	return []JournalEvent{event}, nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
