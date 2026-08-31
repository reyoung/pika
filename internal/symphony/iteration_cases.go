package symphony

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
	"github.com/reyoung/pika-go/internal/iterationcases"
)

// MigrateIterationCases explicitly initializes the Iteration Case history of
// a legacy Optimizing workspace. Callers must hold the workspace daemon lock
// and must have opened the Engine with AllowIterationCaseMigration.
func (e *Engine) MigrateIterationCases(ctx context.Context, selectedCaseIDs []string) (IterationCaseSetView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return IterationCaseSetView{}, fmt.Errorf("begin Iteration Case migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var optimizationID string
	var currentVersion sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT id, iteration_case_set_version FROM optimizations LIMIT 1`).Scan(&optimizationID, &currentVersion); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("read Optimization for Iteration Case migration: %w", err)
	}
	if currentVersion.Valid {
		current, err := currentIterationCaseSet(ctx, tx, optimizationID)
		if err != nil {
			return IterationCaseSetView{}, err
		}
		if !slices.Equal(current.CaseIDs, selectedCaseIDs) {
			return IterationCaseSetView{}, domainError(CodeInvalidTransition, "Iteration Case Set is already initialized with a different selection")
		}
		if err := tx.Rollback(); err != nil {
			return IterationCaseSetView{}, fmt.Errorf("close idempotent Iteration Case migration: %w", err)
		}
		if err := validateOnlineState(ctx, e.db, false); err != nil {
			return IterationCaseSetView{}, fmt.Errorf("validate existing Iteration Case migration: %w", err)
		}
		return current, nil
	}
	var attemptCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE optimization_id = ?`, optimizationID).Scan(&attemptCount); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("count legacy Attempts: %w", err)
	}
	if attemptCount == 0 {
		return IterationCaseSetView{}, domainError(CodeInvalidTransition, "workspace has no legacy Attempts and does not require Iteration Case migration")
	}
	var definition []byte
	if err := tx.QueryRowContext(ctx, `SELECT definition_json FROM baseline_revisions
		WHERE optimization_id = ? AND status = 'accepted' ORDER BY number DESC LIMIT 1`, optimizationID).Scan(&definition); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("read accepted Baseline for Iteration Case migration: %w", err)
	}
	fullCaseIDs, err := benchmarkintegrity.FullCaseIDs(definition)
	if err != nil {
		return IterationCaseSetView{}, fmt.Errorf("read Full Case Set for Iteration Case migration: %w", err)
	}
	wantCount := min(iterationcases.InitialLimit, len(fullCaseIDs))
	if len(selectedCaseIDs) != wantCount {
		return IterationCaseSetView{}, domainError(CodeInvalidCommand, fmt.Sprintf("migration requires exactly %d --case-id values", wantCount))
	}
	full := make(map[string]struct{}, len(fullCaseIDs))
	for _, caseID := range fullCaseIDs {
		full[caseID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(selectedCaseIDs))
	for _, caseID := range selectedCaseIDs {
		if caseID == "" {
			return IterationCaseSetView{}, domainError(CodeInvalidCommand, "migration case IDs must be non-empty")
		}
		if _, ok := full[caseID]; !ok {
			return IterationCaseSetView{}, domainError(CodeInvalidCommand, fmt.Sprintf("migration Case %q is outside the Full Case Set", caseID))
		}
		if _, ok := seen[caseID]; ok {
			return IterationCaseSetView{}, domainError(CodeInvalidCommand, fmt.Sprintf("migration contains duplicate Case %q", caseID))
		}
		seen[caseID] = struct{}{}
	}
	now := e.timestamp()
	for ordinal, caseID := range selectedCaseIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO iteration_cases
			(optimization_id, case_id, ordinal, added_in_version, source, created_at)
			VALUES (?, ?, ?, 1, 'manual_migration', ?)`, optimizationID, caseID, ordinal, now); err != nil {
			return IterationCaseSetView{}, fmt.Errorf("record migrated Iteration Case %q: %w", caseID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET iteration_case_set_version = 1 WHERE id = ?`, optimizationID); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("initialize migrated Iteration Case version: %w", err)
	}
	rounds, err := tx.QueryContext(ctx, `SELECT r.attempt_id, r.round, r.status FROM iteration_rounds r
		JOIN attempts a ON a.id = r.attempt_id WHERE a.optimization_id = ? ORDER BY r.created_at, r.id`, optimizationID)
	if err != nil {
		return IterationCaseSetView{}, fmt.Errorf("read legacy Iteration Rounds: %w", err)
	}
	type legacyRound struct {
		attemptID string
		round     int64
		status    string
	}
	var legacyRounds []legacyRound
	for rounds.Next() {
		var item legacyRound
		if err := rounds.Scan(&item.attemptID, &item.round, &item.status); err != nil {
			_ = rounds.Close()
			return IterationCaseSetView{}, fmt.Errorf("scan legacy Iteration Round: %w", err)
		}
		legacyRounds = append(legacyRounds, item)
	}
	if err := rounds.Err(); err != nil {
		_ = rounds.Close()
		return IterationCaseSetView{}, fmt.Errorf("iterate legacy Iteration Rounds: %w", err)
	}
	if err := rounds.Close(); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("close legacy Iteration Rounds: %w", err)
	}
	for _, item := range legacyRounds {
		caseIDs, version := fullCaseIDs, int64(0)
		if item.status == "queued" || item.status == "running" {
			caseIDs, version = selectedCaseIDs, 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE iteration_rounds SET iteration_case_set_version = ?
			WHERE attempt_id = ? AND round = ?`, version, item.attemptID, item.round); err != nil {
			return IterationCaseSetView{}, fmt.Errorf("version legacy Iteration Round: %w", err)
		}
		for ordinal, caseID := range caseIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO iteration_round_cases(attempt_id, round, ordinal, case_id)
				VALUES (?, ?, ?, ?)`, item.attemptID, item.round, ordinal, caseID); err != nil {
				return IterationCaseSetView{}, fmt.Errorf("snapshot legacy Iteration Round: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("commit Iteration Case migration: %w", err)
	}
	if err := validateOnlineState(ctx, e.db, false); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("validate migrated workspace: %w", err)
	}
	return IterationCaseSetView{Version: 1, CaseIDs: append([]string(nil), selectedCaseIDs...)}, nil
}

