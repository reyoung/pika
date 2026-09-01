package symphony

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/reyoung/pika-go/internal/kdacontract"
)

// createDiagnosisWork is called in the same transaction that accepts a
// Baseline and creates Best revision zero.  There is no scheduler-side race:
// the unique baseline/work keys make the baseline-to-diagnosis edge durable.
func (e *Engine) createDiagnosisWork(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, now string) error {
	diagnosisID, workID := e.newID(), e.newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
		(id, optimization_id, baseline_revision_id, role, status, generation, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)`, workID, optimizationID, baselineID, RoleDiagnosis, WorkPending, now); err != nil {
		return fmt.Errorf("create Diagnosis work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO diagnoses
		(id, optimization_id, baseline_revision_id, work_id, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, diagnosisID, optimizationID, baselineID, workID, DiagnosisPending, now); err != nil {
		return fmt.Errorf("create Diagnosis: %w", err)
	}
	if err := insertEffect(ctx, tx, e.newID(), optimizationID, "work.start_requested", mustJSON(map[string]string{"work_id": workID}), now); err != nil {
		return err
	}
	return nil
}

func (e *Engine) applyFinishDiagnosis(ctx context.Context, tx *sql.Tx, command FinishDiagnosis) (Receipt, error) {
	if command.WorkID == "" || (command.Outcome != DiagnosisReady && command.Outcome != DiagnosisUnavailable) || len(command.Report) == 0 || !json.Valid(command.Report) {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id, ready or unavailable outcome, and a valid report are required")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if optimization.FlowVersion != FlowVersion2 {
		return Receipt{}, domainError(CodeInvalidTransition, "Diagnosis is a flow v2 operation")
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationOptimizing && optimization.Status != OptimizationDraining {
		return Receipt{}, domainError(CodeInvalidTransition, "Diagnosis can finish only while optimizing")
	}
	var diagnosisID, baselineID string
	var workRole WorkRole
	var workStatus WorkStatus
	err = tx.QueryRowContext(ctx, `SELECT d.id, d.baseline_revision_id, w.role, w.status
		FROM diagnoses d JOIN works w ON w.id = d.work_id
		WHERE d.work_id = ? AND d.optimization_id = ?`, command.WorkID, optimization.ID).
		Scan(&diagnosisID, &baselineID, &workRole, &workStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeWorkNotFound, "Diagnosis work was not found")
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("read Diagnosis work: %w", err)
	}
	if workRole != RoleDiagnosis || workStatus != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not an active Diagnosis")
	}
	var baselineStatus BaselineStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM baseline_revisions WHERE id = ?`, baselineID).Scan(&baselineStatus); err != nil {
		return Receipt{}, fmt.Errorf("read Diagnosis baseline: %w", err)
	}
	if baselineStatus != BaselineAccepted {
		return Receipt{}, domainError(CodeInvalidTransition, "Diagnosis baseline is not accepted")
	}
	var bestSHA string
	if err := tx.QueryRowContext(ctx, `SELECT commit_sha FROM best_revisions WHERE optimization_id = ? AND sequence = 0`, optimization.ID).Scan(&bestSHA); err != nil {
		return Receipt{}, fmt.Errorf("read Diagnosis Best revision zero: %w", err)
	}
	caseSet, err := currentIterationCaseSet(ctx, tx, optimization.ID)
	if err != nil {
		return Receipt{}, err
	}
	if err := validateDiagnosisReport(command.Report, command.Outcome, baselineID, bestSHA, caseSet.CaseIDs, command.Artifacts); err != nil {
		return Receipt{}, domainError(CodeInvalidCommand, err.Error())
	}

	now, receiptID := e.timestamp(), e.newID()
	if _, err := tx.ExecContext(ctx, `UPDATE diagnoses SET status = ?, report_json = ?, report_sha256 = ?, finished_at = ? WHERE id = ? AND status = ?`,
		command.Outcome, []byte(command.Report), jsonDigest(command.Report), now, diagnosisID, DiagnosisPending); err != nil {
		return Receipt{}, fmt.Errorf("finish Diagnosis: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
		return Receipt{}, fmt.Errorf("complete Diagnosis work: %w", err)
	}
	if optimization.Status != OptimizationDraining {
		if err := e.seedInitialIterationAttempts(ctx, tx, optimization.ID, baselineID, bestSHA, now); err != nil {
			return Receipt{}, err
		}
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, fmt.Errorf("advance Optimization after Diagnosis: %w", err)
	}
	payload := mustJSON(map[string]string{"diagnosis_id": diagnosisID, "work_id": command.WorkID, "outcome": string(command.Outcome)})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "diagnosis.finished", payload, now); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	for _, artifact := range command.Artifacts {
		if err := e.insertArtifact(ctx, tx, command.WorkID, receiptID, artifact, now); err != nil {
			return Receipt{}, err
		}
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

// ReplayDiagnosis resolves a completed terminal request before callers touch
// the frozen source snapshot or Work-owned evidence files.
func (e *Engine) ReplayDiagnosis(ctx context.Context, workID, requestID string, outcome DiagnosisStatus, report json.RawMessage) (Receipt, bool, error) {
	if workID == "" || requestID == "" || len(report) == 0 || !json.Valid(report) {
		return Receipt{}, false, domainError(CodeInvalidCommand, "work_id, request_id, and a valid Diagnosis report are required")
	}
	var receipt Receipt
	err := e.db.QueryRowContext(ctx, `SELECT receipt_id, command_type, revision, result_json FROM operation_receipts WHERE request_id = ?`, requestID).
		Scan(&receipt.ID, &receipt.Command, &receipt.Revision, &receipt.Result)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read Diagnosis receipt replay: %w", err)
	}
	if receipt.Command != (FinishDiagnosis{}).commandName() {
		return Receipt{}, false, domainError(CodeIdempotencyConflict, "request_id was already used for a different command")
	}
	var result struct {
		WorkID  string          `json:"work_id"`
		Outcome DiagnosisStatus `json:"outcome"`
	}
	if err := json.Unmarshal(receipt.Result, &result); err != nil || result.WorkID == "" {
		return Receipt{}, false, domainError(CodeStateCorrupt, "Diagnosis receipt result is invalid")
	}
	var storedReport []byte
	var storedOutcome DiagnosisStatus
	if err := e.db.QueryRowContext(ctx, `SELECT report_json, status FROM diagnoses WHERE work_id = ?`, result.WorkID).Scan(&storedReport, &storedOutcome); err != nil {
		return Receipt{}, false, domainError(CodeStateCorrupt, "Diagnosis receipt has no persisted report")
	}
	requestedCanonical, err := canonicalContractJSON(report)
	if err != nil {
		return Receipt{}, false, domainError(CodeInvalidCommand, err.Error())
	}
	storedCanonical, err := canonicalContractJSON(storedReport)
	if err != nil {
		return Receipt{}, false, domainError(CodeStateCorrupt, "stored Diagnosis report is invalid")
	}
	if result.WorkID != workID || result.Outcome != outcome || storedOutcome != outcome || !bytes.Equal(requestedCanonical, storedCanonical) {
		return Receipt{}, false, domainError(CodeIdempotencyConflict, "request_id was already used with different Diagnosis input")
	}
	receipt.RequestID = requestID
	receipt.Replayed = true
	return receipt, true, nil
}

