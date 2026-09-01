package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

var sha256DigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (e *Engine) RecordCommitCapability(ctx context.Context, receipt CommitCapabilityReceipt) error {
	if receipt.WorkID == "" || !isGitSHA(receipt.CommitSHA) || !sha256DigestPattern.MatchString(receipt.CommitKeySHA256) || !sha256DigestPattern.MatchString(receipt.RequestSHA256) {
		return domainError(CodeInvalidCommand, "commit capability receipt is incomplete")
	}
	_, err := e.db.ExecContext(ctx, `INSERT OR IGNORE INTO commit_capability_receipts
		(commit_sha, work_id, commit_key_sha256, request_sha256, created_at) VALUES (?, ?, ?, ?, ?)`,
		receipt.CommitSHA, receipt.WorkID, receipt.CommitKeySHA256, receipt.RequestSHA256, e.timestamp())
	if err != nil {
		return fmt.Errorf("record commit capability: %w", err)
	}
	stored, err := e.CommitCapability(ctx, receipt.WorkID, receipt.CommitSHA)
	if err != nil {
		return domainError(CodeIdempotencyConflict, "commit capability key or SHA was reused with a different request")
	}
	if stored != receipt {
		return domainError(CodeIdempotencyConflict, "commit capability receipt changed")
	}
	return nil
}

func (e *Engine) CommitCapability(ctx context.Context, workID, commitSHA string) (CommitCapabilityReceipt, error) {
	var receipt CommitCapabilityReceipt
	err := e.db.QueryRowContext(ctx, `SELECT work_id, commit_sha, commit_key_sha256, request_sha256
		FROM commit_capability_receipts WHERE work_id = ? AND commit_sha = ?`, workID, commitSHA).
		Scan(&receipt.WorkID, &receipt.CommitSHA, &receipt.CommitKeySHA256, &receipt.RequestSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return CommitCapabilityReceipt{}, domainError(CodeForbidden, "commit has no durable commit_changes capability receipt")
	}
	if err != nil {
		return CommitCapabilityReceipt{}, fmt.Errorf("read commit capability: %w", err)
	}
	return receipt, nil
}
