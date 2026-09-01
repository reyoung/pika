package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/cli"
	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/maintenance"
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

func TestWorkspaceMigrateIterationCasesRejectsUnauthorizedGenerationWithoutWrites(t *testing.T) {
	ctx := context.Background()
	workspace, err := optimizationworkspace.Create(ctx, filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: workspace.Identity.SourceRepository,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.NewFileStore(workspace.Root).Write(maintenance.Status{
		ID: "maintenance", RequestID: "prepare", State: maintenance.StateHolding,
		FromGeneration:    maintenance.Generation{Digest: strings.Repeat("a", 64)},
		ToGeneration:      maintenance.Generation{Digest: strings.Repeat("b", 64)},
		HoldingGeneration: maintenance.Generation{Digest: strings.Repeat("c", 64)},
		Targets:           []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
		StartedAt:         "2026-09-01T13:00:00Z",
		ReadyAt:           "2026-09-01T13:01:00Z",
		HoldingAt:         "2026-09-01T13:02:00Z",
		UpdatedAt:         "2026-09-01T13:02:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	maintenancePath := filepath.Join(workspace.RuntimeRoot, "daemon", "maintenance.json")
	paths := []string{workspace.DatabasePath, maintenancePath}
	before := snapshotFiles(t, paths)
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, []string{"workspace", "migrate-iteration-cases", "--workspace", workspace.Root, "--case-id", "case-1", "--json"}, nil, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "maintenance generation rejected") {
		t.Fatalf("migration exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertFilesUnchanged(t, paths, before)
}

func TestWorkspaceMigrateIterationCasesRejectsEveryActiveMaintenanceStateForAuthorizedGeneration(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := daemonupdate.FileDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []maintenance.State{
		maintenance.StateQuiescing, maintenance.StateReady, maintenance.StateHolding, maintenance.StateFailed,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			workspace, createErr := optimizationworkspace.Create(ctx, filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
			if createErr != nil {
				t.Fatal(createErr)
			}
			engine, openErr := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
			if openErr != nil {
				t.Fatal(openErr)
			}
			if _, applyErr := engine.Apply(ctx, symphony.Init{
				Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: workspace.Identity.SourceRepository,
			}); applyErr != nil {
				t.Fatal(applyErr)
			}
			if closeErr := engine.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			status := maintenance.Status{
				ID: "maintenance", RequestID: "prepare", State: state,
				FromGeneration: maintenance.Generation{Digest: digest, Version: "from"},
				ToGeneration:   maintenance.Generation{Digest: strings.Repeat("b", 64), Version: "to"},
				Targets:        []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
				StartedAt:      "2026-09-01T13:00:00Z",
				UpdatedAt:      "2026-09-01T13:00:00Z",
			}
			if state == maintenance.StateReady || state == maintenance.StateHolding {
				status.ReadyAt = "2026-09-01T13:01:00Z"
				status.UpdatedAt = status.ReadyAt
			}
			if state == maintenance.StateHolding {
				status.HoldingGeneration = status.ToGeneration
				status.HoldingAt = "2026-09-01T13:02:00Z"
				status.UpdatedAt = status.HoldingAt
			}
			if state == maintenance.StateFailed {
				status.Failure = "terminal identity drift"
			}
			if writeErr := maintenance.NewFileStore(workspace.Root).Write(status); writeErr != nil {
				t.Fatal(writeErr)
			}
			paths := []string{workspace.DatabasePath, filepath.Join(workspace.RuntimeRoot, "daemon", "maintenance.json")}
			before := snapshotFiles(t, paths)
			var stdout, stderr bytes.Buffer
			code := cli.Run(ctx, []string{
				"workspace", "migrate-iteration-cases", "--workspace", workspace.Root, "--case-id", "case-1", "--json",
			}, nil, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), "migration is forbidden during maintenance state "+string(state)) {
				t.Fatalf("migration exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			assertFilesUnchanged(t, paths, before)
		})
	}
}

func TestWorkspaceImportRejectsUnauthorizedExistingDestinationWithoutWrites(t *testing.T) {
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), newCommittedRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	paths := seedUnauthorizedMaintenanceWorkspace(t, workspace)
	before := snapshotFiles(t, paths)
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{
		"workspace", "import", "--instance", "legacy-instance", "--workspace", workspace.Root,
	}, nil, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "direct Workspace mutation rejected") {
		t.Fatalf("import exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertFilesUnchanged(t, paths, before)
}

func pendingCLIWork(view symphony.View, role symphony.WorkRole) string {
	for _, work := range view.Works {
		if work.Role == role && work.Status == symphony.WorkPending {
			return work.ID
		}
	}
	return ""
}
