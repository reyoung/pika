package symphony

import (
	"context"
	"path/filepath"
	"testing"
)

func TestContextProjectionSelectsFrozenRecentTerminalAttemptHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
		IterationHistoryLimit: 2, IterationHistoryLimitSet: true}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	baselineID := view.Baseline.ID
	if _, err := engine.db.ExecContext(ctx, `UPDATE optimizations SET iteration_case_set_version = 1 WHERE id = 'optimization';
		INSERT INTO iteration_cases(optimization_id, case_id, ordinal, added_in_version, source, created_at)
		VALUES ('optimization', 'case-1', 0, 1, 'seed', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.db.ExecContext(ctx, `INSERT INTO best_revisions(id, optimization_id, sequence, commit_sha, created_at) VALUES ('best', 'optimization', 0, 'base', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	attempts := []struct{ id, status, updated string }{
		{"old-accepted", "accepted", "2026-01-01T00:00:01Z"},
		{"new-running", "iterating", "2026-01-01T00:00:05Z"},
		{"new-rejected", "rejected", "2026-01-01T00:00:03Z"},
		{"newest-accepted", "accepted", "2026-01-01T00:00:04Z"},
		{"current", "iterating", "2026-01-01T00:00:06Z"},
	}
	for slot, attempt := range attempts {
		round := 1
		if attempt.id == "current" {
			round = 2
		}
		if _, err := engine.db.ExecContext(ctx, `INSERT INTO attempts
			(id, optimization_id, slot_index, status, base_best_sequence, base_sha, current_iteration_round, summary, history_limit, created_at, updated_at)
			VALUES (?, 'optimization', ?, ?, 0, 'base', ?, ?, 2, ?, ?)`, attempt.id, slot, attempt.status, round, "summary "+attempt.id, attempt.updated, attempt.updated); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.db.ExecContext(ctx, `INSERT INTO works
			(id, optimization_id, baseline_revision_id, role, status, generation, attempt_id, iteration_round, created_at)
			VALUES (?, 'optimization', ?, ?, ?, 1, ?, ?, ?)`, "work-"+attempt.id, baselineID, RoleIteration,
			map[bool]WorkStatus{true: WorkPending, false: WorkCompleted}[attempt.id == "current"], attempt.id, round, attempt.updated); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.db.ExecContext(ctx, `INSERT INTO iteration_rounds
		(id, attempt_id, round, kind, base_sha, status, iteration_case_set_version, created_at, finished_at)
		VALUES ('previous-round', 'current', 1, 'initial', 'base', 'candidate', 1, '2026-01-01T00:00:01Z', '2026-01-01T00:00:02Z'),
		       ('current-round', 'current', 2, 'stale_best', 'base', 'running', 1, '2026-01-01T00:00:06Z', NULL);
		INSERT INTO iteration_round_cases(attempt_id, round, ordinal, case_id)
		VALUES ('current', 1, 0, 'case-1'), ('current', 2, 0, 'case-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.db.ExecContext(ctx, `INSERT INTO works
		(id, optimization_id, baseline_revision_id, role, status, generation, attempt_id, iteration_round, created_at, finished_at)
		VALUES ('work-current-previous', 'optimization', ?, ?, ?, 1, 'current', 1, '2026-01-01T00:00:01Z', '2026-01-01T00:00:02Z')`, baselineID, RoleIteration, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	session := AgentSession{ID: "session", WorkID: "work-current", Generation: 1, Role: RoleIteration, AgentKind: "codex", AgentName: "agent", Status: AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	projection, err := engine.ContextProjection(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.TargetWork.IterationHistoryLimit != 2 || len(projection.AttemptHistories) != 2 {
		t.Fatalf("history projection = %+v", projection.AttemptHistories)
	}
	if projection.AttemptHistories[0].Attempt.ID != "new-rejected" || projection.AttemptHistories[1].Attempt.ID != "newest-accepted" {
		t.Fatalf("history order = %s, %s", projection.AttemptHistories[0].Attempt.ID, projection.AttemptHistories[1].Attempt.ID)
	}
	if projection.PreviousRound == nil || projection.PreviousRound.Round.Round != 1 || projection.PreviousRound.Work.ID != "work-current-previous" {
		t.Fatalf("previous Round = %+v", projection.PreviousRound)
	}
}