// validateDiagnosisReport applies the canonical Diagnosis report schema first,
// then provenance and graph shape. Profiler metric semantics stay with
// Integration; Pika's durable contract is that every conclusion is traceable.
func validateDiagnosisReport(raw json.RawMessage, outcome DiagnosisStatus, baselineID, bestSHA string, allowedCases []string, artifacts []ArtifactInput) error {
	report, err := kdacontract.ParseDiagnosisReport(raw)
	if err != nil {
		return err
	}
	if report.SchemaVersion != 1 || report.Subject.BaselineRevisionID != baselineID || report.Subject.BestSHA != bestSHA {
		return errors.New("Diagnosis report has a different Baseline or Best subject")
	}
	allowed := make(map[string]bool, len(allowedCases))
	for _, id := range allowedCases {
		allowed[id] = true
	}
	if len(report.Coverage.CaseIDs) == 0 || !uniqueSubset(report.Coverage.CaseIDs, allowed) {
		return errors.New("Diagnosis coverage must be a non-empty subset of the initial Iteration Case Set")
	}
	artifactPaths := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		artifactPaths[artifact.RelativePath] = true
	}
	for _, artifact := range report.Artifacts {
		if artifact.Path == "" || !artifactPaths[artifact.Path] {
			return fmt.Errorf("Diagnosis artifact %q has no submitted receipt", artifact.Path)
		}
	}
	observations := map[string]bool{}
	for _, observation := range report.Observations {
		if observation.ID == "" || observations[observation.ID] || observation.Summary == "" || !uniqueSubset(observation.CaseIDs, allowed) {
			return errors.New("Diagnosis observations are incomplete or duplicated")
		}
		if len(observation.SourceArtifacts) == 0 {
			return errors.New("Diagnosis observation requires a submitted source artifact")
		}
		for _, path := range observation.SourceArtifacts {
			if !artifactPaths[path] {
				return fmt.Errorf("Diagnosis observation references unknown artifact %q", path)
			}
		}
		observations[observation.ID] = true
	}
	bottlenecks := map[string]bool{}
	for _, bottleneck := range report.Bottlenecks {
		if bottleneck.ID == "" || bottlenecks[bottleneck.ID] || bottleneck.Summary == "" || (bottleneck.Confidence != "high" && bottleneck.Confidence != "medium" && bottleneck.Confidence != "low") {
			return errors.New("Diagnosis bottlenecks are incomplete or duplicated")
		}
		for _, id := range bottleneck.ObservationIDs {
			if !observations[id] {
				return fmt.Errorf("Diagnosis bottleneck references unknown observation %q", id)
			}
		}
		if len(bottleneck.ObservationIDs) == 0 {
			return errors.New("Diagnosis bottleneck requires an observation")
		}
		bottlenecks[bottleneck.ID] = true
	}
	ranks := make([]int, 0, len(report.Hypotheses))
	seenHypotheses := map[string]bool{}
	for _, hypothesis := range report.Hypotheses {
		if hypothesis.ID == "" || seenHypotheses[hypothesis.ID] || hypothesis.Rank < 1 || hypothesis.Summary == "" || !uniqueSubset(hypothesis.TargetCaseIDs, allowed) {
			return errors.New("Diagnosis hypotheses are incomplete or duplicated")
		}
		for _, id := range hypothesis.BottleneckIDs {
			if !bottlenecks[id] {
				return fmt.Errorf("Diagnosis hypothesis references unknown bottleneck %q", id)
			}
		}
		if len(hypothesis.BottleneckIDs) == 0 {
			return errors.New("Diagnosis hypothesis requires a bottleneck")
		}
		seenHypotheses[hypothesis.ID] = true
		ranks = append(ranks, int(hypothesis.Rank))
	}
	sort.Ints(ranks)
	for index, rank := range ranks {
		if rank != index+1 {
			return errors.New("Diagnosis hypothesis ranks must be dense starting at one")
		}
	}
	if outcome == DiagnosisReady && (len(report.Observations) == 0 || len(report.Bottlenecks) == 0 || len(report.Hypotheses) == 0) {
		return errors.New("ready Diagnosis requires observations, bottlenecks, and hypotheses")
	}
	if outcome == DiagnosisUnavailable {
		found := false
		for _, limitation := range report.Limitations {
			if limitation.AttemptedCommand != "" && limitation.FailureCategory != "" && limitation.ErrorText != "" {
				found = true
				break
			}
		}
		if !found {
			return errors.New("unavailable Diagnosis requires a factual attempted-command failure")
		}
	}
	return nil
}

