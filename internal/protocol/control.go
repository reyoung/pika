package protocol

import (
	"encoding/json"

	"github.com/reyoung/pika-go/internal/symphony"
)

type Mutation struct {
	RequestID        string `json:"request_id"`
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

type InitRequest struct {
	Mutation
	Repository        string  `json:"repository"`
	CallerPaneID      string  `json:"caller_pane_id,omitempty"`
	ConfigurationTOML *string `json:"configuration_toml,omitempty"`
}

type ProviderOption struct {
	Kind          string          `json:"kind"`
	Executable    string          `json:"executable,omitempty"`
	Version       string          `json:"version,omitempty"`
	Compatible    bool            `json:"compatible"`
	Authenticated bool            `json:"authenticated"`
	Capabilities  map[string]bool `json:"capabilities,omitempty"`
	Models        []ModelOption   `json:"models,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type ModelOption struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name,omitempty"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
	Default          bool     `json:"default,omitempty"`
}

type InitOptionsResponse struct {
	ConfigurationExists bool             `json:"configuration_exists"`
	Providers           []ProviderOption `json:"providers"`
}

type DraftBaselineRequest struct {
	Mutation
}

type BackOffRequest struct {
	Mutation
	WorkID  string `json:"work_id"`
	Message string `json:"message"`
}

type CancelWorkRequest struct {
	Mutation
}

type ShutdownRequest struct {
	Mutation
}

type MaintenancePrepareRequest struct {
	RequestID    string                `json:"request_id"`
	ToGeneration MaintenanceGeneration `json:"to_generation"`
}

type MaintenanceResumeRequest struct {
	RequestID string `json:"request_id"`
}

type MaintenanceGeneration struct {
	Digest  string `json:"digest"`
	Version string `json:"version,omitempty"`
}

type SchedulerControlRequest struct {
	Mutation
}

type SchedulerControlResponse struct {
	Receipt symphony.Receipt                   `json:"receipt"`
	Control symphony.SchedulerControlCycleView `json:"control"`
}

type BackupRequest struct {
	Destination string `json:"destination"`
}

type BackupResponse struct {
	Destination string `json:"destination"`
	ByteSize    int64  `json:"byte_size"`
}

type ApplyGitIntentRequest struct {
	Message string `json:"message,omitempty"`
}

type ApplyGitIntentResponse struct {
	IntentID   string `json:"intent_id"`
	AppliedSHA string `json:"applied_sha"`
}

type ProviderEventRequest struct {
	AgentSessionID string          `json:"agent_session_id"`
	HookEventName  string          `json:"hook_event_name,omitempty"`
	Event          json.RawMessage `json:"event"`
}

type ErrorBody struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}
