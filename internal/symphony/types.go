package symphony

import (
	"context"
	"encoding/json"
	"fmt"
)

type OptimizationStatus string

const (
	OptimizationDraftingBaseline  OptimizationStatus = "drafting_baseline"
	OptimizationVerifyingBaseline OptimizationStatus = "verifying_baseline"
	OptimizationOptimizing        OptimizationStatus = "optimizing"
	OptimizationPaused            OptimizationStatus = "paused"
	OptimizationDraining          OptimizationStatus = "draining"
)

type SchedulerStatus string

const (
	SchedulerRunning SchedulerStatus = "running"
	SchedulerPaused  SchedulerStatus = "paused"
)

type BaselineStatus string

const (
	BaselineDrafting  BaselineStatus = "drafting"
	BaselineSubmitted BaselineStatus = "submitted"
	BaselineVerifying BaselineStatus = "verifying"
	BaselineAccepted  BaselineStatus = "accepted"
	BaselineRejected  BaselineStatus = "rejected"
)

type WorkRole string

const (
	RoleBaselineDraft        WorkRole = "baseline_draft"
	RoleBaselineVerification WorkRole = "baseline_verification"
	RoleIteration            WorkRole = "iteration"
	RoleIntegration          WorkRole = "integration"
	RoleFollowUp             WorkRole = "follow_up"
)

type WorkStatus string

const (
	WorkPending   WorkStatus = "pending"
	WorkCompleted WorkStatus = "completed"
	WorkCancelled WorkStatus = "cancelled"
)

