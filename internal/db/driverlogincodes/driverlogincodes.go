package driverlogincodes

import (
	"context"
	"database/sql"
	"strings"
)

// Ensure creates driver_login_codes / driver_auth_sessions if missing and adds
// failed_attempts when an older driver_login_codes table lacks that column.
// Apps do not run goose on startup; this avoids 500s when a deploy skips migrate.
func Ensure(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS driver_login_codes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used INTEGER NOT NULL DEFAULT 0 CHECK (used IN (0, 1)),
  failed_attempts INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_driver_login_codes_user_created ON driver_login_codes(user_id, created_at)`); err != nil {
		return err
	}
	if err := ensureFailedAttemptsColumn(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS driver_auth_sessions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at INTEGER NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
)`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_driver_auth_sessions_user
    ON driver_auth_sessions(user_id, revoked, expires_at)`)
	return err
}

func ensureFailedAttemptsColumn(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('driver_login_codes')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	has := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(name), "failed_attempts") {
			has = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE driver_login_codes ADD COLUMN failed_attempts INTEGER NOT NULL DEFAULT 0`)
	return err
}
