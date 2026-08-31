package symphony

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenConfiguresAndMigratesSQLite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state", "pika.db")
	engine, err := Open(ctx, databasePath, Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	var journalMode string
	if err := engine.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}
	var synchronous int
	if err := engine.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("read synchronous mode: %v", err)
	}
	if synchronous != 2 {
		t.Fatalf("synchronous = %d, want FULL (2)", synchronous)
	}
	var foreignKeys int
	if err := engine.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}
	var busyTimeout int
	if err := engine.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}
	var version int
	var checksum string
	if err := engine.db.QueryRowContext(ctx, "SELECT version, checksum FROM migrations ORDER BY version DESC LIMIT 1").Scan(&version, &checksum); err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if version != schemaVersion || len(checksum) != 64 {
		t.Fatalf("migration = version %d, checksum %q", version, checksum)
	}
	wantTables := map[string]bool{
		"agent_sessions": false, "back_offs": false, "baseline_revisions": false, "baseline_verifications": false,
		"domain_events": false, "evidence_artifacts": false, "init_diagnostics": false, "instruction_snapshots": false,
		"migrations": false, "operation_receipts": false, "optimizations": false, "pane_bindings": false,
		"runtime_outbox": false, "session_grants": false, "works": false, "attempts": false,
		"best_revisions": false, "iteration_rounds": false, "integrations": false, "git_intents": false,
		"iteration_cases": false, "iteration_round_cases": false,
		"workspace_identity": false, "git_worktrees": false,
		"context_snapshots":        false,
		"scheduler_control_cycles": false, "session_control_actions": false,
	}
	rows, err := engine.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("list schema tables: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan schema table: %v", err)
		}
		if _, expected := wantTables[name]; expected {
			wantTables[name] = true
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close schema rows: %v", err)
	}
	for table, found := range wantTables {
		if !found {
			t.Errorf("schema is missing table %q", table)
		}
	}
	info, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o, want 600", info.Mode().Perm())
	}
}

func TestOpenProtectsPreexistingDatabaseDirectory(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "instance")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	engine, err := Open(context.Background(), filepath.Join(directory, "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("database directory mode = %o", info.Mode().Perm())
	}
}

func TestForeignKeyViolationIsRejected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	_, err = engine.db.ExecContext(ctx, `INSERT INTO baseline_revisions
        (id, optimization_id, number, status, created_at) VALUES ('baseline', 'missing', 1, 'drafting', 'now')`)
	if err == nil {
		t.Fatal("foreign-key violating insert succeeded")
	}
}

func TestOpenRejectsUnknownFutureSchema(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	if _, err := engine.db.ExecContext(ctx, `INSERT INTO migrations(version, checksum, applied_at) VALUES (?, 'future', 'now')`, schemaVersion+1); err != nil {
		t.Fatalf("inject future migration: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}
	_, err = Open(ctx, path, Options{})
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open error = %v, want future-schema rejection", err)
	}
}

func TestOpenRejectsChangedAppliedMigration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.db.ExecContext(ctx, `UPDATE migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, path, Options{})
	if err == nil || !strings.Contains(err.Error(), "migration 1 checksum mismatch") {
		t.Fatalf("open error = %v", err)
	}
}

func TestOpenRejectsCorruptDomainState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := Open(ctx, path, Options{NewID: func() string { return "unused" }})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	if _, err := engine.db.ExecContext(ctx, `INSERT INTO optimizations
        (id, status, revision, repository, created_at, updated_at)
        VALUES ('optimization', 'impossible', 1, '/repo', 'now', 'now')`); err != nil {
		t.Fatalf("inject corrupt optimization: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}
	_, err = Open(ctx, path, Options{})
	if err == nil || !strings.Contains(err.Error(), string(CodeStateCorrupt)) {
		t.Fatalf("open error = %v, want corrupt-state rejection", err)
	}
}

func TestStatusReportsSQLiteAndJournalDiskGrowth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init-storage"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Storage.DatabaseBytes <= 0 || view.Storage.PageCount <= 0 || view.Storage.PageSize <= 0 {
		t.Fatalf("storage visibility = %+v", view.Storage)
	}
	if view.Storage.ToolPayloadBytes != 0 || view.Storage.ProviderEventBytes != 0 {
		t.Fatalf("empty journal storage = %+v", view.Storage)
	}
}

func TestBackupCreatesConsistentSQLiteSnapshotWithoutStoppingWriter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	engine, err := Open(ctx, filepath.Join(root, "live", "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init-backup"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "backups", "snapshot.db")
	if err := engine.Backup(ctx, destination); err != nil {
		t.Fatalf("backup live WAL database: %v", err)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		t.Fatalf("backup artifact: info=%v err=%v", info, err)
	}
	snapshot, err := Open(ctx, destination, Options{})
	if err != nil {
		t.Fatalf("open backup snapshot: %v", err)
	}
	defer snapshot.Close()
	view, err := snapshot.Inspect(ctx, Status{})
	if err != nil || view.Optimization.ID != "optimization" || view.Optimization.Revision != 1 {
		t.Fatalf("backup domain state: view=%+v err=%v", view, err)
	}
	if err := engine.Backup(ctx, destination); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("backup overwrote existing artifact: %v", err)
	}
}
