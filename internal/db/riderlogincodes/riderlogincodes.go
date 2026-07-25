package riderlogincodes

import (
	"context"
	"database/sql"
	"log"
	"strings"
)

// Ensure creates rider_login_codes and rider_auth_sessions if missing, and
// transparently rebuilds either table when an older / mismatched version
// already exists.
//
// Why "rebuild on drift" is safe here:
//
//   - rider_login_codes only ever holds short-lived 6-digit OTPs (5-minute TTL).
//     Dropping it loses at most a few seconds of in-flight login codes; the
//     rider client retries by tapping "Send code" again.
//   - rider_auth_sessions holds refresh-token hashes. Dropping it forces every
//     rider to re-login once. That is a perfectly acceptable trade-off the
//     first time a deployment with a mismatched legacy schema starts up; in
//     normal steady-state operation the tables already match and no rebuild
//     happens.
//
// Why we need this at all: a previous experimental deploy on the production
// Turso DB pre-created a rider_login_codes table with a different shape (e.g.
// the user_id-based driver_login_codes shape) and advanced goose_db_version
// past our migration 057. On subsequent boots `CREATE TABLE IF NOT EXISTS`
// silently no-ops on the wrong table, then `CREATE INDEX ...(phone, ...)`
// fails with "no such column: phone" and the process exits.
//
// Mirrors the legalrepair startup helper pattern.
func Ensure(ctx context.Context, db *sql.DB) error {
	if err := ensureRiderLoginCodes(ctx, db); err != nil {
		return err
	}
	if err := ensureRiderAuthSessions(ctx, db); err != nil {
		return err
	}
	return nil
}

// ensureRiderLoginCodes makes the rider_login_codes table match the shape we
// expect. If it exists with the wrong schema we DROP and recreate it.
func ensureRiderLoginCodes(ctx context.Context, db *sql.DB) error {
	exists, cols, err := tableInfo(ctx, db, "rider_login_codes")
	if err != nil {
		return err
	}
	if exists && !hasAll(cols, "phone", "code_hash", "salt", "expires_at", "attempts", "consumed", "created_at") {
		log.Printf("riderlogincodes: rider_login_codes exists with legacy schema cols=%v; rebuilding", cols)
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS rider_login_codes`); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rider_login_codes (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			phone      TEXT NOT NULL,
			code_hash  TEXT NOT NULL,
			salt       TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			attempts   INTEGER NOT NULL DEFAULT 0,
			consumed   INTEGER NOT NULL DEFAULT 0 CHECK (consumed IN (0, 1)),
			created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_rider_login_codes_phone
			ON rider_login_codes(phone, consumed, expires_at)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_rider_login_codes_phone_created
			ON rider_login_codes(phone, created_at)`); err != nil {
		return err
	}
	return nil
}

// ensureRiderAuthSessions makes the rider_auth_sessions table match the shape
// we expect. If it exists with the wrong schema we DROP and recreate it.
func ensureRiderAuthSessions(ctx context.Context, db *sql.DB) error {
	exists, cols, err := tableInfo(ctx, db, "rider_auth_sessions")
	if err != nil {
		return err
	}
	if exists && !hasAll(cols, "user_id", "refresh_hash", "refresh_expires_at", "revoked", "created_at") {
		log.Printf("riderlogincodes: rider_auth_sessions exists with legacy schema cols=%v; rebuilding", cols)
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS rider_auth_sessions`); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rider_auth_sessions (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id            INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			refresh_hash       TEXT NOT NULL UNIQUE,
			refresh_expires_at INTEGER NOT NULL,
			revoked            INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
			created_at         INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_rider_auth_sessions_user
			ON rider_auth_sessions(user_id, revoked, refresh_expires_at)`); err != nil {
		return err
	}
	return nil
}

// tableInfo returns whether the table exists and, if so, the lower-cased
// names of its columns. Uses sqlite_master + pragma_table_info, which works
// on Turso/libSQL just like on plain SQLite.
func tableInfo(ctx context.Context, db *sql.DB, name string) (bool, []string, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?1`, name,
	).Scan(&n); err != nil {
		return false, nil, err
	}
	if n == 0 {
		return false, nil, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM pragma_table_info(?1)`, name)
	if err != nil {
		return true, nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return true, cols, err
		}
		cols = append(cols, strings.ToLower(strings.TrimSpace(col)))
	}
	if err := rows.Err(); err != nil {
		return true, cols, err
	}
	return true, cols, nil
}

func hasAll(cols []string, want ...string) bool {
	got := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		got[c] = struct{}{}
	}
	for _, w := range want {
		if _, ok := got[strings.ToLower(w)]; !ok {
			return false
		}
	}
	return true
}
