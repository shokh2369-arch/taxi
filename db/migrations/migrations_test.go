package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate migrations directory")
	}
	return filepath.Dir(thisFile)
}

func openFreshDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// The full chain must apply cleanly to an empty database. The container runs
// `migrate -up && exec ./app`, so a migration that fails means the service never
// starts at all.
func TestMigrations_ApplyCleanlyToEmptyDatabase(t *testing.T) {
	db := openFreshDB(t, "migrations_fresh")
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.RunContext(context.Background(), "up", db, migrationsDir(t)); err != nil {
		t.Fatalf("migrate up on a fresh database failed: %v", err)
	}

	// Spot-check the tables the service cannot run without.
	for _, table := range []string{
		"users", "drivers", "ride_requests", "trips", "trip_locations",
		"request_notifications", "driver_ledger", "payments",
		"legal_documents", "legal_acceptances", "fare_settings",
	} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?1`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("table %q missing after migrate up", table)
		}
	}
}

// Migration 034 used to drop legal_acceptances unconditionally, destroying the
// record of who accepted which terms. It must now leave a healthy schema alone.
func TestMigration034_PreservesExistingAcceptances(t *testing.T) {
	db := openFreshDB(t, "migrations_preserve_consent")
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	dir := migrationsDir(t)
	ctx := context.Background()

	// Bring the database up to just before the rebuild.
	if err := goose.UpToContext(ctx, db, dir, 33); err != nil {
		t.Fatalf("migrate up to 33: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, telegram_id, role) VALUES (1, 111, 'driver')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO legal_acceptances (user_id, document_type, version, accepted_at)
		VALUES (1, 'driver_terms', 1, datetime('now'))`); err != nil {
		t.Fatalf("seed acceptance: %v", err)
	}

	if err := goose.UpToContext(ctx, db, dir, 34); err != nil {
		t.Fatalf("migrate up to 34: %v", err)
	}

	var kept int
	if err := db.QueryRow(`SELECT COUNT(*) FROM legal_acceptances WHERE user_id = 1`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Errorf("consent record count = %d, want 1 — migration 034 must not wipe a healthy legal_acceptances table", kept)
	}
}

// 048 runs without a transaction, so a partial run must be recoverable: re-running
// the statements has to succeed rather than crash-looping the container.
func TestMigration048_IsIdempotent(t *testing.T) {
	db := openFreshDB(t, "migrations_048_idempotent")
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	dir := migrationsDir(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, dir, 48); err != nil {
		t.Fatalf("migrate up to 48: %v", err)
	}

	// Simulate an interrupted run: rewind the recorded version and re-apply.
	if _, err := db.Exec(`DELETE FROM goose_db_version WHERE version_id = 48`); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db, dir, 48); err != nil {
		t.Fatalf("re-running migration 048 must succeed, got: %v", err)
	}

	// And the rest of the chain still applies afterwards.
	if err := goose.RunContext(ctx, "up", db, dir); err != nil {
		t.Fatalf("migrate up after replaying 048: %v", err)
	}
}
