package contextbundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/reyoung/pika-go/internal/evidence"
	"github.com/reyoung/pika-go/internal/kdacontract"
	"github.com/reyoung/pika-go/internal/skillsnapshot"
	"github.com/reyoung/pika-go/internal/symphony"
)

const (
	// SchemaVersion is the flow-v2 artifact-first Context contract. Existing
	// flow-v1 sessions are still materialized as LegacySchemaVersion.
	SchemaVersion       int64 = 4
	FlowV3SchemaVersion int64 = 5
	LegacySchemaVersion int64 = 3
)

//go:embed schemas/*.json
var schemaFiles embed.FS

type Store interface {
	ContextProjection(context.Context, string) (symphony.ContextProjection, error)
	ReadContextSnapshot(context.Context, string) (symphony.ContextSnapshot, bool, error)
	FreezeContextSnapshot(context.Context, symphony.ContextSnapshot) (symphony.ContextSnapshot, error)
}

type Materializer struct {
	Store        Store
	Root         string
	EvidenceRoot string
}

type Bundle struct {
	SchemaVersion  int64
	ContextPath    string
	ContextSHA256  string
	MessagesPath   string
	MessagesSHA256 string
	MessageRecords int64
}

type FileReference struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
	Records int64  `json:"records"`
}

type BlobReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type AttemptHistory struct {
	Attempt  symphony.AttemptView `json:"attempt"`
	Messages FileReference        `json:"messages"`
	Summary  FileReference        `json:"summary"`
}

type IterationContext struct {
	HistoryLimit           int64                         `json:"history_limit"`
	RequiredCaseSet        symphony.IterationCaseSetView `json:"required_case_set"`
	RecentTerminalAttempts []AttemptHistory              `json:"recent_terminal_attempts"`
	EvidenceRoot           string                        `json:"evidence_root,omitempty"`
	PreviousRound          *RoundHistory                 `json:"previous_round,omitempty"`
	Experiments            *FileReference                `json:"experiments,omitempty"`
}

type IntegrationContext struct {
	CandidateExperimentID string        `json:"candidate_experiment_id"`
	Experiments           FileReference `json:"experiments"`
}

type BenchmarkContext struct {
	EvidenceRoot string `json:"evidence_root"`
}

type SkillSnapshotReference struct {
	SchemaVersion int64                         `json:"schema_version"`
	SnapshotID    string                        `json:"snapshot_id"`
	Entries       []symphony.SkillSnapshotEntry `json:"entries"`
	Manifest      BlobReference                 `json:"manifest"`
}

type DiagnosisReference struct {
	ID           string                   `json:"id"`
	BaselineID   string                   `json:"baseline_revision_id"`
	WorkID       string                   `json:"work_id"`
	Status       symphony.DiagnosisStatus `json:"status"`
	Hypotheses   int64                    `json:"hypothesis_count"`
	Report       *BlobReference           `json:"report,omitempty"`
	EvidenceRoot string                   `json:"evidence_root,omitempty"`
}

type RoundHistory struct {
	Work     symphony.WorkView           `json:"work"`
	Round    symphony.IterationRoundView `json:"round"`
	Messages FileReference               `json:"messages"`
}

type Document struct {
	SchemaVersion     int64                           `json:"schema_version"`
	Session           symphony.AgentSession           `json:"session"`
	Optimization      symphony.OptimizationView       `json:"optimization"`
	Baseline          *symphony.BaselineView          `json:"baseline,omitempty"`
	Best              *symphony.BestView              `json:"best,omitempty"`
	IterationCaseSet  *symphony.IterationCaseSetView  `json:"iteration_case_set,omitempty"`
	Attempts          []symphony.AttemptView          `json:"attempts"`
	IterationRounds   []symphony.IterationRoundView   `json:"iteration_rounds"`
	Integrations      []symphony.IntegrationView      `json:"integrations"`
	BackOffs          []symphony.BackOffView          `json:"back_offs"`
	Work              symphony.RuntimeWork            `json:"work"`
	GeneratorWork     symphony.WorkView               `json:"generator_work"`
	TerminalOperation string                          `json:"terminal_operation"`
	Messages          FileReference                   `json:"messages"`
	Iteration         *IterationContext               `json:"iteration_context,omitempty"`
	Integration       *IntegrationContext             `json:"integration_context,omitempty"`
	SkillSnapshot     *SkillSnapshotReference         `json:"skill_snapshot,omitempty"`
	Diagnosis         *DiagnosisReference             `json:"diagnosis,omitempty"`
	Knowledge         *FileReference                  `json:"knowledge,omitempty"`
	AllowedTerminals  []string                        `json:"allowed_terminal_operations,omitempty"`
	ExperimentCycles  []symphony.ExperimentCycleView  `json:"experiment_cycles,omitempty"`
	ReferenceReceipts []symphony.ReferenceReceiptView `json:"reference_receipts,omitempty"`
	ExperimentCycle   *symphony.ExperimentCycleView   `json:"experiment_cycle,omitempty"`
	ReferenceReceipt  *symphony.ReferenceReceiptView  `json:"reference_receipt,omitempty"`
	Benchmark         *BenchmarkContext               `json:"benchmark_context,omitempty"`
}

// v3Document freezes the legacy wire surface independently from the
// artifact-first v4 projection. Document is only an internal assembly model;
// adding a v4 field there cannot silently change an existing v3 snapshot.
type v3Document struct {
	SchemaVersion     int64                          `json:"schema_version"`
	Session           symphony.AgentSession          `json:"session"`
	Optimization      symphony.OptimizationView      `json:"optimization"`
	Baseline          *symphony.BaselineView         `json:"baseline,omitempty"`
	Best              *symphony.BestView             `json:"best,omitempty"`
	IterationCaseSet  *symphony.IterationCaseSetView `json:"iteration_case_set,omitempty"`
	Attempts          []symphony.AttemptView         `json:"attempts"`
	IterationRounds   []symphony.IterationRoundView  `json:"iteration_rounds"`
	Integrations      []symphony.IntegrationView     `json:"integrations"`
	BackOffs          []symphony.BackOffView         `json:"back_offs"`
	Work              symphony.RuntimeWork           `json:"work"`
	GeneratorWork     symphony.WorkView              `json:"generator_work"`
	TerminalOperation string                         `json:"terminal_operation"`
	Messages          FileReference                  `json:"messages"`
	Iteration         *v3IterationContext            `json:"iteration_context,omitempty"`
}

type v3IterationContext struct {
	HistoryLimit           int64                         `json:"history_limit"`
	RequiredCaseSet        symphony.IterationCaseSetView `json:"required_case_set"`
	RecentTerminalAttempts []AttemptHistory              `json:"recent_terminal_attempts"`
	PreviousRound          *RoundHistory                 `json:"previous_round,omitempty"`
}

func v3DocumentFrom(value Document) v3Document {
	result := v3Document{
		SchemaVersion: value.SchemaVersion, Session: value.Session, Optimization: value.Optimization,
		Baseline: value.Baseline, Best: value.Best, IterationCaseSet: value.IterationCaseSet,
		Attempts: value.Attempts, IterationRounds: value.IterationRounds, Integrations: value.Integrations, BackOffs: value.BackOffs,
		Work: value.Work, GeneratorWork: value.GeneratorWork, TerminalOperation: value.TerminalOperation, Messages: value.Messages,
	}
	if value.Iteration != nil {
		result.Iteration = &v3IterationContext{
			HistoryLimit: value.Iteration.HistoryLimit, RequiredCaseSet: value.Iteration.RequiredCaseSet,
			RecentTerminalAttempts: value.Iteration.RecentTerminalAttempts, PreviousRound: value.Iteration.PreviousRound,
		}
	}
	return result
}