type CommandMeta struct {
	RequestID        string `json:"request_id"`
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

type Command interface {
	commandName() string
	commandMeta() CommandMeta
}

type Init struct {
	Meta                     CommandMeta `json:"meta"`
	OptimizationID           string      `json:"optimization_id"`
	Repository               string      `json:"repository"`
	CallerPaneID             string      `json:"caller_pane_id,omitempty"`
	IterationConcurrency     int64       `json:"iteration_concurrency,omitempty"`
	MaxPendingAttempts       int64       `json:"max_pending_attempts,omitempty"`
	IterationHistoryLimit    int64       `json:"iteration_history_limit,omitempty"`
	IterationHistoryLimitSet bool        `json:"-"`
}

func (Init) commandName() string        { return "init" }
func (c Init) commandMeta() CommandMeta { return c.Meta }

type SubmitBaselineDefinition struct {
	Meta          CommandMeta     `json:"meta"`
	WorkID        string          `json:"work_id"`
	Definition    json.RawMessage `json:"definition"`
	RepositorySHA string          `json:"repository_sha,omitempty"`
	Artifact      *ArtifactInput  `json:"artifact,omitempty"`
}

func (SubmitBaselineDefinition) commandName() string        { return "submit_baseline_definition" }
func (c SubmitBaselineDefinition) commandMeta() CommandMeta { return c.Meta }

type VerificationDecision string

const (
	VerificationAccepted VerificationDecision = "accepted"
	VerificationRejected VerificationDecision = "rejected"
)

type FinishBaselineVerification struct {
	Meta             CommandMeta          `json:"meta"`
	WorkID           string               `json:"work_id"`
	Decision         VerificationDecision `json:"decision"`
	FailureKind      string               `json:"failure_kind,omitempty"`
	Reason           string               `json:"reason,omitempty"`
	RequestedChanges string               `json:"requested_changes,omitempty"`
	Evidence         json.RawMessage      `json:"evidence,omitempty"`
	Artifacts        []ArtifactInput      `json:"artifacts,omitempty"`
	InitialBestSHA   string               `json:"initial_best_sha,omitempty"`
}

type ArtifactInput struct {
	RelativePath    string `json:"relative_path"`
	ByteSize        int64  `json:"byte_size"`
	ContentSHA256   string `json:"content_sha256"`
	ContractVersion int64  `json:"contract_version"`
}

type EvidenceArtifact struct {
	ID              string `json:"id"`
	WorkID          string `json:"work_id"`
	ReceiptID       string `json:"receipt_id"`
	RelativePath    string `json:"relative_path"`
	ByteSize        int64  `json:"byte_size"`
	ContentSHA256   string `json:"content_sha256"`
	ContractVersion int64  `json:"contract_version"`
}

func (FinishBaselineVerification) commandName() string        { return "finish_baseline_verification" }
func (c FinishBaselineVerification) commandMeta() CommandMeta { return c.Meta }

type IterationOutcome string

const (
	IterationCandidate IterationOutcome = "candidate"
	IterationRejected  IterationOutcome = "rejected"
)

type FinishIteration struct {
	Meta         CommandMeta      `json:"meta"`
	WorkID       string           `json:"work_id"`
	Outcome      IterationOutcome `json:"outcome"`
	CandidateSHA string           `json:"candidate_sha,omitempty"`
	Summary      string           `json:"summary"`
	Evidence     json.RawMessage  `json:"evidence,omitempty"`
}

func (FinishIteration) commandName() string        { return "finish_iteration" }
func (c FinishIteration) commandMeta() CommandMeta { return c.Meta }

type PrepareBestUpdate struct {
	Meta       CommandMeta     `json:"meta"`
	WorkID     string          `json:"work_id"`
	Validation json.RawMessage `json:"validation"`
}

func (PrepareBestUpdate) commandName() string        { return "prepare_best_update" }
func (c PrepareBestUpdate) commandMeta() CommandMeta { return c.Meta }

type IntegrationOutcome string

const (
	IntegrationAccepted IntegrationOutcome = "accepted"
	IntegrationRejected IntegrationOutcome = "rejected"
	IntegrationStale    IntegrationOutcome = "stale"
)

type FinishIntegration struct {
	Meta            CommandMeta        `json:"meta"`
	WorkID          string             `json:"work_id"`
	Outcome         IntegrationOutcome `json:"outcome"`
	Result          json.RawMessage    `json:"result"`
	ObservedBestSHA string             `json:"observed_best_sha,omitempty"`
	AppliedSHA      string             `json:"applied_sha,omitempty"`
}

func (FinishIntegration) commandName() string        { return "finish_integration" }
func (c FinishIntegration) commandMeta() CommandMeta { return c.Meta }

type SubmitFollowUpMessage struct {
	Meta    CommandMeta `json:"meta"`
	WorkID  string      `json:"work_id"`
	Message string      `json:"message"`
}

func (SubmitFollowUpMessage) commandName() string        { return "submit_followup_message" }
func (c SubmitFollowUpMessage) commandMeta() CommandMeta { return c.Meta }

type BackOff struct {
	Meta    CommandMeta `json:"meta"`
	WorkID  string      `json:"work_id"`
	Message string      `json:"message"`
}

func (BackOff) commandName() string        { return "back_off" }
func (c BackOff) commandMeta() CommandMeta { return c.Meta }

type CancelWork struct {
	Meta   CommandMeta `json:"meta"`
	WorkID string      `json:"work_id"`
}

func (CancelWork) commandName() string        { return "cancel_work" }
func (c CancelWork) commandMeta() CommandMeta { return c.Meta }

type RequestShutdown struct {
	Meta CommandMeta `json:"meta"`
}

func (RequestShutdown) commandName() string        { return "request_shutdown" }
func (c RequestShutdown) commandMeta() CommandMeta { return c.Meta }

type PauseScheduler struct {
	Meta CommandMeta `json:"meta"`
}

func (PauseScheduler) commandName() string        { return "pause_scheduler" }
func (c PauseScheduler) commandMeta() CommandMeta { return c.Meta }

type ResumeScheduler struct {
	Meta CommandMeta `json:"meta"`
}

func (ResumeScheduler) commandName() string        { return "resume_scheduler" }
func (c ResumeScheduler) commandMeta() CommandMeta { return c.Meta }

type StartBaselineDraft struct {
	Meta CommandMeta `json:"meta"`
}

func (StartBaselineDraft) commandName() string        { return "start_baseline_draft" }
func (c StartBaselineDraft) commandMeta() CommandMeta { return c.Meta }

type Query interface {
	queryName() string
}

type Status struct{}

func (Status) queryName() string { return "status" }

type Receipt struct {
	ID        string          `json:"id"`
	RequestID string          `json:"request_id"`
	Command   string          `json:"command"`
	Revision  int64           `json:"revision"`
	Result    json.RawMessage `json:"result"`
	Replayed  bool            `json:"replayed"`
}

type OptimizationView struct {
	ID                    string             `json:"id"`
	Status                OptimizationStatus `json:"status"`
	Revision              int64              `json:"revision"`
	Repository            string             `json:"repository"`
	IterationConcurrency  int64              `json:"iteration_concurrency"`
	MaxPendingAttempts    int64              `json:"max_pending_attempts"`
	IterationHistoryLimit int64              `json:"iteration_history_limit"`
}

type BaselineView struct {
	ID                   string          `json:"id"`
	Number               int64           `json:"number"`
	Status               BaselineStatus  `json:"status"`
	Definition           json.RawMessage `json:"definition,omitempty"`
	RepositorySHA        string          `json:"repository_sha,omitempty"`
	PredecessorID        string          `json:"predecessor_id,omitempty"`
	FailureKind          string          `json:"failure_kind,omitempty"`
	FailureReason        string          `json:"failure_reason,omitempty"`
	RequestedChanges     string          `json:"requested_changes,omitempty"`
	VerificationEvidence json.RawMessage `json:"verification_evidence,omitempty"`
}

type WorkView struct {
	ID                 string     `json:"id"`
	BaselineRevisionID string     `json:"baseline_revision_id"`
	Role               WorkRole   `json:"role"`
	Status             WorkStatus `json:"status"`
	Generation         int64      `json:"generation"`
	AttemptID          string     `json:"attempt_id,omitempty"`
	IterationRound     int64      `json:"iteration_round,omitempty"`
	IntegrationID      string     `json:"integration_id,omitempty"`
	ParentWorkID       string     `json:"parent_work_id,omitempty"`
	FollowUpRequestID  string     `json:"followup_request_id,omitempty"`
}

type AttemptView struct {
	ID                    string `json:"id"`
	SlotIndex             int64  `json:"slot_index"`
	Status                string `json:"status"`
	BaseBestSequence      int64  `json:"base_best_sequence"`
	BaseSHA               string `json:"base_sha"`
	CurrentIterationRound int64  `json:"current_iteration_round"`
	CandidateSHA          string `json:"candidate_sha,omitempty"`
	Summary               string `json:"summary,omitempty"`
	FailureReason         string `json:"failure_reason,omitempty"`
	HistoryLimit          int64  `json:"history_limit"`
}

type BestView struct {
	ID              string `json:"id"`
	Sequence        int64  `json:"sequence"`
	CommitSHA       string `json:"commit_sha"`
	SourceAttemptID string `json:"source_attempt_id,omitempty"`
}

type IntegrationView struct {
	Sequence        int64  `json:"sequence"`
	FIFOPosition    int64  `json:"fifo_position"`
	ID              string `json:"id"`
	AttemptID       string `json:"attempt_id"`
	IterationRound  int64  `json:"iteration_round"`
	Status          string `json:"status"`
	CandidateSHA    string `json:"candidate_sha"`
	ExpectedBestSHA string `json:"expected_best_sha"`
	IntentID        string `json:"intent_id,omitempty"`
}

type IterationRoundView struct {
	AttemptID      string `json:"attempt_id"`
	Round          int64  `json:"round"`
	Kind           string `json:"kind"`
	BaseSHA        string `json:"base_sha"`
	Status         string `json:"status"`
	BackOffMessage string `json:"back_off_message,omitempty"`
}

type GitIntentView struct {
	ID              string `json:"id"`
	IntegrationID   string `json:"integration_id"`
	State           string `json:"state"`
	ExpectedBestSHA string `json:"expected_best_sha"`
	CandidateSHA    string `json:"candidate_sha"`
}

type BackOffView struct {
	ID                          string `json:"id"`
	SourceWorkID                string `json:"source_work_id"`
	SuccessorBaselineRevisionID string `json:"successor_baseline_revision_id"`
	Message                     string `json:"message"`
}

type FollowUpView struct {
	ID                         string   `json:"id"`
	TargetWorkID               string   `json:"target_work_id"`
	TargetAgentSessionID       string   `json:"target_agent_session_id"`
	TargetProviderTurnID       string   `json:"target_provider_turn_id"`
	TargetRole                 WorkRole `json:"target_role"`
	RequestSequence            int64    `json:"request_sequence"`
	Status                     string   `json:"status"`
	InactivityTimeoutMS        int64    `json:"inactivity_timeout_ms"`
	DueAt                      string   `json:"due_at"`
	LastObservedPaneActivityAt string   `json:"last_observed_pane_activity_at,omitempty"`
	ActivitySource             string   `json:"activity_source,omitempty"`
	GeneratorWorkID            string   `json:"generator_work_id,omitempty"`
	Message                    string   `json:"message,omitempty"`
	DeliveryID                 string   `json:"delivery_id,omitempty"`
	TargetMaxMessages          int64    `json:"target_max_messages"`
	GeneratorMaxAttempts       int64    `json:"generator_max_attempts"`
	GeneratorAttempts          int64    `json:"generator_attempts"`
}

type FollowUpPolicy struct {
	MaxMessages          int64 `json:"max_messages"`
	GeneratorMaxAttempts int64 `json:"generator_max_attempts"`
}

type SchedulerControlActionStatus string

const (
	SchedulerActionPending         SchedulerControlActionStatus = "pending"
	SchedulerActionDispatching     SchedulerControlActionStatus = "dispatching"
	SchedulerActionSent            SchedulerControlActionStatus = "sent"
	SchedulerActionSkipped         SchedulerControlActionStatus = "skipped"
	SchedulerActionFailed          SchedulerControlActionStatus = "failed"
	SchedulerActionDeliveryUnknown SchedulerControlActionStatus = "delivery_unknown"
	SchedulerActionObserved        SchedulerControlActionStatus = "observed"
)

type SchedulerControlActionView struct {
	ID                  string                       `json:"id"`
	AgentSessionID      string                       `json:"agent_session_id"`
	WorkID              string                       `json:"work_id"`
	Role                WorkRole                     `json:"role"`
	AgentKind           string                       `json:"agent_kind"`
	AgentName           string                       `json:"agent_name"`
	Action              string                       `json:"action"`
	Message             string                       `json:"message,omitempty"`
	Status              SchedulerControlActionStatus `json:"status"`
	ObservedAgentStatus string                       `json:"observed_agent_status,omitempty"`
	Error               string                       `json:"error,omitempty"`
	StartedAt           string                       `json:"started_at,omitempty"`
	CompletedAt         string                       `json:"completed_at,omitempty"`
}

type SchedulerControlCycleView struct {
	ID          string                       `json:"id,omitempty"`
	Epoch       int64                        `json:"epoch"`
	Action      string                       `json:"action,omitempty"`
	Status      string                       `json:"status,omitempty"`
	CreatedAt   string                       `json:"created_at,omitempty"`
	CompletedAt string                       `json:"completed_at,omitempty"`
	Actions     []SchedulerControlActionView `json:"actions,omitempty"`
}

func (c SchedulerControlCycleView) HasDeliveryFailure() bool {
	for _, action := range c.Actions {
		if action.Status == SchedulerActionFailed || action.Status == SchedulerActionDeliveryUnknown {
			return true
		}
	}
	return false
}

type SchedulerView struct {
	Status   SchedulerStatus            `json:"status"`
	PausedAt string                     `json:"paused_at,omitempty"`
	Epoch    int64                      `json:"epoch"`
	Latest   *SchedulerControlCycleView `json:"latest_control,omitempty"`
}

type InitDiagnostic struct {
	ID        string `json:"id"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
}

type RuntimeEffect struct {
	Sequence       int64           `json:"sequence"`
	ID             string          `json:"id"`
	OptimizationID string          `json:"optimization_id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
}

type AgentSessionStatus string

const (
	AgentSessionStarting AgentSessionStatus = "starting"
	AgentSessionRunning  AgentSessionStatus = "running"
	AgentSessionExited   AgentSessionStatus = "exited"
	AgentSessionLost     AgentSessionStatus = "lost"
)

type AgentSession struct {
	ID                   string             `json:"id"`
	WorkID               string             `json:"work_id"`
	Generation           int64              `json:"generation"`
	Role                 WorkRole           `json:"role"`
	AgentKind            string             `json:"agent_kind"`
	AgentName            string             `json:"agent_name"`
	ProviderVersion      string             `json:"provider_version,omitempty"`
	ProviderCapabilities json.RawMessage    `json:"provider_capabilities,omitempty"`
	Status               AgentSessionStatus `json:"status"`
}

type PaneBinding struct {
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	PaneID      string `json:"pane_id"`
	TerminalID  string `json:"terminal_id"`
}

type RuntimeWork struct {
	Work                            WorkView           `json:"work"`
	Repository                      string             `json:"repository"`
	OptimizationRepository          string             `json:"optimization_repository"`
	OptimizationID                  string             `json:"optimization_id"`
	OptimizationStatus              OptimizationStatus `json:"optimization_status"`
	OptimizationRevision            int64              `json:"optimization_revision"`
	BaselineNumber                  int64              `json:"baseline_number"`
	BaselineStatus                  BaselineStatus     `json:"baseline_status"`
	BaselineDefinitionSHA256        string             `json:"baseline_definition_sha256,omitempty"`
	BaselineRepositorySHA           string             `json:"baseline_repository_sha,omitempty"`
	PredecessorBaselineID           string             `json:"predecessor_baseline_id,omitempty"`
	PredecessorFailureKind          string             `json:"predecessor_failure_kind,omitempty"`
	PredecessorFailureReason        string             `json:"predecessor_failure_reason,omitempty"`
	PredecessorRequestedChanges     string             `json:"predecessor_requested_changes,omitempty"`
	PredecessorVerificationEvidence json.RawMessage    `json:"predecessor_verification_evidence,omitempty"`
	BaseSHA                         string             `json:"base_sha,omitempty"`
	CandidateSHA                    string             `json:"candidate_sha,omitempty"`
	IterationKind                   string             `json:"iteration_kind,omitempty"`
	BackOffMessage                  string             `json:"back_off_message,omitempty"`
	BestSHA                         string             `json:"best_sha,omitempty"`
	BestSequence                    int64              `json:"best_sequence,omitempty"`
	ExpectedBestSHA                 string             `json:"expected_best_sha,omitempty"`
	IntegrationFIFOPosition         int64              `json:"integration_fifo_position,omitempty"`
	IntegrationStatus               string             `json:"integration_status,omitempty"`
	GitIntentID                     string             `json:"git_intent_id,omitempty"`
	GitIntentState                  string             `json:"git_intent_state,omitempty"`
	FollowUpTargetWorkID            string             `json:"followup_target_work_id,omitempty"`
	FollowUpTargetRole              WorkRole           `json:"followup_target_role,omitempty"`
	FollowUpSequence                int64              `json:"followup_sequence,omitempty"`
	FollowUpDueAt                   string             `json:"followup_due_at,omitempty"`
	FollowUpMaxMessages             int64              `json:"followup_max_messages,omitempty"`
	FollowUpGeneratorMax            int64              `json:"followup_generator_max_attempts,omitempty"`
	FollowUpGeneratorTry            int64              `json:"followup_generator_attempt,omitempty"`
	IterationHistoryLimit           int64              `json:"iteration_history_limit,omitempty"`
}

type ActiveAgentSession struct {
	Session AgentSession `json:"session"`
	Binding PaneBinding  `json:"binding"`
}

type AgentGrant struct {
	ID             string             `json:"id"`
	Token          string             `json:"-"`
	AgentSessionID string             `json:"agent_session_id"`
	WorkID         string             `json:"work_id"`
	Generation     int64              `json:"generation"`
	Role           WorkRole           `json:"role"`
	SessionStatus  AgentSessionStatus `json:"session_status"`
	Catalog        json.RawMessage    `json:"catalog"`
	ExpiresAt      string             `json:"expires_at"`
	Revoked        bool               `json:"revoked"`
}

type InstructionSnapshot struct {
	AgentSessionID   string `json:"agent_session_id"`
	LogicalName      string `json:"logical_name"`
	SourcePath       string `json:"source_path"`
	ContentSHA256    string `json:"content_sha256"`
	Content          []byte `json:"-"`
	SystemPrompt     []byte `json:"-"`
	ActivationSHA256 string `json:"activation_sha256"`
}

type ContextSnapshot struct {
	AgentSessionID       string `json:"agent_session_id"`
	SchemaVersion        int64  `json:"schema_version"`
	ContextRelativePath  string `json:"context_relative_path"`
	ContextSHA256        string `json:"context_sha256"`
	ContextBytes         int64  `json:"context_bytes"`
	MessagesRelativePath string `json:"messages_relative_path"`
	MessagesSHA256       string `json:"messages_sha256"`
	MessagesBytes        int64  `json:"messages_bytes"`
	MessageRecords       int64  `json:"message_records"`
}

type ContextProjection struct {
	Session          AgentSession               `json:"session"`
	View             View                       `json:"-"`
	TargetWork       RuntimeWork                `json:"target_work"`
	GeneratorWork    WorkView                   `json:"generator_work"`
	Journal          ConversationJournalView    `json:"-"`
	AttemptHistories []AttemptHistoryProjection `json:"-"`
	PreviousRound    *RoundHistoryProjection    `json:"-"`
}

type AttemptHistoryProjection struct {
	Attempt AttemptView             `json:"attempt"`
	Journal ConversationJournalView `json:"-"`
}

type RoundHistoryProjection struct {
	Work    WorkView                `json:"work"`
	Round   IterationRoundView      `json:"round"`
	Journal ConversationJournalView `json:"-"`
}

type View struct {
	Optimization       OptimizationView     `json:"optimization"`
	Scheduler          SchedulerView        `json:"scheduler"`
	Baseline           *BaselineView        `json:"baseline,omitempty"`
	Baselines          []BaselineView       `json:"baselines"`
	Works              []WorkView           `json:"works"`
	AgentSessions      []AgentSession       `json:"agent_sessions,omitempty"`
	BackOffs           []BackOffView        `json:"back_offs"`
	Attempts           []AttemptView        `json:"attempts"`
	Integrations       []IntegrationView    `json:"integrations"`
	IterationRounds    []IterationRoundView `json:"iteration_rounds"`
	Best               *BestView            `json:"best,omitempty"`
	FollowUps          []FollowUpView       `json:"follow_ups,omitempty"`
	PaneActivityNotice string               `json:"pane_activity_notice,omitempty"`
	DomainEventCount   int64                `json:"domain_event_count"`
	PendingEffectCount int64                `json:"pending_effect_count"`
	Storage            StorageView          `json:"storage"`
}

type StorageView struct {
	DatabaseBytes      int64 `json:"database_bytes"`
	WALBytes           int64 `json:"wal_bytes"`
	PageCount          int64 `json:"page_count"`
	PageSize           int64 `json:"page_size"`
	ProviderEventBytes int64 `json:"provider_event_bytes"`
	ToolPayloadBytes   int64 `json:"tool_payload_bytes"`
}

type ErrorCode string

const (
	CodeInvalidCommand      ErrorCode = "invalid_command"
	CodeInvalidTransition   ErrorCode = "invalid_transition"
	CodeRevisionConflict    ErrorCode = "revision_conflict"
	CodeIdempotencyConflict ErrorCode = "idempotency_conflict"
	CodeWorkNotFound        ErrorCode = "work_not_found"
	CodeWorkTerminal        ErrorCode = "work_terminal"
	CodeNotInitialized      ErrorCode = "daemon_not_initialized"
	CodeStateCorrupt        ErrorCode = "state_corrupt"
	CodeForbidden           ErrorCode = "forbidden"
)

type DomainError struct {
	Code    ErrorCode
	Message string
}

func (e *DomainError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type Symphony interface {
	Apply(context.Context, Command) (Receipt, error)
	Inspect(context.Context, Query) (View, error)
}