func seedIterationCaseSet(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, now string) error {
	var definition []byte
	if err := tx.QueryRowContext(ctx, `SELECT definition_json FROM baseline_revisions WHERE id = ?`, baselineID).Scan(&definition); err != nil {
		return fmt.Errorf("read Baseline Definition for Iteration Case Set: %w", err)
	}
	fullCaseIDs, err := benchmarkintegrity.FullCaseIDs(definition)
	if err != nil {
		return fmt.Errorf("read Full Case Set: %w", err)
	}
	selected, err := iterationcases.Seed(fullCaseIDs)
	if err != nil {
		return fmt.Errorf("seed Iteration Case Set: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET iteration_case_set_version = 1
		WHERE id = ? AND iteration_case_set_version IS NULL`, optimizationID); err != nil {
		return fmt.Errorf("initialize Iteration Case Set version: %w", err)
	}
	for ordinal, caseID := range selected {
		if _, err := tx.ExecContext(ctx, `INSERT INTO iteration_cases
			(optimization_id, case_id, ordinal, added_in_version, source, created_at)
			VALUES (?, ?, ?, 1, 'seed', ?)`, optimizationID, caseID, ordinal, now); err != nil {
			return fmt.Errorf("record seeded Iteration Case %q: %w", caseID, err)
		}
	}
	return nil
}

func currentIterationCaseSet(ctx context.Context, q queryContext, optimizationID string) (IterationCaseSetView, error) {
	var result IterationCaseSetView
	if err := q.QueryRowContext(ctx, `SELECT iteration_case_set_version FROM optimizations WHERE id = ?`, optimizationID).Scan(&result.Version); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("read Iteration Case Set version: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT case_id FROM iteration_cases WHERE optimization_id = ? ORDER BY ordinal`, optimizationID)
	if err != nil {
		return IterationCaseSetView{}, fmt.Errorf("read Iteration Case Set: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var caseID string
		if err := rows.Scan(&caseID); err != nil {
			return IterationCaseSetView{}, fmt.Errorf("scan Iteration Case Set: %w", err)
		}
		result.CaseIDs = append(result.CaseIDs, caseID)
	}
	if err := rows.Err(); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("iterate Iteration Case Set: %w", err)
	}
	if len(result.CaseIDs) == 0 {
		return IterationCaseSetView{}, domainError(CodeStateCorrupt, "Iteration Case Set is empty")
	}
	return result, nil
}

type queryContext interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func snapshotIterationCaseSet(ctx context.Context, tx *sql.Tx, optimizationID, attemptID string, round int64) error {
	set, err := currentIterationCaseSet(ctx, tx, optimizationID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE iteration_rounds SET iteration_case_set_version = ?
		WHERE attempt_id = ? AND round = ?`, set.Version, attemptID, round); err != nil {
		return fmt.Errorf("freeze Iteration Case Set version: %w", err)
	}
	for ordinal, caseID := range set.CaseIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO iteration_round_cases(attempt_id, round, ordinal, case_id)
			VALUES (?, ?, ?, ?)`, attemptID, round, ordinal, caseID); err != nil {
			return fmt.Errorf("freeze Iteration Case %q: %w", caseID, err)
		}
	}
	return nil
}

