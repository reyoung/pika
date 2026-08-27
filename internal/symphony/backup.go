package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Backup creates one self-contained SQLite snapshot from the live WAL database.
// The destination must be a new absolute path; it is never overwritten.
func (e *Engine) Backup(ctx context.Context, destination string) error {
	if destination == "" || !filepath.IsAbs(destination) {
		return errors.New("backup destination must be an absolute path")
	}
	clean := filepath.Clean(destination)
	if clean == e.databasePath || clean == e.databasePath+"-wal" || clean == e.databasePath+"-shm" {
		return errors.New("backup destination must differ from the live SQLite files")
	}
	if _, err := os.Lstat(clean); err == nil {
		return fmt.Errorf("backup destination already exists: %s", clean)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	var busy, logFrames, checkpointed int64
	if err := e.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint SQLite before backup: %w", err)
	}
	if _, err := e.db.ExecContext(ctx, `VACUUM INTO ?`, clean); err != nil {
		_ = os.Remove(clean)
		return fmt.Errorf("create SQLite backup: %w", err)
	}
	valid := false
	defer func() {
		if !valid {
			_ = os.Remove(clean)
		}
	}()
	if err := os.Chmod(clean, 0o600); err != nil {
		return fmt.Errorf("protect SQLite backup: %w", err)
	}
	check, err := sql.Open("sqlite", clean)
	if err != nil {
		return fmt.Errorf("open SQLite backup for validation: %w", err)
	}
	defer check.Close()
	var quickCheck string
	if err := check.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&quickCheck); err != nil || quickCheck != "ok" {
		return fmt.Errorf("validate SQLite backup: quick_check=%q err=%v", quickCheck, err)
	}
	var version int
	if err := check.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&version); err != nil || version != schemaVersion {
		return fmt.Errorf("validate SQLite backup schema: version=%d err=%v", version, err)
	}
	valid = true
	return nil
}