type SummaryRecord struct {
	SchemaVersion int64                `json:"schema_version"`
	Attempt       symphony.AttemptView `json:"attempt"`
}

// v4Document is deliberately a separate wire model.  Unlike Document (the
// frozen v3 compatibility model), none of these types embeds a Symphony view
// that can later grow an unreviewed raw JSON field into context.json.
type v4Document struct {
	SchemaVersion     int64               `json:"schema_version"`
	Session           v4Session           `json:"session"`
	Optimization      v4Optimization      `json:"optimization"`
	Baseline          *v4Baseline         `json:"baseline,omitempty"`
	Best              *v4Best             `json:"best,omitempty"`
	IterationCaseSet  *v4CaseSet          `json:"iteration_case_set,omitempty"`
	Attempts          []v4Attempt         `json:"attempts"`
	IterationRounds   []v4Round           `json:"iteration_rounds"`
	Integrations      []v4Integration     `json:"integrations"`
	BackOffs          []v4BackOff         `json:"back_offs"`
	Work              v4RuntimeWork       `json:"work"`
	GeneratorWork     v4Work              `json:"generator_work"`
	TerminalOperation string              `json:"terminal_operation"`
	Messages          FileReference       `json:"messages"`
	Iteration         *v4IterationContext `json:"iteration_context,omitempty"`
	Integration       *IntegrationContext `json:"integration_context,omitempty"`
	SkillSnapshot     *v4SkillSnapshotRef `json:"skill_snapshot"`
	Diagnosis         *DiagnosisReference `json:"diagnosis,omitempty"`
	Knowledge         *FileReference      `json:"knowledge"`
}