func roundIterationCaseSet(ctx context.Context, q queryContext, attemptID string, round int64) (IterationCaseSetView, error) {
	var result IterationCaseSetView
	if err := q.QueryRowContext(ctx, `SELECT iteration_case_set_version FROM iteration_rounds
		WHERE attempt_id = ? AND round = ?`, attemptID, round).Scan(&result.Version); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("read Iteration Case Snapshot version: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT case_id FROM iteration_round_cases
		WHERE attempt_id = ? AND round = ? ORDER BY ordinal`, attemptID, round)
	if err != nil {
		return IterationCaseSetView{}, fmt.Errorf("read Iteration Case Snapshot: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var caseID string
		if err := rows.Scan(&caseID); err != nil {
			return IterationCaseSetView{}, fmt.Errorf("scan Iteration Case Snapshot: %w", err)
		}
		result.CaseIDs = append(result.CaseIDs, caseID)
	}
	if err := rows.Err(); err != nil {
		return IterationCaseSetView{}, fmt.Errorf("iterate Iteration Case Snapshot: %w", err)
	}
	if len(result.CaseIDs) == 0 {
		return IterationCaseSetView{}, domainError(CodeStateCorrupt, "Iteration Case Snapshot is empty")
	}
	return result, nil
}

func validateRegressionCases(reports []RegressionCase) error {
	for index, report := range reports {
		if report.CaseID == "" || (report.Kind != "correctness" && report.Kind != "performance") || report.Summary == "" {
			return domainError(CodeInvalidCommand, fmt.Sprintf("regression_cases[%d] requires case_id, correctness or performance kind, and summary", index))
		}
		if len(report.Evidence) == 0 || !json.Valid(report.Evidence) {
			return domainError(CodeInvalidCommand, fmt.Sprintf("regression_cases[%d].evidence must be a JSON object", index))
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(report.Evidence, &object); err != nil || object == nil {
			return domainError(CodeInvalidCommand, fmt.Sprintf("regression_cases[%d].evidence must be a JSON object", index))
		}
	}
	return nil
}

func appendIterationCases(ctx context.Context, tx *sql.Tx, optimizationID, integrationID string, reports []RegressionCase, fullCaseIDs []string, now string) (IterationCaseSetView, []string, error) {
	current, err := currentIterationCaseSet(ctx, tx, optimizationID)
	if err != nil {
		return IterationCaseSetView{}, nil, err
	}
	ranked := make([]string, 0, len(reports))
	for _, report := range reports {
		ranked = append(ranked, report.CaseID)
	}
	additions, err := iterationcases.SelectAdditions(fullCaseIDs, current.CaseIDs, ranked)
	if err != nil {
		return IterationCaseSetView{}, nil, domainError(CodeInvalidCommand, "invalid Regression Cases: "+err.Error())
	}
	if len(additions) == 0 {
		return current, nil, nil
	}
	newVersion := current.Version + 1
	ranks := make(map[string]int, len(reports))
	for index, report := range reports {
		ranks[report.CaseID] = index
	}
	for index, caseID := range additions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO iteration_cases
			(optimization_id, case_id, ordinal, added_in_version, source, source_integration_id, source_rank, created_at)
			VALUES (?, ?, ?, ?, 'integration_regression', ?, ?, ?)`, optimizationID, caseID, len(current.CaseIDs)+index,
			newVersion, integrationID, ranks[caseID], now); err != nil {
			return IterationCaseSetView{}, nil, fmt.Errorf("append Regression Case %q: %w", caseID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET iteration_case_set_version = ? WHERE id = ?`, newVersion, optimizationID); err != nil {
		return IterationCaseSetView{}, nil, fmt.Errorf("advance Iteration Case Set version: %w", err)
	}
	current.Version = newVersion
	current.CaseIDs = append(current.CaseIDs, additions...)
	return current, additions, nil
}