func uniqueSubset(values []string, allowed map[string]bool) bool {
	if len(values) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || !allowed[value] || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func jsonDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// summarizeKnowledge mirrors the status transitions used by Context Bundle
// v4, while retaining only counts for operator-facing status.  In particular,
// an Integration rejection is not a global negative technique record.
func summarizeKnowledge(view *View) error {
	for index := range view.Diagnoses {
		if len(view.Diagnoses[index].Report) == 0 {
			continue
		}
		_, _, hypotheses, err := diagnosisReportCounts(view.Diagnoses[index].Report)
		if err != nil {
			return domainError(CodeStateCorrupt, "stored Diagnosis report is invalid JSON")
		}
		view.Diagnoses[index].HypothesisCount = hypotheses
	}
	records, err := BuildKnowledgeProjection(RuntimeWork{BestSHA: bestSHAFromView(*view), Diagnosis: diagnosisForView(*view)}, *view, view.IterationExperiments, nil)
	if err != nil {
		return domainError(CodeStateCorrupt, "stored knowledge projection source is invalid JSON")
	}
	for _, record := range records {
		switch record.Status {
		case "verified":
			view.Knowledge.Verified++
		case "observed-negative", "integration-rejected":
			view.Knowledge.Negative++
		case "inconclusive":
			view.Knowledge.Inconclusive++
		default:
			view.Knowledge.Provisional++
		}
	}
	return nil
}

func bestSHAFromView(view View) string {
	if view.Best != nil {
		return view.Best.CommitSHA
	}
	return ""
}

func diagnosisForView(view View) *DiagnosisView {
	if len(view.Diagnoses) == 0 {
		return nil
	}
	return &view.Diagnoses[len(view.Diagnoses)-1]
}

// diagnosisReportCounts is shared by Inspect and RuntimeWork so every
// projection derives count fields from the persisted report bytes rather than
// retaining an independently updated summary.
func diagnosisReportCounts(raw json.RawMessage) (observations, bottlenecks, hypotheses int64, err error) {
	report, err := kdacontract.ParseDiagnosisReport(raw)
	if err != nil {
		return 0, 0, 0, err
	}
	return int64(len(report.Observations)), int64(len(report.Bottlenecks)), int64(len(report.Hypotheses)), nil
}
