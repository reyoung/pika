package symphony

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// WorkRoleTerminalReceipt returns the receipt for the role-specific command
// and matching domain event that legitimately completed workID.
func (e *Engine) WorkRoleTerminalReceipt(ctx context.Context, workID string, role WorkRole) (Receipt, bool, error) {
	descriptor, ok := DescribeRole(role)
	if !ok || descriptor.TerminalOperation == "" {
		return Receipt{}, false, fmt.Errorf("Work %s has unsupported role %s", workID, role)
	}
	eventTypes, payloadKey, correlationKey, ok := roleTerminalEvents(role)
	if !ok {
		return Receipt{}, false, fmt.Errorf("Work %s role %s has no terminal event contract", workID, role)
	}
	var optimizationID, storedRole string
	var integrationID sql.NullString
	if err := e.db.QueryRowContext(ctx, `SELECT optimization_id, role, integration_id FROM works WHERE id = ?`, workID).
		Scan(&optimizationID, &storedRole, &integrationID); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, domainError(CodeWorkNotFound, "work was not found")
	} else if err != nil {
		return Receipt{}, false, fmt.Errorf("read terminal Work identity: %w", err)
	}
	if WorkRole(storedRole) != role {
		return Receipt{}, false, domainError(CodeStateCorrupt, "work role changed while checking terminal receipt")
	}
	expected := workID
	if role == RoleIntegration {
		if !integrationID.Valid || integrationID.String == "" {
			return Receipt{}, false, domainError(CodeStateCorrupt, "integration Work has no integration identity")
		}
		expected = integrationID.String
	}
	rows, err := e.db.QueryContext(ctx, `SELECT r.request_id, r.receipt_id, r.command_type, r.revision, r.result_json, e.event_type, e.payload_json
		FROM operation_receipts r
		JOIN domain_events e ON e.optimization_id = ? AND e.revision = r.revision
		WHERE r.command_type = ?
		ORDER BY r.created_at DESC`, optimizationID, descriptor.TerminalOperation)
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read terminal receipt: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var receipt Receipt
		var eventType string
		var payload []byte
		if err := rows.Scan(&receipt.RequestID, &receipt.ID, &receipt.Command, &receipt.Revision, &receipt.Result, &eventType, &payload); err != nil {
			return Receipt{}, false, fmt.Errorf("scan terminal receipt: %w", err)
		}
		if !eventTypes[eventType] {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			return Receipt{}, false, fmt.Errorf("decode terminal event: %w", err)
		}
		var result map[string]any
		if err := json.Unmarshal(receipt.Result, &result); err != nil {
			return Receipt{}, false, fmt.Errorf("decode terminal receipt: %w", err)
		}
		eventCorrelation, _ := event[correlationKey].(string)
		resultCorrelation, _ := result[correlationKey].(string)
		if value, _ := event[payloadKey].(string); value == expected &&
			eventCorrelation != "" && eventCorrelation == resultCorrelation {
			return receipt, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return Receipt{}, false, fmt.Errorf("iterate terminal receipts: %w", err)
	}
	return Receipt{}, false, nil
}

func roleTerminalEvents(role WorkRole) (eventTypes map[string]bool, payloadKey, correlationKey string, ok bool) {
	switch role {
	case RoleBaselineDraft:
		return map[string]bool{"baseline.submitted": true, "baseline.submitted_while_draining": true}, "draft_work_id", "baseline_revision_id", true
	case RoleBaselineVerification:
		return map[string]bool{"baseline.verification_finished": true}, "verification_work_id", "baseline_revision_id", true
	case RoleDiagnosis:
		return map[string]bool{"diagnosis.finished": true}, "work_id", "diagnosis_id", true
	case RoleIteration:
		return map[string]bool{"iteration.finished": true}, "work_id", "attempt_id", true
	case RoleIntegration:
		return map[string]bool{"integration.finished": true}, "integration_id", "integration_id", true
	case RoleFollowUp:
		return map[string]bool{"followup.message_submitted": true}, "generator_work_id", "followup_request_id", true
	default:
		return nil, "", "", false
	}
}
