package provider

import (
	"context"
	"encoding/json"
	"time"
)

type AgentConfiguration struct {
	Kind            string   `json:"kind"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoning_effort"`
	Args            []string `json:"args,omitempty"`
}

type Capabilities struct {
	Kind          string `json:"kind"`
	Executable    string `json:"executable"`
	Version       string `json:"version"`
	Authenticated bool   `json:"authenticated"`
	Journal       bool   `json:"journal"`
	TurnStop      bool   `json:"turn_stop"`
	FollowUp      bool   `json:"follow_up"`
	FullOutput    bool   `json:"full_output"`
	FreshSession  bool   `json:"fresh_session"`
	Compatible    bool   `json:"compatible"`
}

type ProbeRequest struct {
	Executable string
}

type SessionActivation struct {
	AgentSessionID string
	Repository     string
	Configuration  AgentConfiguration
	SystemPrompt   []byte
	InitialPrompt  string
	Environment    map[string]string
}

type Launch struct {
	AgentKind            string
	Environment          map[string]string
	EphemeralPath        string
	StartupTimeout       time.Duration
	HandlesInitialPrompt bool
	ReturnOnLaunch       bool
	Cleanup              func() error
}

type SessionBinding struct {
	AgentSessionID string
	HookEventName  string
}

type JournalEventKind string

const (
	JournalObserved         JournalEventKind = "observed"
	JournalSessionStarted   JournalEventKind = "session_started"
	JournalSessionEnded     JournalEventKind = "session_ended"
	JournalUserMessage      JournalEventKind = "user_message"
	JournalAssistantMessage JournalEventKind = "assistant_message"
	JournalToolCompleted    JournalEventKind = "tool_completed"
	JournalToolFailed       JournalEventKind = "tool_failed"
	JournalToolSupplement   JournalEventKind = "tool_supplement"
	JournalTurnStopped      JournalEventKind = "turn_stopped"
)

type ToolEvent struct {
	ID           string
	Name         string
	Input        json.RawMessage
	Output       json.RawMessage
	Status       string
	ErrorMessage string
	FailureType  string
	DurationMS   int64
	Interrupted  bool
}

type ToolSupplement struct {
	Kind       string
	Name       string
	ServerName string
	Input      json.RawMessage
	Output     json.RawMessage
	DurationMS int64
}

type JournalEvent struct {
	Provider             string
	ProviderSessionID    string
	ProviderTurnID       string
	HookEventName        string
	Kind                 JournalEventKind
	UserMessage          string
	AssistantMessage     string
	Tool                 *ToolEvent
	Supplement           *ToolSupplement
	TurnStatus           string
	Raw                  json.RawMessage
	ProviderSessionEnded bool
}

type Adapter interface {
	Kind() string
	Validate(AgentConfiguration) error
	Probe(context.Context, ProbeRequest) (Capabilities, error)
	PrepareSession(context.Context, SessionActivation) (Launch, error)
	Normalize(SessionBinding, json.RawMessage) ([]JournalEvent, error)
}
