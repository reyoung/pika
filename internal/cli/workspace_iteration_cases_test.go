package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/cli"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
	_ "modernc.org/sqlite"
)

func TestWorkspaceMigrateIterationCasesCommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace, err := optimizationworkspace.Create(ctx, filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: workspace.BaseRepository,
		IterationConcurrency: 1, MaxPendingAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID,
		Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verificationID := pendingCLIWork(view, symphony.RoleBaselineVerification)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "accept"}, WorkID: verificationID,
		Decision: symphony.VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: "baseline"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", workspace.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON;
		DELETE FROM iteration_round_cases;
		DELETE FROM iteration_cases;
		UPDATE iteration_rounds SET iteration_case_set_version = NULL;
		UPDATE optimizations SET iteration_case_set_version = NULL`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, []string{"workspace", "migrate-iteration-cases", "--workspace", workspace.Root, "--case-id", "case-1", "--json"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("migration exit=%d stderr=%s", code, stderr.String())
	}
	var result symphony.IterationCaseSetView
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Version != 1 || len(result.CaseIDs) != 1 || result.CaseIDs[0] != "case-1" {
		t.Fatalf("migration output=%s result=%+v err=%v", stdout.String(), result, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cli.Run(ctx, []string{"workspace", "migrate-iteration-cases", "--workspace", workspace.Root, "--case-id", "case-1", "--json"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("idempotent migration exit=%d stderr=%s", code, stderr.String())
	}
	reopened, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("open migrated workspace: %v", err)
	}
	defer reopened.Close()
}

func pendingCLIWork(view symphony.View, role symphony.WorkRole) string {
	for _, work := range view.Works {
		if work.Role == role && work.Status == symphony.WorkPending {
			return work.ID
		}
	}
	return ""
}
