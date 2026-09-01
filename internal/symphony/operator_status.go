package symphony

// OperatorStatus is the deliberately bounded status wire model. Detailed
// reports, benchmark payloads, provider payloads, and evidence remain available
// only through SQLite, artifacts, and the workbench.
type OperatorStatus struct {
	Optimization         OperatorOptimization          `json:"optimization"`
	Scheduler            OperatorScheduler             `json:"scheduler"`
	Baseline             *OperatorBaseline             `json:"baseline,omitempty"`
	Baselines            []OperatorBaseline            `json:"baselines"`
	Works                []WorkView                    `json:"works"`
	AgentSessions        []OperatorAgentSession        `json:"agent_sessions,omitempty"`
	BackOffs             []OperatorBackOff             `json:"back_offs"`
	Attempts             []OperatorAttempt             `json:"attempts"`
	Integrations         []OperatorIntegration         `json:"integrations"`
	IterationRounds      []OperatorIterationRound      `json:"iteration_rounds"`
	Best                 *OperatorBest                 `json:"best,omitempty"`
	Bests                []OperatorBest                `json:"bests,omitempty"`
	IterationCaseSet     *IterationCaseSetView         `json:"iteration_case_set,omitempty"`
	SkillSnapshot        *OperatorSkillSnapshot        `json:"skill_snapshot,omitempty"`
	Diagnoses            []OperatorDiagnosis           `json:"diagnoses,omitempty"`
	IterationExperiments []OperatorIterationExperiment `json:"iteration_experiments,omitempty"`
	Knowledge            KnowledgeSummary              `json:"knowledge"`
	FollowUps            []OperatorFollowUp            `json:"follow_ups,omitempty"`
	DomainEventCount     int64                         `json:"domain_event_count"`
	PendingEffectCount   int64                         `json:"pending_effect_count"`
	Storage              StorageView                   `json:"storage"`
}

type OperatorBaseline struct {
	ID                         string         `json:"id"`
	Number                     int64          `json:"number"`
	Status                     BaselineStatus `json:"status"`
	RepositorySHA              string         `json:"repository_sha,omitempty"`
	PredecessorID              string         `json:"predecessor_id,omitempty"`
	FailureKind                string         `json:"failure_kind,omitempty"`
	MeasurementContractVersion int64          `json:"measurement_contract_version,omitempty"`
}

type OperatorOptimization struct {
	ID                    string             `json:"id"`
	Status                OptimizationStatus `json:"status"`
	Revision              int64              `json:"revision"`
	IterationConcurrency  int64              `json:"iteration_concurrency"`
	MaxPendingAttempts    int64              `json:"max_pending_attempts"`
	IterationHistoryLimit int64              `json:"iteration_history_limit"`
	FlowVersion           FlowVersion        `json:"flow_version"`
}

type OperatorScheduler struct {
	Status   SchedulerStatus `json:"status"`
	PausedAt string          `json:"paused_at,omitempty"`
	Epoch    int64           `json:"epoch"`
}

type OperatorAgentSession struct {
	ID              string             `json:"id"`
	WorkID          string             `json:"work_id"`
	Generation      int64              `json:"generation"`
	Role            WorkRole           `json:"role"`
	AgentKind       string             `json:"agent_kind"`
	AgentName       string             `json:"agent_name"`
	ProviderVersion string             `json:"provider_version,omitempty"`
	Status          AgentSessionStatus `json:"status"`
}

type OperatorBackOff struct {
	ID                          string `json:"id"`
	SourceWorkID                string `json:"source_work_id"`
	SuccessorBaselineRevisionID string `json:"successor_baseline_revision_id"`
}

type OperatorAttempt struct {
	ID                    string `json:"id"`
	SlotIndex             int64  `json:"slot_index"`
	Status                string `json:"status"`
	BaseBestSequence      int64  `json:"base_best_sequence"`
	BaseSHA               string `json:"base_sha"`
	CurrentIterationRound int64  `json:"current_iteration_round"`
	CandidateSHA          string `json:"candidate_sha,omitempty"`
	HistoryLimit          int64  `json:"history_limit"`
}

type OperatorIntegration struct {
	Sequence              int64  `json:"sequence"`
	FIFOPosition          int64  `json:"fifo_position"`
	ID                    string `json:"id"`
	AttemptID             string `json:"attempt_id"`
	IterationRound        int64  `json:"iteration_round"`
	Status                string `json:"status"`
	CandidateSHA          string `json:"candidate_sha"`
	ExpectedBestSHA       string `json:"expected_best_sha"`
	CandidateExperimentID string `json:"candidate_experiment_id,omitempty"`
	IntentID              string `json:"intent_id,omitempty"`
	RegressionCaseCount   int64  `json:"regression_case_count"`
}

