package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// WorkspaceIdentity is the immutable Optimization Workspace identity mirrored
// into SQLite. The JSON manifest remains the bootstrap authority; this record
// prevents a database from being opened under a different Workspace.
type WorkspaceIdentity struct {
	ID                 string `json:"id"`
	Root               string `json:"root"`
	SourceRepository   string `json:"source_repository"`
	GitCommonDir       string `json:"git_common_dir"`
	GitCommonDirDevice uint64 `json:"git_common_dir_device"`
	GitCommonDirInode  uint64 `json:"git_common_dir_inode"`
	InitialSHA         string `json:"initial_sha"`
}

type GitWorktreeRecord struct {
	Role           string `json:"role"`
	AttemptID      string `json:"attempt_id,omitempty"`
	IterationRound int64  `json:"iteration_round,omitempty"`
	Branch         string `json:"branch"`
	Repository     string `json:"repository"`
	HeadSHA        string `json:"head_sha"`
	State          string `json:"state"`
	UpdatedAt      string `json:"updated_at"`
}

func (e *Engine) EnsureWorkspaceIdentity(ctx context.Context, identity WorkspaceIdentity) error {
	if identity.ID == "" || identity.Root == "" || identity.SourceRepository == "" || identity.GitCommonDir == "" || identity.InitialSHA == "" {
		return errors.New("complete Workspace identity is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var stored WorkspaceIdentity
	err := e.db.QueryRowContext(ctx, `SELECT workspace_id, root, source_repository, git_common_dir,
		git_common_dir_device, git_common_dir_inode, initial_sha FROM workspace_identity WHERE singleton = 1`).Scan(
		&stored.ID, &stored.Root, &stored.SourceRepository, &stored.GitCommonDir,
		&stored.GitCommonDirDevice, &stored.GitCommonDirInode, &stored.InitialSHA,
	)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = e.db.ExecContext(ctx, `INSERT INTO workspace_identity(singleton, workspace_id, root, source_repository,
			git_common_dir, git_common_dir_device, git_common_dir_inode, initial_sha, created_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)`, identity.ID, identity.Root, identity.SourceRepository, identity.GitCommonDir,
			identity.GitCommonDirDevice, identity.GitCommonDirInode, identity.InitialSHA, e.timestamp())
		if err != nil {
			return fmt.Errorf("record Workspace identity: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Workspace identity: %w", err)
	}
	if stored != identity {
		return fmt.Errorf("database belongs to a different Optimization Workspace: stored %s at %s", stored.ID, stored.Root)
	}
	return nil
}

func (e *Engine) UpsertGitWorktree(ctx context.Context, record GitWorktreeRecord) error {
	if record.Role == "" || record.Branch == "" || record.Repository == "" || record.HeadSHA == "" || record.State == "" {
		return errors.New("complete Git worktree record is required")
	}
	if record.IterationRound < 0 {
		return errors.New("Git worktree iteration round cannot be negative")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	record.UpdatedAt = e.timestamp()
	_, err := e.db.ExecContext(ctx, `INSERT INTO git_worktrees(role, attempt_id, iteration_round, branch, repository, head_sha, state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(role, attempt_id, iteration_round) DO UPDATE SET branch = excluded.branch, repository = excluded.repository,
			head_sha = excluded.head_sha, state = excluded.state, updated_at = excluded.updated_at`,
		record.Role, record.AttemptID, record.IterationRound, record.Branch, record.Repository, record.HeadSHA, record.State, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("record Git worktree: %w", err)
	}
	return nil
}

func (e *Engine) GitWorktrees(ctx context.Context) ([]GitWorktreeRecord, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT role, attempt_id, iteration_round, branch, repository, head_sha, state, updated_at
		FROM git_worktrees ORDER BY role, attempt_id, iteration_round`)
	if err != nil {
		return nil, fmt.Errorf("read Git worktrees: %w", err)
	}
	defer rows.Close()
	var records []GitWorktreeRecord
	for rows.Next() {
		var record GitWorktreeRecord
		if err := rows.Scan(&record.Role, &record.AttemptID, &record.IterationRound, &record.Branch, &record.Repository, &record.HeadSHA, &record.State, &record.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Git worktree: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Git worktrees: %w", err)
	}
	return records, nil
}

// WorkRepository resolves the durable Git worktree in which a Work's
// registered artifacts were created. The optimization source repository is
// deliberately not a fallback: artifact paths are scoped to Pika-owned
// worktrees and must never turn into arbitrary source-tree browsing.
func (e *Engine) WorkRepository(ctx context.Context, workID string) (string, error) {
	var role WorkRole
	var attemptID, followUpRequestID sql.NullString
	var iterationRound sql.NullInt64
	if err := e.db.QueryRowContext(ctx, `SELECT role, attempt_id, iteration_round, followup_request_id FROM works WHERE id = ?`, workID).
		Scan(&role, &attemptID, &iterationRound, &followUpRequestID); errors.Is(err, sql.ErrNoRows) {
		return "", domainError(CodeWorkNotFound, "work was not found")
	} else if err != nil {
		return "", fmt.Errorf("read Work repository identity: %w", err)
	}
	if role == RoleFollowUp {
		var targetWorkID string
		if err := e.db.QueryRowContext(ctx, `SELECT target_work_id FROM followup_requests WHERE id = ?`, followUpRequestID.String).Scan(&targetWorkID); err != nil {
			return "", fmt.Errorf("read Follow-up target Work: %w", err)
		}
		return e.WorkRepository(ctx, targetWorkID)
	}
	worktreeRole, worktreeAttemptID, worktreeRound := "base", "", int64(0)
	if role == RoleIteration || role == RoleIntegration {
		worktreeRole, worktreeAttemptID, worktreeRound = "attempt", attemptID.String, iterationRound.Int64
	}
	var repository string
	if err := e.db.QueryRowContext(ctx, `SELECT repository FROM git_worktrees WHERE role = ? AND attempt_id = ? AND iteration_round = ?`,
		worktreeRole, worktreeAttemptID, worktreeRound).Scan(&repository); errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("Work repository is unavailable for %s", workID)
	} else if err != nil {
		return "", fmt.Errorf("read Work repository: %w", err)
	}
	return repository, nil
}
