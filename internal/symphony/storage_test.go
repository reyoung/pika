package symphony

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func applyMigrationsThrough(t *testing.T, ctx context.Context, db *sql.DB, through int) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `CREATE TABLE migrations (version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create migration ledger: %v", err)
	}
	for _, migration := range schemaMigrations[:through] {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin migration %d: %v", migration.version, err)
		}
		if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		digest := sha256.Sum256([]byte(migration.sql))
		if _, err := tx.ExecContext(ctx, `INSERT INTO migrations(version, checksum, applied_at) VALUES (?, ?, 'test')`, migration.version, hex.EncodeToString(digest[:])); err != nil {
			_ = tx.Rollback()
			t.Fatalf("record migration %d: %v", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration %d: %v", migration.version, err)
		}
	}
}

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
		"skill_snapshots": false, "skill_snapshot_entries": false, "diagnoses": false, "iteration_experiments": false,
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

func TestMigrationV21FailureRollsBackToRecoverableV20Database(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	applyMigrationsThrough(t, ctx, db, 19)
	// This collision occurs in v21. v20 should remain committed and v21 must
	// roll back atomically.
	if _, err := db.ExecContext(ctx, `CREATE TABLE diagnoses (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, "test"); err == nil || !strings.Contains(err.Error(), "apply schema migration 21") {
		t.Fatalf("v21 collision error = %v", err)
	}
	var flowVersionColumns int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('optimizations') WHERE name = 'flow_version'`).Scan(&flowVersionColumns); err != nil {
		t.Fatal(err)
	}
	if flowVersionColumns != 0 {
		t.Fatal("failed v21 migration left flow_version behind")
	}
	var latest int
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM migrations`).Scan(&latest); err != nil || latest != 20 {
		t.Fatalf("migration ledger after rollback = %d, err=%v", latest, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE diagnoses`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("recover v20 database through v21: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "legacy"}, OptimizationID: "legacy", Repository: "/repo"}); err != nil {
		t.Fatalf("recovered database cannot run flow-v1: %v", err)
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

func TestSchemaV20UpgradeAndFailedMigrationRollbackPreserveLegacyState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	backupPath := filepath.Join(root, "verified-v19.db")
	createDatabaseThroughMigration(t, backupPath, 19)

	legacy, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `
		INSERT INTO optimizations(id, status, revision, repository, created_at, updated_at)
		VALUES ('optimization', 'drafting_baseline', 7, '/repo', 'now', 'now');
		INSERT INTO baseline_revisions(id, optimization_id, number, status, created_at)
		VALUES ('legacy-baseline', 'optimization', 1, 'drafting', 'now')`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backupPath, 0o600); err != nil {
		t.Fatal(err)
	}

	failedPath := filepath.Join(root, "failed-candidate.db")
	copyFileForTest(t, backupPath, failedPath)
	failed, err := sql.Open("sqlite", failedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failed.ExecContext(ctx, `CREATE TABLE benchmark_metric_definitions(conflict TEXT)`); err != nil {
		_ = failed.Close()
		t.Fatal(err)
	}
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, failedPath, Options{}); err == nil || !strings.Contains(err.Error(), "apply schema migration 20") {
		t.Fatalf("candidate migration error = %v", err)
	}

	failed, err = sql.Open("sqlite", failedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer failed.Close()
	var version int
	if err := failed.QueryRowContext(ctx, `SELECT MAX(version) FROM migrations`).Scan(&version); err != nil || version != 19 {
		t.Fatalf("failed candidate schema version = %d, err=%v", version, err)
	}
	rows, err := failed.QueryContext(ctx, `PRAGMA table_info(baseline_revisions)`)
	if err != nil {
		t.Fatal(err)
	}
	hasMeasurementColumn := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		hasMeasurementColumn = hasMeasurementColumn || name == "measurement_contract_version"
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if hasMeasurementColumn {
		t.Fatal("failed migration did not roll back ALTER TABLE")
	}

	restoredPath := filepath.Join(root, "restored.db")
	copyFileForTest(t, backupPath, restoredPath)
	restored, err := Open(ctx, restoredPath, Options{})
	if err != nil {
		t.Fatalf("restore verified backup and migrate: %v", err)
	}
	defer restored.Close()
	var contractVersion sql.NullInt64
	if err := restored.db.QueryRowContext(ctx, `SELECT measurement_contract_version FROM baseline_revisions WHERE id = 'legacy-baseline'`).Scan(&contractVersion); err != nil {
		t.Fatal(err)
	}
	if contractVersion.Valid {
		t.Fatalf("legacy revision was heuristically backfilled: %+v", contractVersion)
	}
	var quickCheck string
	if err := restored.db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&quickCheck); err != nil || quickCheck != "ok" {
		t.Fatalf("restored database quick_check = %q, err=%v", quickCheck, err)
	}
}

func createDatabaseThroughMigration(t *testing.T, path string, through int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE migrations (
		version INTEGER PRIMARY KEY,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range schemaMigrations {
		if migration.version > through {
			break
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(migration.sql); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply fixture migration %d: %v", migration.version, err)
		}
		digest := sha256.Sum256([]byte(migration.sql))
		if _, err := tx.Exec(`INSERT INTO migrations(version, checksum, applied_at) VALUES (?, ?, 'fixture')`, migration.version, hex.EncodeToString(digest[:])); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func copyFileForTest(t *testing.T, source, destination string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