type OperatorIterationRound struct {
	AttemptID               string `json:"attempt_id"`
	Round                   int64  `json:"round"`
	Kind                    string `json:"kind"`
	BaseSHA                 string `json:"base_sha"`
	Status                  string `json:"status"`
	IterationCaseSetVersion int64  `json:"iteration_case_set_version"`
	CurrentCheckpointSHA    string `json:"current_checkpoint_sha,omitempty"`
}

type OperatorBest struct {
	ID              string `json:"id"`
	Sequence        int64  `json:"sequence"`
	CommitSHA       string `json:"commit_sha"`
	SourceAttemptID string `json:"source_attempt_id,omitempty"`
}

type OperatorSkillSnapshot struct {
	SchemaVersion int64                        `json:"schema_version"`
	SnapshotID    string                       `json:"snapshot_id"`
	Entries       []OperatorSkillSnapshotEntry `json:"entries"`
}

type OperatorSkillSnapshotEntry struct {
	Name      string `json:"name"`
	CommitSHA string `json:"commit_sha"`
}

type OperatorDiagnosis struct {
	ID                 string          `json:"id"`
	BaselineRevisionID string          `json:"baseline_revision_id"`
	WorkID             string          `json:"work_id"`
	Status             DiagnosisStatus `json:"status"`
	HypothesisCount    int64           `json:"hypothesis_count"`
}

type OperatorIterationExperiment struct {
	ID                  string `json:"id"`
	AttemptID           string `json:"attempt_id"`
	IterationRound      int64  `json:"iteration_round"`
	Sequence            int64  `json:"sequence"`
	Outcome             string `json:"outcome"`
	ParentCheckpointSHA string `json:"parent_checkpoint_sha"`
	CheckpointSHA       string `json:"checkpoint_sha,omitempty"`
	ScopeBestSHA        string `json:"scope_best_sha"`
	ReceiptID           string `json:"receipt_id"`
	MeasurementCount    int64  `json:"measurement_count"`
	ArtifactCount       int64  `json:"artifact_count"`
}

type OperatorFollowUp struct {
	ID                   string   `json:"id"`
	TargetWorkID         string   `json:"target_work_id"`
	TargetAgentSessionID string   `json:"target_agent_session_id"`
	TargetRole           WorkRole `json:"target_role"`
	RequestSequence      int64    `json:"request_sequence"`
	Status               string   `json:"status"`
	GeneratorWorkID      string   `json:"generator_work_id,omitempty"`
	DeliveryID           string   `json:"delivery_id,omitempty"`
}