type v4Session struct {
	ID              string                      `json:"id"`
	WorkID          string                      `json:"work_id"`
	Generation      int64                       `json:"generation"`
	Role            symphony.WorkRole           `json:"role"`
	AgentKind       string                      `json:"agent_kind"`
	AgentName       string                      `json:"agent_name"`
	ProviderVersion string                      `json:"provider_version,omitempty"`
	Status          symphony.AgentSessionStatus `json:"status"`
}
type v4Optimization struct {
	ID                    string                      `json:"id"`
	Status                symphony.OptimizationStatus `json:"status"`
	Revision              int64                       `json:"revision"`
	Repository            string                      `json:"repository"`
	IterationConcurrency  int64                       `json:"iteration_concurrency"`
	MaxPendingAttempts    int64                       `json:"max_pending_attempts"`
	IterationHistoryLimit int64                       `json:"iteration_history_limit"`
	FlowVersion           symphony.FlowVersion        `json:"flow_version"`
}
type v4Work struct {
	ID                 string              `json:"id"`
	BaselineRevisionID string              `json:"baseline_revision_id"`
	Role               symphony.WorkRole   `json:"role"`
	Status             symphony.WorkStatus `json:"status"`
	Generation         int64               `json:"generation"`
	AttemptID          string              `json:"attempt_id,omitempty"`
	IterationRound     int64               `json:"iteration_round,omitempty"`
	IntegrationID      string              `json:"integration_id,omitempty"`
	ParentWorkID       string              `json:"parent_work_id,omitempty"`
	FollowUpRequestID  string              `json:"followup_request_id,omitempty"`
}
type v4CaseSet struct {
	Version int64    `json:"version"`
	CaseIDs []string `json:"case_ids"`
}
type v4Attempt struct {
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
type v4Round struct {
	AttemptID               string `json:"attempt_id"`
	Round                   int64  `json:"round"`
	Kind                    string `json:"kind"`
	BaseSHA                 string `json:"base_sha"`
	Status                  string `json:"status"`
	BackOffMessage          string `json:"back_off_message,omitempty"`
	IterationCaseSetVersion int64  `json:"iteration_case_set_version"`
	CurrentCheckpointSHA    string `json:"current_checkpoint_sha,omitempty"`
}
type v4Regression struct {
	CaseID  string `json:"case_id"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}
type v4Integration struct {
	Sequence              int64          `json:"sequence"`
	FIFOPosition          int64          `json:"fifo_position"`
	ID                    string         `json:"id"`
	AttemptID             string         `json:"attempt_id"`
	IterationRound        int64          `json:"iteration_round"`
	Status                string         `json:"status"`
	CandidateSHA          string         `json:"candidate_sha"`
	ExpectedBestSHA       string         `json:"expected_best_sha"`
	CandidateExperimentID string         `json:"candidate_experiment_id,omitempty"`
	IntentID              string         `json:"intent_id,omitempty"`
	RegressionCases       []v4Regression `json:"regression_cases,omitempty"`
}
type v4BackOff struct {
	ID                          string `json:"id"`
	SourceWorkID                string `json:"source_work_id"`
	SuccessorBaselineRevisionID string `json:"successor_baseline_revision_id"`
	Message                     string `json:"message"`
}
type v4Baseline struct {
	ID               string                  `json:"id"`
	Number           int64                   `json:"number"`
	Status           symphony.BaselineStatus `json:"status"`
	RepositorySHA    string                  `json:"repository_sha,omitempty"`
	PredecessorID    string                  `json:"predecessor_id,omitempty"`
	FailureKind      string                  `json:"failure_kind,omitempty"`
	FailureReason    string                  `json:"failure_reason,omitempty"`
	RequestedChanges string                  `json:"requested_changes,omitempty"`
}
type v4Best struct {
	ID              string `json:"id"`
	Sequence        int64  `json:"sequence"`
	CommitSHA       string `json:"commit_sha"`
	SourceAttemptID string `json:"source_attempt_id,omitempty"`
}
type v4RuntimeWork struct {
	Work                        v4Work                      `json:"work"`
	Repository                  string                      `json:"repository"`
	OptimizationRepository      string                      `json:"optimization_repository"`
	OptimizationID              string                      `json:"optimization_id"`
	OptimizationStatus          symphony.OptimizationStatus `json:"optimization_status"`
	OptimizationRevision        int64                       `json:"optimization_revision"`
	BaselineNumber              int64                       `json:"baseline_number"`
	BaselineStatus              symphony.BaselineStatus     `json:"baseline_status"`
	BaselineDefinitionSHA256    string                      `json:"baseline_definition_sha256,omitempty"`
	BaselineRepositorySHA       string                      `json:"baseline_repository_sha,omitempty"`
	PredecessorBaselineID       string                      `json:"predecessor_baseline_id,omitempty"`
	PredecessorFailureKind      string                      `json:"predecessor_failure_kind,omitempty"`
	PredecessorFailureReason    string                      `json:"predecessor_failure_reason,omitempty"`
	PredecessorRequestedChanges string                      `json:"predecessor_requested_changes,omitempty"`
	BaseSHA                     string                      `json:"base_sha,omitempty"`
	CandidateSHA                string                      `json:"candidate_sha,omitempty"`
	IterationKind               string                      `json:"iteration_kind,omitempty"`
	BackOffMessage              string                      `json:"back_off_message,omitempty"`
	BestSHA                     string                      `json:"best_sha,omitempty"`
	BestSequence                int64                       `json:"best_sequence,omitempty"`
	ExpectedBestSHA             string                      `json:"expected_best_sha,omitempty"`
	IntegrationFIFOPosition     int64                       `json:"integration_fifo_position,omitempty"`
	IntegrationStatus           string                      `json:"integration_status,omitempty"`
	GitIntentID                 string                      `json:"git_intent_id,omitempty"`
	GitIntentState              string                      `json:"git_intent_state,omitempty"`
	FollowUpTargetWorkID        string                      `json:"followup_target_work_id,omitempty"`
	FollowUpTargetRole          symphony.WorkRole           `json:"followup_target_role,omitempty"`
	FollowUpSequence            int64                       `json:"followup_sequence,omitempty"`
	FollowUpDueAt               string                      `json:"followup_due_at,omitempty"`
	FollowUpMaxMessages         int64                       `json:"followup_max_messages,omitempty"`
	FollowUpGeneratorMax        int64                       `json:"followup_generator_max_attempts,omitempty"`
	FollowUpGeneratorTry        int64                       `json:"followup_generator_attempt,omitempty"`
	IterationHistoryLimit       int64                       `json:"iteration_history_limit,omitempty"`
	IterationCaseSet            *v4CaseSet                  `json:"iteration_case_set,omitempty"`
	FlowVersion                 symphony.FlowVersion        `json:"flow_version"`
	CurrentCheckpointSHA        string                      `json:"current_checkpoint_sha,omitempty"`
}
type v4AttemptHistory struct {
	Attempt  v4Attempt     `json:"attempt"`
	Messages FileReference `json:"messages"`
	Summary  FileReference `json:"summary"`
}
type v4RoundHistory struct {
	Work     v4Work        `json:"work"`
	Round    v4Round       `json:"round"`
	Messages FileReference `json:"messages"`
}
type v4IterationContext struct {
	HistoryLimit           int64              `json:"history_limit"`
	RequiredCaseSet        v4CaseSet          `json:"required_case_set"`
	RecentTerminalAttempts []v4AttemptHistory `json:"recent_terminal_attempts"`
	EvidenceRoot           string             `json:"evidence_root"`
	PreviousRound          *v4RoundHistory    `json:"previous_round,omitempty"`
	Experiments            *FileReference     `json:"experiments,omitempty"`
}
type v4SkillSnapshotEntry struct {
	Name          string `json:"name"`
	Repository    string `json:"repository"`
	Branch        string `json:"branch"`
	CommitSHA     string `json:"commit_sha"`
	Path          string `json:"path"`
	ContentSHA256 string `json:"content_sha256"`
}
type v4SkillSnapshotRef struct {
	SchemaVersion int64                  `json:"schema_version"`
	SnapshotID    string                 `json:"snapshot_id"`
	Entries       []v4SkillSnapshotEntry `json:"entries"`
	Manifest      BlobReference          `json:"manifest"`
}

// v5 adds the flow-v3 benchmark gate without changing the frozen v4 wire
// surface. The embedded v4 fields retain their reviewed projection; every new
// field is explicit here.
type v5Document struct {
	v4Document
	AllowedTerminals  []string                        `json:"allowed_terminal_operations"`
	ExperimentCycles  []symphony.ExperimentCycleView  `json:"experiment_cycles"`
	ReferenceReceipts []symphony.ReferenceReceiptView `json:"reference_receipts"`
	ExperimentCycle   *symphony.ExperimentCycleView   `json:"experiment_cycle,omitempty"`
	ReferenceReceipt  *symphony.ReferenceReceiptView  `json:"reference_receipt,omitempty"`
	Benchmark         *BenchmarkContext               `json:"benchmark_context,omitempty"`
}

func v5DocumentFrom(value Document) (v5Document, error) {
	base, err := v4DocumentFrom(value)
	if err != nil {
		return v5Document{}, err
	}
	return v5Document{
		v4Document: base, AllowedTerminals: append([]string{}, value.AllowedTerminals...),
		ExperimentCycles:  append([]symphony.ExperimentCycleView{}, value.ExperimentCycles...),
		ReferenceReceipts: append([]symphony.ReferenceReceiptView{}, value.ReferenceReceipts...),
		ExperimentCycle:   value.ExperimentCycle, ReferenceReceipt: value.ReferenceReceipt, Benchmark: value.Benchmark,
	}, nil
}

func v4WorkFrom(value symphony.WorkView) v4Work {
	return v4Work{ID: value.ID, BaselineRevisionID: value.BaselineRevisionID, Role: value.Role, Status: value.Status, Generation: value.Generation, AttemptID: value.AttemptID, IterationRound: value.IterationRound, IntegrationID: value.IntegrationID, ParentWorkID: value.ParentWorkID, FollowUpRequestID: value.FollowUpRequestID}
}
func v4CaseSetFrom(value symphony.IterationCaseSetView) v4CaseSet {
	return v4CaseSet{Version: value.Version, CaseIDs: append([]string{}, value.CaseIDs...)}
}
func v4AttemptFrom(value symphony.AttemptView) v4Attempt {
	return v4Attempt{ID: value.ID, SlotIndex: value.SlotIndex, Status: value.Status, BaseBestSequence: value.BaseBestSequence, BaseSHA: value.BaseSHA, CurrentIterationRound: value.CurrentIterationRound, CandidateSHA: value.CandidateSHA, Summary: value.Summary, FailureReason: value.FailureReason, HistoryLimit: value.HistoryLimit}
}
func v4RoundFrom(value symphony.IterationRoundView) v4Round {
	return v4Round{AttemptID: value.AttemptID, Round: value.Round, Kind: value.Kind, BaseSHA: value.BaseSHA, Status: value.Status, BackOffMessage: value.BackOffMessage, IterationCaseSetVersion: value.IterationCaseSetVersion, CurrentCheckpointSHA: value.CurrentCheckpointSHA}
}

func v4RuntimeWorkFrom(value symphony.RuntimeWork) v4RuntimeWork {
	result := v4RuntimeWork{Work: v4WorkFrom(value.Work), Repository: value.Repository, OptimizationRepository: value.OptimizationRepository, OptimizationID: value.OptimizationID, OptimizationStatus: value.OptimizationStatus, OptimizationRevision: value.OptimizationRevision, BaselineNumber: value.BaselineNumber, BaselineStatus: value.BaselineStatus, BaselineDefinitionSHA256: value.BaselineDefinitionSHA256, BaselineRepositorySHA: value.BaselineRepositorySHA, PredecessorBaselineID: value.PredecessorBaselineID, PredecessorFailureKind: value.PredecessorFailureKind, PredecessorFailureReason: value.PredecessorFailureReason, PredecessorRequestedChanges: value.PredecessorRequestedChanges, BaseSHA: value.BaseSHA, CandidateSHA: value.CandidateSHA, IterationKind: value.IterationKind, BackOffMessage: value.BackOffMessage, BestSHA: value.BestSHA, BestSequence: value.BestSequence, ExpectedBestSHA: value.ExpectedBestSHA, IntegrationFIFOPosition: value.IntegrationFIFOPosition, IntegrationStatus: value.IntegrationStatus, GitIntentID: value.GitIntentID, GitIntentState: value.GitIntentState, FollowUpTargetWorkID: value.FollowUpTargetWorkID, FollowUpTargetRole: value.FollowUpTargetRole, FollowUpSequence: value.FollowUpSequence, FollowUpDueAt: value.FollowUpDueAt, FollowUpMaxMessages: value.FollowUpMaxMessages, FollowUpGeneratorMax: value.FollowUpGeneratorMax, FollowUpGeneratorTry: value.FollowUpGeneratorTry, IterationHistoryLimit: value.IterationHistoryLimit, FlowVersion: value.FlowVersion, CurrentCheckpointSHA: value.CurrentCheckpointSHA}
	if value.IterationCaseSet != nil {
		caseSet := v4CaseSetFrom(*value.IterationCaseSet)
		result.IterationCaseSet = &caseSet
	}
	return result
}

func v4DocumentFrom(value Document) (v4Document, error) {
	result := v4Document{SchemaVersion: value.SchemaVersion, Session: v4Session{ID: value.Session.ID, WorkID: value.Session.WorkID, Generation: value.Session.Generation, Role: value.Session.Role, AgentKind: value.Session.AgentKind, AgentName: value.Session.AgentName, ProviderVersion: value.Session.ProviderVersion, Status: value.Session.Status}, Optimization: v4Optimization{ID: value.Optimization.ID, Status: value.Optimization.Status, Revision: value.Optimization.Revision, Repository: value.Optimization.Repository, IterationConcurrency: value.Optimization.IterationConcurrency, MaxPendingAttempts: value.Optimization.MaxPendingAttempts, IterationHistoryLimit: value.Optimization.IterationHistoryLimit, FlowVersion: value.Optimization.FlowVersion}, Attempts: []v4Attempt{}, IterationRounds: []v4Round{}, Integrations: []v4Integration{}, BackOffs: []v4BackOff{}, Work: v4RuntimeWorkFrom(value.Work), GeneratorWork: v4WorkFrom(value.GeneratorWork), TerminalOperation: value.TerminalOperation, Messages: value.Messages, Integration: value.Integration, Diagnosis: value.Diagnosis, Knowledge: value.Knowledge}
	if value.Baseline != nil {
		result.Baseline = &v4Baseline{ID: value.Baseline.ID, Number: value.Baseline.Number, Status: value.Baseline.Status, RepositorySHA: value.Baseline.RepositorySHA, PredecessorID: value.Baseline.PredecessorID, FailureKind: value.Baseline.FailureKind, FailureReason: value.Baseline.FailureReason, RequestedChanges: value.Baseline.RequestedChanges}
	}
	if value.Best != nil {
		result.Best = &v4Best{ID: value.Best.ID, Sequence: value.Best.Sequence, CommitSHA: value.Best.CommitSHA, SourceAttemptID: value.Best.SourceAttemptID}
	}
	if value.IterationCaseSet != nil {
		caseSet := v4CaseSetFrom(*value.IterationCaseSet)
		result.IterationCaseSet = &caseSet
	}
	for _, item := range value.Attempts {
		result.Attempts = append(result.Attempts, v4AttemptFrom(item))
	}
	for _, item := range value.IterationRounds {
		result.IterationRounds = append(result.IterationRounds, v4RoundFrom(item))
	}
	for _, item := range value.Integrations {
		integration := v4Integration{Sequence: item.Sequence, FIFOPosition: item.FIFOPosition, ID: item.ID, AttemptID: item.AttemptID, IterationRound: item.IterationRound, Status: item.Status, CandidateSHA: item.CandidateSHA, ExpectedBestSHA: item.ExpectedBestSHA, CandidateExperimentID: item.CandidateExperimentID, IntentID: item.IntentID}
		for _, regression := range item.RegressionCases {
			integration.RegressionCases = append(integration.RegressionCases, v4Regression{CaseID: regression.CaseID, Kind: regression.Kind, Summary: regression.Summary})
		}
		result.Integrations = append(result.Integrations, integration)
	}
	for _, item := range value.BackOffs {
		result.BackOffs = append(result.BackOffs, v4BackOff{ID: item.ID, SourceWorkID: item.SourceWorkID, SuccessorBaselineRevisionID: item.SuccessorBaselineRevisionID, Message: item.Message})
	}
	if value.Iteration != nil {
		iteration := &v4IterationContext{HistoryLimit: value.Iteration.HistoryLimit, RequiredCaseSet: v4CaseSetFrom(value.Iteration.RequiredCaseSet), RecentTerminalAttempts: []v4AttemptHistory{}, EvidenceRoot: value.Iteration.EvidenceRoot, Experiments: value.Iteration.Experiments}
		for _, item := range value.Iteration.RecentTerminalAttempts {
			iteration.RecentTerminalAttempts = append(iteration.RecentTerminalAttempts, v4AttemptHistory{Attempt: v4AttemptFrom(item.Attempt), Messages: item.Messages, Summary: item.Summary})
		}
		if value.Iteration.PreviousRound != nil {
			iteration.PreviousRound = &v4RoundHistory{Work: v4WorkFrom(value.Iteration.PreviousRound.Work), Round: v4RoundFrom(value.Iteration.PreviousRound.Round), Messages: value.Iteration.PreviousRound.Messages}
		}
		result.Iteration = iteration
	}
	if value.SkillSnapshot == nil {
		return v4Document{}, errors.New("flow v2 Context has no skill snapshot provenance")
	}
	if len(value.SkillSnapshot.SnapshotID) != 64 {
		return v4Document{}, errors.New("flow v2 Context has an invalid skill snapshot_id")
	}
	if _, err := hex.DecodeString(value.SkillSnapshot.SnapshotID); err != nil {
		return v4Document{}, errors.New("flow v2 Context has an invalid skill snapshot_id")
	}
	entries, err := v4SkillEntries(value.SkillSnapshot.Entries)
	if err != nil {
		return v4Document{}, err
	}
	result.SkillSnapshot = &v4SkillSnapshotRef{SchemaVersion: value.SkillSnapshot.SchemaVersion, SnapshotID: value.SkillSnapshot.SnapshotID, Entries: entries, Manifest: value.SkillSnapshot.Manifest}
	return result, nil
}

func v4SkillEntries(entries []symphony.SkillSnapshotEntry) ([]v4SkillSnapshotEntry, error) {
	if len(entries) != 2 {
		return nil, errors.New("flow v2 Context requires exactly two skill provenance entries")
	}
	expected := []struct{ name, repository, branch, path string }{{"KernelWiki", symphony.KernelWikiRepository, symphony.KernelWikiBranch, "skills/KernelWiki"}, {"ncu-report-skill", symphony.NCUReportSkillRepository, symphony.NCUReportSkillBranch, "skills/ncu-report-skill"}}
	result := make([]v4SkillSnapshotEntry, len(entries))
	for index, entry := range entries {
		if entry.Name != expected[index].name || entry.Repository != expected[index].repository || entry.Branch != expected[index].branch || entry.RelativePath != expected[index].path {
			return nil, fmt.Errorf("invalid flow v2 skill provenance entry %d", index)
		}
		result[index] = v4SkillSnapshotEntry{Name: entry.Name, Repository: entry.Repository, Branch: entry.Branch, CommitSHA: entry.CommitSHA, Path: entry.RelativePath, ContentSHA256: entry.ContentSHA256}
	}
	return result, nil
}

type MessageRecord struct {
	SchemaVersion   int64                         `json:"schema_version"`
	Sequence        int64                         `json:"sequence"`
	Turn            symphony.ConversationTurnView `json:"turn"`
	Tools           []symphony.ToolEventView      `json:"tools"`
	ToolSupplements []symphony.ToolSupplementView `json:"tool_supplements"`
}

func Schemas() (contextSchema, messageSchema []byte, err error) {
	// Kept for callers that predate flow versioning. New activation uses the
	// explicit version-aware API below.
	return SchemasForVersion(LegacySchemaVersion)
}

func SchemasForVersion(version int64) (contextSchema, messageSchema []byte, err error) {
	contextName, messageName := "schemas/context-v4.schema.json", "schemas/message-v4.schema.json"
	if version == LegacySchemaVersion {
		contextName, messageName = "schemas/context.schema.json", "schemas/message.schema.json"
	} else if version != SchemaVersion && version != FlowV3SchemaVersion {
		return nil, nil, fmt.Errorf("unsupported Context Bundle schema version %d", version)
	}
	contextSchema, err = schemaFiles.ReadFile(contextName)
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded context schema: %w", err)
	}
	messageSchema, err = schemaFiles.ReadFile(messageName)
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded message schema: %w", err)
	}
	if version == FlowV3SchemaVersion {
		contextSchema, err = flowV3ContextSchema(contextSchema)
		if err != nil {
			return nil, nil, err
		}
		messageSchema, err = rewriteSchemaVersion(messageSchema, FlowV3SchemaVersion, "message-v5")
		if err != nil {
			return nil, nil, err
		}
	}
	return contextSchema, messageSchema, nil
}

func SummarySchema() ([]byte, error) {
	return SummarySchemaForVersion(LegacySchemaVersion)
}

func SummarySchemaForVersion(version int64) ([]byte, error) {
	if version != LegacySchemaVersion && version != SchemaVersion && version != FlowV3SchemaVersion {
		return nil, fmt.Errorf("unsupported Context Bundle schema version %d", version)
	}
	name := "schemas/summary.schema.json"
	if version == SchemaVersion || version == FlowV3SchemaVersion {
		name = "schemas/summary-v4.schema.json"
	}
	contents, err := schemaFiles.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read embedded summary schema: %w", err)
	}
	if version == FlowV3SchemaVersion {
		return rewriteSchemaVersion(contents, FlowV3SchemaVersion, "attempt-summary-v5")
	}
	return contents, nil
}

func rewriteSchemaVersion(contents []byte, version int64, idSuffix string) ([]byte, error) {
	var schema map[string]any
	if err := json.Unmarshal(contents, &schema); err != nil {
		return nil, fmt.Errorf("decode embedded schema: %w", err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, errors.New("embedded schema has no properties")
	}
	properties["schema_version"] = map[string]any{"const": version}
	schema["$id"] = "https://pika-go.local/schemas/" + idSuffix + ".json"
	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode schema version %d: %w", version, err)
	}
	return append(encoded, '\n'), nil
}

func flowV3ContextSchema(contents []byte) ([]byte, error) {
	var schema map[string]any
	if err := json.Unmarshal(contents, &schema); err != nil {
		return nil, fmt.Errorf("decode v4 Context schema: %w", err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, errors.New("v4 Context schema has no properties")
	}
	properties["schema_version"] = map[string]any{"const": FlowV3SchemaVersion}
	properties["allowed_terminal_operations"] = map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1}}
	properties["experiment_cycles"] = map[string]any{"type": "array", "items": experimentCycleSchema()}
	properties["reference_receipts"] = map[string]any{"type": "array", "items": referenceReceiptSchema()}
	properties["experiment_cycle"] = experimentCycleSchema()
	properties["reference_receipt"] = referenceReceiptSchema()
	properties["benchmark_context"] = strictSchema(map[string]any{"evidence_root": map[string]any{"type": "string", "minLength": 1}}, "evidence_root")
	definitions, ok := schema["$defs"].(map[string]any)
	if !ok {
		return nil, errors.New("v4 Context schema has no definitions")
	}
	for _, name := range []string{"optimization", "runtimeWork"} {
		definition, ok := definitions[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("v4 Context schema has no %s definition", name)
		}
		definitionProperties, ok := definition["properties"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("v4 Context %s definition has no properties", name)
		}
		definitionProperties["flow_version"] = map[string]any{"const": 3}
	}
	required, ok := schema["required"].([]any)
	if !ok {
		return nil, errors.New("v4 Context schema has no required fields")
	}
	schema["required"] = append(required, "allowed_terminal_operations", "experiment_cycles", "reference_receipts")
	schema["$id"] = "https://pika-go.local/schemas/context-v5.json"
	schema["title"] = "Pika Benchmark-Gated Agent Session Context"
	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode v5 Context schema: %w", err)
	}
	return append(encoded, '\n'), nil
}

func strictSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func experimentCycleSchema() map[string]any {
	return strictSchema(map[string]any{
		"id": map[string]any{"type": "string"}, "attempt_id": map[string]any{"type": "string"},
		"iteration_round": map[string]any{"type": "integer", "minimum": 1}, "sequence": map[string]any{"type": "integer", "minimum": 1},
		"checkpoint_sha": map[string]any{"type": "string"}, "baseline_definition_sha256": map[string]any{"type": "string"},
		"case_snapshot_sha256": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"},
		"benchmark_work_id": map[string]any{"type": "string"}, "iteration_work_id": map[string]any{"type": "string"},
		"reference_receipt_id": map[string]any{"type": "string"}, "failure_reason": map[string]any{"type": "string"},
	}, "id", "attempt_id", "iteration_round", "sequence", "checkpoint_sha", "baseline_definition_sha256", "case_snapshot_sha256", "status")
}

func referenceReceiptSchema() map[string]any {
	jsonValue := map[string]any{}
	return strictSchema(map[string]any{
		"id": map[string]any{"type": "string"}, "experiment_cycle_id": map[string]any{"type": "string"},
		"benchmark_run_id": map[string]any{"type": "string"}, "benchmark_work_id": map[string]any{"type": "string"},
		"checkpoint_sha": map[string]any{"type": "string"}, "baseline_definition_sha256": map[string]any{"type": "string"},
		"case_snapshot_sha256": map[string]any{"type": "string"}, "provider": map[string]any{"type": "string"},
		"model": map[string]any{"type": "string"}, "measurements": jsonValue, "environment": jsonValue,
		"consumed_experiment_id": map[string]any{"type": "string"},
	}, "id", "experiment_cycle_id", "benchmark_run_id", "benchmark_work_id", "checkpoint_sha", "baseline_definition_sha256", "case_snapshot_sha256", "measurements")
}

func (m Materializer) Materialize(ctx context.Context, session symphony.AgentSession) (Bundle, error) {
	if m.Store == nil || m.Root == "" {
		return Bundle{}, errors.New("Context Bundle store and root are required")
	}
	if session.ID == "" || filepath.Base(session.ID) != session.ID || strings.ContainsAny(session.ID, `/\\`) {
		return Bundle{}, errors.New("safe Agent Session ID is required")
	}
	if stored, found, err := m.Store.ReadContextSnapshot(ctx, session.ID); err != nil {
		return Bundle{}, err
	} else if found {
		return m.verifyStored(stored)
	}

	projection, err := m.Store.ContextProjection(ctx, session.ID)
	if err != nil {
		return Bundle{}, err
	}
	if projection.Session.ID != session.ID || projection.Session.WorkID != session.WorkID || projection.Session.Generation != session.Generation {
		return Bundle{}, errors.New("Context Projection does not match the requested Agent Session")
	}
	schemaVersion := LegacySchemaVersion
	if projection.TargetWork.FlowVersion == symphony.FlowVersion2 {
		schemaVersion = SchemaVersion
	} else if projection.TargetWork.FlowVersion == symphony.FlowVersion3 {
		schemaVersion = FlowV3SchemaVersion
	}
	records := recordsFor(schemaVersion, projection.Journal)
	messages, err := encodeJSONL(records)
	if err != nil {
		return Bundle{}, err
	}
	contextRelative := filepath.Join(session.ID, "context.json")
	messagesRelative := filepath.Join(session.ID, "messages.jsonl")
	messagesPath := filepath.Join(m.Root, messagesRelative)
	messagesDigest := digest(messages)
	document := Document{
		SchemaVersion: schemaVersion, Session: projection.Session, Optimization: projection.View.Optimization,
		Baseline: projection.View.Baseline, Best: projection.View.Best, IterationCaseSet: projection.View.IterationCaseSet,
		Attempts: nonNil(projection.View.Attempts), IterationRounds: nonNil(projection.View.IterationRounds),
		Integrations: nonNil(projection.View.Integrations), BackOffs: nonNil(projection.View.BackOffs),
		Work: projection.TargetWork, GeneratorWork: projection.GeneratorWork,
		TerminalOperation: terminalOperation(projection.Session.Role),
		Messages:          FileReference{Path: messagesPath, SHA256: messagesDigest, Bytes: int64(len(messages)), Records: int64(len(records))},
	}
	if schemaVersion == FlowV3SchemaVersion {
		document.AllowedTerminals = []string{document.TerminalOperation}
		if projection.Session.Role == symphony.RoleIteration {
			document.AllowedTerminals = append(document.AllowedTerminals, "start_next_experiment")
		}
		document.ExperimentCycles = append([]symphony.ExperimentCycleView{}, projection.View.ExperimentCycles...)
		document.ReferenceReceipts = append([]symphony.ReferenceReceiptView{}, projection.View.ReferenceReceipts...)
		document.ExperimentCycle = projection.TargetWork.ExperimentCycle
		document.ReferenceReceipt = projection.TargetWork.ReferenceReceipt
	}
	files := map[string][]byte{messagesRelative: messages}
	if projection.TargetWork.Work.Role == symphony.RoleIteration {
		if projection.TargetWork.IterationCaseSet == nil {
			return Bundle{}, errors.New("Iteration Work has no frozen Iteration Case Snapshot")
		}
		document.Iteration = &IterationContext{HistoryLimit: projection.TargetWork.IterationHistoryLimit,
			RequiredCaseSet: *projection.TargetWork.IterationCaseSet, RecentTerminalAttempts: []AttemptHistory{}}
		if projection.PreviousRound != nil {
			previousRecords := recordsFor(schemaVersion, projection.PreviousRound.Journal)
			previousMessages, err := encodeJSONL(previousRecords)
			if err != nil {
				return Bundle{}, err
			}
			previousRelative := filepath.Join(session.ID, "previous-round", fmt.Sprintf("round-%d", projection.PreviousRound.Round.Round), "messages.jsonl")
			files[previousRelative] = previousMessages
			document.Iteration.PreviousRound = &RoundHistory{Work: projection.PreviousRound.Work, Round: projection.PreviousRound.Round,
				Messages: FileReference{Path: filepath.Join(m.Root, previousRelative), SHA256: digest(previousMessages), Bytes: int64(len(previousMessages)), Records: int64(len(previousRecords))}}
		}
		for _, history := range projection.AttemptHistories {
			historyRecords := recordsFor(schemaVersion, history.Journal)
			historyMessages, err := encodeJSONL(historyRecords)
			if err != nil {
				return Bundle{}, err
			}
			summary, err := encodeSummaryJSONL(SummaryRecord{SchemaVersion: schemaVersion, Attempt: history.Attempt})
			if err != nil {
				return Bundle{}, err
			}
			root := filepath.Join(session.ID, "attempt-history", history.Attempt.ID)
			historyMessagesRelative := filepath.Join(root, "messages.jsonl")
			summaryRelative := filepath.Join(root, "summary.jsonl")
			files[historyMessagesRelative], files[summaryRelative] = historyMessages, summary
			document.Iteration.RecentTerminalAttempts = append(document.Iteration.RecentTerminalAttempts, AttemptHistory{
				Attempt:  history.Attempt,
				Messages: FileReference{Path: filepath.Join(m.Root, historyMessagesRelative), SHA256: digest(historyMessages), Bytes: int64(len(historyMessages)), Records: int64(len(historyRecords))},
				Summary:  FileReference{Path: filepath.Join(m.Root, summaryRelative), SHA256: digest(summary), Bytes: int64(len(summary)), Records: 1},
			})
		}
	}
	if schemaVersion == SchemaVersion || schemaVersion == FlowV3SchemaVersion {
		if m.EvidenceRoot == "" || !filepath.IsAbs(m.EvidenceRoot) {
			return Bundle{}, errors.New("absolute evidence root is required for flow v2+ Context")
		}
		if err := m.addV4Artifacts(&document, projection, session, files); err != nil {
			return Bundle{}, err
		}
	}
	var contextBytes []byte
	if schemaVersion == FlowV3SchemaVersion {
		v5, err := v5DocumentFrom(document)
		if err != nil {
			return Bundle{}, err
		}
		contextBytes, err = json.MarshalIndent(v5, "", "  ")
	} else if schemaVersion == SchemaVersion {
		v4, err := v4DocumentFrom(document)
		if err != nil {
			return Bundle{}, err
		}
		contextBytes, err = json.MarshalIndent(v4, "", "  ")
	} else {
		contextBytes, err = json.MarshalIndent(v3DocumentFrom(document), "", "  ")
	}
	if err != nil {
		return Bundle{}, fmt.Errorf("encode context.json: %w", err)
	}
	contextBytes = append(contextBytes, '\n')
	contextDigest := digest(contextBytes)
	files[contextRelative] = contextBytes

	if err := m.writeAtomically(session.ID, files); err != nil {
		return Bundle{}, err
	}
	candidate := symphony.ContextSnapshot{
		AgentSessionID: session.ID, SchemaVersion: schemaVersion,
		ContextRelativePath: contextRelative, ContextSHA256: contextDigest, ContextBytes: int64(len(contextBytes)),
		MessagesRelativePath: messagesRelative, MessagesSHA256: messagesDigest, MessagesBytes: int64(len(messages)),
		MessageRecords: int64(len(records)),
	}
	stored, err := m.Store.FreezeContextSnapshot(ctx, candidate)
	if err != nil {
		_ = os.RemoveAll(filepath.Join(m.Root, session.ID))
		return Bundle{}, err
	}
	return m.verifyStored(stored)
}

func (m Materializer) addV4Artifacts(document *Document, projection symphony.ContextProjection, session symphony.AgentSession, files map[string][]byte) error {
	work := projection.TargetWork
	add := func(relative string, contents []byte, records int64) FileReference {
		files[relative] = contents
		return FileReference{Path: filepath.Join(m.Root, relative), SHA256: digest(contents), Bytes: int64(len(contents)), Records: records}
	}
	addBlob := func(relative string, contents []byte) BlobReference {
		files[relative] = contents
		return BlobReference{Path: filepath.Join(m.Root, relative), SHA256: digest(contents), Bytes: int64(len(contents))}
	}
	if work.SkillSnapshot != nil {
		input, err := skillsnapshot.InputFromView(*work.SkillSnapshot)
		if err != nil {
			return fmt.Errorf("load frozen skill manifest for Context: %w", err)
		}
		if _, err := v4SkillEntries(work.SkillSnapshot.Entries); err != nil {
			return err
		}
		relative := filepath.Join(session.ID, "artifacts", "skill-snapshot.json")
		reference := addBlob(relative, append([]byte{}, input.Manifest...))
		document.SkillSnapshot = &SkillSnapshotReference{
			SchemaVersion: work.SkillSnapshot.SchemaVersion, SnapshotID: work.SkillSnapshot.SnapshotID,
			Entries: append([]symphony.SkillSnapshotEntry{}, work.SkillSnapshot.Entries...), Manifest: reference,
		}
	}
	if work.Diagnosis != nil {
		diagnosis := &DiagnosisReference{
			ID: work.Diagnosis.ID, BaselineID: work.Diagnosis.BaselineRevisionID,
			WorkID: work.Diagnosis.WorkID, Status: work.Diagnosis.Status,
			Hypotheses: work.Diagnosis.HypothesisCount,
		}
		if len(work.Diagnosis.Report) != 0 {
			hypotheses, err := canonicalDiagnosisHypothesisCount(work.Diagnosis.Report)
			if err != nil {
				return fmt.Errorf("validate canonical Diagnosis artifact for Context: %w", err)
			}
			if work.Diagnosis.HypothesisCount != hypotheses {
				return fmt.Errorf("runtime Diagnosis hypothesis_count %d differs from canonical artifact count %d", work.Diagnosis.HypothesisCount, hypotheses)
			}
			diagnosis.Hypotheses = hypotheses
			relative := filepath.Join(session.ID, "artifacts", "diagnosis.json")
			diagnosis.Report = ptrBlobReference(addBlob(relative, append(append([]byte{}, work.Diagnosis.Report...), '\n')))
		}
		if work.Work.Role == symphony.RoleDiagnosis {
			root, err := evidence.EnsureWorkRoot(m.EvidenceRoot, evidence.DiagnosisScope, work.Work.ID)
			if err != nil {
				return fmt.Errorf("prepare Diagnosis evidence root: %w", err)
			}
			diagnosis.EvidenceRoot = root
		}
		document.Diagnosis = diagnosis
	}
	if work.Work.Role == symphony.RoleIteration {
		root, err := evidence.EnsureWorkRoot(m.EvidenceRoot, evidence.IterationScope, work.Work.ID)
		if err != nil {
			return fmt.Errorf("prepare Iteration evidence root: %w", err)
		}
		document.Iteration.EvidenceRoot = root
		experiments, err := encodeExperimentsJSONL(work.IterationExperiments)
		if err != nil {
			return err
		}
		relative := filepath.Join(session.ID, "artifacts", "experiments.jsonl")
		reference := add(relative, experiments, int64(len(work.IterationExperiments)))
		if document.Iteration == nil {
			return errors.New("flow v2 Iteration has no Iteration Context")
		}
		document.Iteration.Experiments = &reference
	}
	if work.Work.Role == symphony.RoleBenchmark {
		root, err := evidence.EnsureWorkRoot(m.EvidenceRoot, evidence.BenchmarkScope, work.Work.ID)
		if err != nil {
			return fmt.Errorf("prepare Benchmark evidence root: %w", err)
		}
		document.Benchmark = &BenchmarkContext{EvidenceRoot: root}
	}
	if work.Work.Role == symphony.RoleIntegration {
		var candidateExperimentID string
		for _, integration := range projection.View.Integrations {
			if integration.ID == work.Work.IntegrationID {
				candidateExperimentID = integration.CandidateExperimentID
				break
			}
		}
		if candidateExperimentID == "" {
			return errors.New("flow v2 Integration has no candidate_experiment_id")
		}
		experiments, err := encodeExperimentsJSONL(work.IterationExperiments)
		if err != nil {
			return err
		}
		relative := filepath.Join(session.ID, "artifacts", "experiments.jsonl")
		document.Integration = &IntegrationContext{
			CandidateExperimentID: candidateExperimentID,
			Experiments:           add(relative, experiments, int64(len(work.IterationExperiments))),
		}
	}
	knowledge, records, err := encodeKnowledge(work, projection.View, projection.KnowledgeExperiments, projection.KnowledgeArtifacts)
	if err != nil {
		return err
	}
	knowledgeRelative := filepath.Join(session.ID, "artifacts", "knowledge.jsonl")
	knowledgeReference := add(knowledgeRelative, knowledge, int64(records))
	document.Knowledge = &knowledgeReference
	return nil
}

func canonicalDiagnosisHypothesisCount(raw json.RawMessage) (int64, error) {
	report, err := kdacontract.ParseDiagnosisReport(raw)
	if err != nil {
		return 0, err
	}
	return int64(len(report.Hypotheses)), nil
}

// artifactFirstRuntimeWork returns the v4 current-facts projection.  The
// complete skill manifest, Diagnosis report, and Experiment ledger are each
// emitted exactly once as verified artifacts and referenced from context.json.
// V3 intentionally continues to serialize RuntimeWork unchanged.

func ptrBlobReference(value BlobReference) *BlobReference { return &value }

func encodeExperimentsJSONL(experiments []symphony.IterationExperimentView) ([]byte, error) {
	type experimentEnvelope struct {
		SchemaVersion       int64           `json:"schema_version"`
		ExperimentID        string          `json:"experiment_id"`
		AttemptID           string          `json:"attempt_id"`
		IterationRound      int64           `json:"iteration_round"`
		Sequence            int64           `json:"sequence"`
		Outcome             string          `json:"outcome"`
		ParentCheckpointSHA string          `json:"parent_checkpoint_sha"`
		CheckpointSHA       string          `json:"checkpoint_sha,omitempty"`
		ScopeBestSHA        string          `json:"scope_best_sha"`
		ReceiptID           string          `json:"receipt_id"`
		RawInput            json.RawMessage `json:"raw_input"`
		DerivedMetrics      json.RawMessage `json:"derived_metrics,omitempty"`
		ArtifactIDs         []string        `json:"artifact_ids,omitempty"`
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, experiment := range experiments {
		if !json.Valid(experiment.Experiment) || experiment.ReceiptID == "" || experiment.ScopeBestSHA == "" {
			return nil, errors.New("stored Iteration Experiment is invalid JSON")
		}
		envelope := experimentEnvelope{
			SchemaVersion:       1,
			ExperimentID:        experiment.ID,
			AttemptID:           experiment.AttemptID,
			IterationRound:      experiment.IterationRound,
			Sequence:            experiment.Sequence,
			Outcome:             experiment.Outcome,
			ParentCheckpointSHA: experiment.ParentCheckpointSHA,
			CheckpointSHA:       experiment.CheckpointSHA,
			ScopeBestSHA:        experiment.ScopeBestSHA,
			ReceiptID:           experiment.ReceiptID,
			RawInput:            append(json.RawMessage{}, experiment.Experiment...),
			ArtifactIDs:         append([]string{}, experiment.ArtifactIDs...),
		}
		derived, err := json.Marshal(map[string]any{"comparisons": experiment.DerivedComparisons})
		if err != nil {
			return nil, err
		}
		envelope.DerivedMetrics = derived
		if err := encoder.Encode(envelope); err != nil {
			return nil, err
		}
	}
	return output.Bytes(), nil
}

func encodeKnowledge(work symphony.RuntimeWork, view symphony.View, experiments []symphony.IterationExperimentView, artifacts []symphony.EvidenceArtifact) ([]byte, int, error) {
	records, err := symphony.BuildKnowledgeProjection(work, view, experiments, artifacts)
	if err != nil {
		return nil, 0, errors.New("stored knowledge projection source is invalid JSON")
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return nil, 0, err
		}
	}
	return output.Bytes(), len(records), nil
}

func recordsFor(schemaVersion int64, journal symphony.ConversationJournalView) []MessageRecord {
	tools := make(map[string][]symphony.ToolEventView)
	for _, tool := range journal.Tools {
		tools[tool.ConversationTurnID] = append(tools[tool.ConversationTurnID], tool)
	}
	supplements := make(map[string][]symphony.ToolSupplementView)
	for _, supplement := range journal.ToolSupplements {
		supplements[supplement.ConversationTurnID] = append(supplements[supplement.ConversationTurnID], supplement)
	}
	records := make([]MessageRecord, 0, len(journal.Turns))
	for index, turn := range journal.Turns {
		records = append(records, MessageRecord{SchemaVersion: schemaVersion, Sequence: int64(index + 1), Turn: turn,
			Tools: nonNil(tools[turn.ID]), ToolSupplements: nonNil(supplements[turn.ID])})
	}
	return records
}

func encodeJSONL(records []MessageRecord) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return nil, fmt.Errorf("encode messages.jsonl: %w", err)
		}
	}
	return output.Bytes(), nil
}

func encodeSummaryJSONL(record SummaryRecord) ([]byte, error) {
	contents, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode summary.jsonl: %w", err)
	}
	return append(contents, '\n'), nil
}

func (m Materializer) writeAtomically(sessionID string, files map[string][]byte) error {
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		return fmt.Errorf("create Context Bundle root: %w", err)
	}
	if err := os.Chmod(m.Root, 0o700); err != nil {
		return fmt.Errorf("protect Context Bundle root: %w", err)
	}
	finalRoot := filepath.Join(m.Root, sessionID)
	if _, err := os.Stat(finalRoot); err == nil {
		if err := os.RemoveAll(finalRoot); err != nil {
			return fmt.Errorf("remove incomplete Context Bundle: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Context Bundle destination: %w", err)
	}
	temporary, err := os.MkdirTemp(m.Root, ".context-"+sessionID+"-")
	if err != nil {
		return fmt.Errorf("create temporary Context Bundle: %w", err)
	}
	defer os.RemoveAll(temporary)
	for relative, contents := range files {
		trimmed := strings.TrimPrefix(relative, sessionID+string(filepath.Separator))
		if trimmed == relative || trimmed == "" || strings.HasPrefix(trimmed, "..") {
			return errors.New("Context Bundle contains an unsafe relative path")
		}
		path := filepath.Join(temporary, trimmed)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create Context Bundle directory: %w", err)
		}
		if err := writeFile(path, contents); err != nil {
			return err
		}
	}
	if err := os.Chmod(temporary, 0o700); err != nil {
		return fmt.Errorf("protect Context Bundle directory: %w", err)
	}
	if err := os.Rename(temporary, finalRoot); err != nil {
		return fmt.Errorf("publish Context Bundle: %w", err)
	}
	return nil
}

func writeFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("create Context Bundle file %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write Context Bundle file %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync Context Bundle file %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Context Bundle file %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (m Materializer) verifyStored(snapshot symphony.ContextSnapshot) (Bundle, error) {
	if (snapshot.SchemaVersion < 2 || snapshot.SchemaVersion > FlowV3SchemaVersion) || filepath.Clean(snapshot.ContextRelativePath) != filepath.Join(snapshot.AgentSessionID, "context.json") ||
		filepath.Clean(snapshot.MessagesRelativePath) != filepath.Join(snapshot.AgentSessionID, "messages.jsonl") {
		return Bundle{}, errors.New("stored Context Snapshot has an unsupported contract")
	}
	contextPath := filepath.Join(m.Root, snapshot.ContextRelativePath)
	messagesPath := filepath.Join(m.Root, snapshot.MessagesRelativePath)
	contextDigest, contextBytes, err := digestPath(contextPath)
	if err != nil {
		return Bundle{}, err
	}
	messagesDigest, messagesBytes, err := digestPath(messagesPath)
	if err != nil {
		return Bundle{}, err
	}
	if contextDigest != snapshot.ContextSHA256 || contextBytes != snapshot.ContextBytes ||
		messagesDigest != snapshot.MessagesSHA256 || messagesBytes != snapshot.MessagesBytes {
		return Bundle{}, errors.New("frozen Context Bundle digest does not match its files")
	}
	contents, err := os.ReadFile(contextPath)
	if err != nil {
		return Bundle{}, err
	}
	var document Document
	if err := json.Unmarshal(contents, &document); err != nil {
		return Bundle{}, errors.New("frozen context.json is invalid")
	}
	if document.Iteration != nil {
		if document.Iteration.PreviousRound != nil {
			reference := document.Iteration.PreviousRound.Messages
			actualDigest, actualBytes, err := digestPath(reference.Path)
			if err != nil || actualDigest != reference.SHA256 || actualBytes != reference.Bytes {
				return Bundle{}, errors.New("frozen previous Round history digest does not match its file")
			}
		}
		for _, history := range document.Iteration.RecentTerminalAttempts {
			for _, reference := range []FileReference{history.Messages, history.Summary} {
				actualDigest, actualBytes, err := digestPath(reference.Path)
				if err != nil || actualDigest != reference.SHA256 || actualBytes != reference.Bytes {
					return Bundle{}, errors.New("frozen Attempt history digest does not match its file")
				}
			}
		}
	}
	if snapshot.SchemaVersion == SchemaVersion || snapshot.SchemaVersion == FlowV3SchemaVersion {
		if document.SkillSnapshot != nil {
			if err := verifyBlobReference(document.SkillSnapshot.Manifest); err != nil {
				return Bundle{}, fmt.Errorf("frozen skill manifest digest does not match its file: %w", err)
			}
		}
		if document.Diagnosis != nil && document.Diagnosis.Report != nil {
			if err := verifyBlobReference(*document.Diagnosis.Report); err != nil {
				return Bundle{}, fmt.Errorf("frozen Diagnosis report digest does not match its file: %w", err)
			}
		}
		if document.Knowledge == nil {
			return Bundle{}, errors.New("flow v2 Context Bundle has no knowledge projection")
		}
		if err := verifyFileReference(*document.Knowledge); err != nil {
			return Bundle{}, fmt.Errorf("frozen knowledge digest does not match its file: %w", err)
		}
		if document.Iteration != nil && document.Iteration.Experiments != nil {
			if err := verifyFileReference(*document.Iteration.Experiments); err != nil {
				return Bundle{}, fmt.Errorf("frozen Experiment ledger digest does not match its file: %w", err)
			}
		}
		if document.Integration != nil {
			if err := verifyFileReference(document.Integration.Experiments); err != nil {
				return Bundle{}, fmt.Errorf("frozen Integration Experiment ledger digest does not match its file: %w", err)
			}
		}
	}
	return Bundle{SchemaVersion: snapshot.SchemaVersion, ContextPath: contextPath, ContextSHA256: contextDigest, MessagesPath: messagesPath,
		MessagesSHA256: messagesDigest, MessageRecords: snapshot.MessageRecords}, nil
}

func verifyFileReference(reference FileReference) error {
	return verifyBlobReference(BlobReference{Path: reference.Path, SHA256: reference.SHA256, Bytes: reference.Bytes})
}

func verifyBlobReference(reference BlobReference) error {
	digestValue, bytesValue, err := digestPath(reference.Path)
	if err != nil {
		return err
	}
	if digestValue != reference.SHA256 || bytesValue != reference.Bytes {
		return errors.New("digest or byte size differs")
	}
	return nil
}

func digest(contents []byte) string {
	value := sha256.Sum256(contents)
	return hex.EncodeToString(value[:])
}

func digestPath(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open frozen Context Bundle file %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, fmt.Errorf("hash frozen Context Bundle file %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func terminalOperation(role symphony.WorkRole) string {
	if descriptor, ok := symphony.DescribeRole(role); ok {
		return descriptor.TerminalOperation
	}
	return ""
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
