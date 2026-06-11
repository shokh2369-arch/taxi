package services

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"taxi-mvp/internal/auth"

	_ "modernc.org/sqlite"
)

func TestShouldSkipDriverTripTelegramNotify_AppFresh(t *testing.T) {
	db := openDriverTelegramNotifyTestDB(t)
	defer db.Close()

	const driverID int64 = 42
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := db.Exec(`
		INSERT INTO users (id, telegram_id, role) VALUES (42, 9001, 'driver');
		INSERT INTO drivers (user_id, app_location_active, app_last_seen_at, app_lat, app_lng)
		VALUES (42, 1, ?, 41.0, 69.0)`, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx := context.Background()
	if !ShouldSkipDriverTripTelegramNotify(ctx, db, driverID) {
		t.Fatal("expected skip when native app location is fresh")
	}
	if ShouldSkipDriverTripTelegramNotify(ctx, db, 999) {
		t.Fatal("unknown driver should not skip")
	}

	_, _ = db.Exec(`UPDATE drivers SET app_location_active = 0 WHERE user_id = ?`, driverID)
	if ShouldSkipDriverTripTelegramNotify(ctx, db, driverID) {
		t.Fatal("stale/off app should not skip without http action source")
	}
}

func TestShouldSkipDriverTripTelegramNotify_HTTPActionSource(t *testing.T) {
	db := openDriverTelegramNotifyTestDB(t)
	defer db.Close()

	const driverID int64 = 7
	_, err := db.Exec(`INSERT INTO users (id, telegram_id, role) VALUES (7, 7007, 'driver');
		INSERT INTO drivers (user_id) VALUES (7)`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx := auth.WithActionSource(context.Background(), auth.ActionSourceHTTPApp)
	if !ShouldSkipDriverTripTelegramNotify(ctx, db, driverID) {
		t.Fatal("http app action source should skip even without app location")
	}
}

func openDriverTelegramNotifyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	schema := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, telegram_id INTEGER, role TEXT);
		CREATE TABLE drivers (
			user_id INTEGER PRIMARY KEY,
			app_location_active INTEGER DEFAULT 0,
			app_last_seen_at TEXT,
			app_lat REAL,
			app_lng REAL
		);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}