func ProjectOperatorStatus(view View) OperatorStatus {
	status := OperatorStatus{
		Optimization: OperatorOptimization{
			ID: view.Optimization.ID, Status: view.Optimization.Status, Revision: view.Optimization.Revision,
			IterationConcurrency: view.Optimization.IterationConcurrency, MaxPendingAttempts: view.Optimization.MaxPendingAttempts,
			IterationHistoryLimit: view.Optimization.IterationHistoryLimit, FlowVersion: view.Optimization.FlowVersion,
		},
		Scheduler: OperatorScheduler{Status: view.Scheduler.Status, PausedAt: view.Scheduler.PausedAt, Epoch: view.Scheduler.Epoch},
		Works:     append([]WorkView{}, view.Works...), IterationCaseSet: view.IterationCaseSet,
		Knowledge:        view.Knowledge,
		DomainEventCount: view.DomainEventCount, PendingEffectCount: view.PendingEffectCount, Storage: view.Storage,
	}
	for _, backOff := range view.BackOffs {
		status.BackOffs = append(status.BackOffs, OperatorBackOff{
			ID: backOff.ID, SourceWorkID: backOff.SourceWorkID, SuccessorBaselineRevisionID: backOff.SuccessorBaselineRevisionID,
		})
	}
	for _, attempt := range view.Attempts {
		status.Attempts = append(status.Attempts, OperatorAttempt{
			ID: attempt.ID, SlotIndex: attempt.SlotIndex, Status: attempt.Status, BaseBestSequence: attempt.BaseBestSequence,
			BaseSHA: attempt.BaseSHA, CurrentIterationRound: attempt.CurrentIterationRound,
			CandidateSHA: attempt.CandidateSHA, HistoryLimit: attempt.HistoryLimit,
		})
	}
	for _, followUp := range view.FollowUps {
		status.FollowUps = append(status.FollowUps, OperatorFollowUp{
			ID: followUp.ID, TargetWorkID: followUp.TargetWorkID, TargetAgentSessionID: followUp.TargetAgentSessionID,
			TargetRole: followUp.TargetRole, RequestSequence: followUp.RequestSequence, Status: followUp.Status,
			GeneratorWorkID: followUp.GeneratorWorkID, DeliveryID: followUp.DeliveryID,
		})
	}
	for _, baseline := range view.Baselines {
		status.Baselines = append(status.Baselines, operatorBaseline(baseline))
	}
	if view.Baseline != nil {
		value := operatorBaseline(*view.Baseline)
		status.Baseline = &value
	}
	for _, session := range view.AgentSessions {
		status.AgentSessions = append(status.AgentSessions, OperatorAgentSession{
			ID: session.ID, WorkID: session.WorkID, Generation: session.Generation, Role: session.Role,
			AgentKind: session.AgentKind, AgentName: session.AgentName, ProviderVersion: session.ProviderVersion, Status: session.Status,
		})
	}
	for _, integration := range view.Integrations {
		status.Integrations = append(status.Integrations, OperatorIntegration{
			Sequence: integration.Sequence, FIFOPosition: integration.FIFOPosition, ID: integration.ID,
			AttemptID: integration.AttemptID, IterationRound: integration.IterationRound, Status: integration.Status,
			CandidateSHA: integration.CandidateSHA, ExpectedBestSHA: integration.ExpectedBestSHA,
			CandidateExperimentID: integration.CandidateExperimentID, IntentID: integration.IntentID,
			RegressionCaseCount: int64(len(integration.RegressionCases)),
		})
	}
	for _, round := range view.IterationRounds {
		status.IterationRounds = append(status.IterationRounds, OperatorIterationRound{
			AttemptID: round.AttemptID, Round: round.Round, Kind: round.Kind, BaseSHA: round.BaseSHA,
			Status: round.Status, IterationCaseSetVersion: round.IterationCaseSetVersion, CurrentCheckpointSHA: round.CurrentCheckpointSHA,
		})
	}
	for _, best := range view.Bests {
		status.Bests = append(status.Bests, operatorBest(best))
	}
	if view.Best != nil {
		value := operatorBest(*view.Best)
		status.Best = &value
	}
	if view.SkillSnapshot != nil {
		status.SkillSnapshot = &OperatorSkillSnapshot{SchemaVersion: view.SkillSnapshot.SchemaVersion, SnapshotID: view.SkillSnapshot.SnapshotID}
		for _, entry := range view.SkillSnapshot.Entries {
			status.SkillSnapshot.Entries = append(status.SkillSnapshot.Entries, OperatorSkillSnapshotEntry{Name: entry.Name, CommitSHA: entry.CommitSHA})
		}
	}
	for _, diagnosis := range view.Diagnoses {
		status.Diagnoses = append(status.Diagnoses, OperatorDiagnosis{
			ID: diagnosis.ID, BaselineRevisionID: diagnosis.BaselineRevisionID, WorkID: diagnosis.WorkID,
			Status: diagnosis.Status, HypothesisCount: diagnosis.HypothesisCount,
		})
	}
	for _, experiment := range view.IterationExperiments {
		status.IterationExperiments = append(status.IterationExperiments, OperatorIterationExperiment{
			ID: experiment.ID, AttemptID: experiment.AttemptID, IterationRound: experiment.IterationRound,
			Sequence: experiment.Sequence, Outcome: experiment.Outcome, ParentCheckpointSHA: experiment.ParentCheckpointSHA,
			CheckpointSHA: experiment.CheckpointSHA, ScopeBestSHA: experiment.ScopeBestSHA, ReceiptID: experiment.ReceiptID,
			MeasurementCount: int64(len(experiment.DerivedComparisons)), ArtifactCount: int64(len(experiment.ArtifactIDs)),
		})
	}
	return status
}

func operatorBaseline(value BaselineView) OperatorBaseline {
	return OperatorBaseline{
		ID: value.ID, Number: value.Number, Status: value.Status, RepositorySHA: value.RepositorySHA,
		PredecessorID: value.PredecessorID, FailureKind: value.FailureKind,
		MeasurementContractVersion: value.MeasurementContractVersion,
	}
}

func operatorBest(value BestView) OperatorBest {
	return OperatorBest{ID: value.ID, Sequence: value.Sequence, CommitSHA: value.CommitSHA, SourceAttemptID: value.SourceAttemptID}
}
