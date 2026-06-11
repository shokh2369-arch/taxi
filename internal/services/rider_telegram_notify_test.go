package services

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"

	_ "modernc.org/sqlite"
)

func TestShouldSkipRiderTripTelegramNotify_AppFresh(t *testing.T) {
	db := openRiderTelegramNotifyTestDB(t)
	defer db.Close()

	const riderID int64 = 42
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := db.Exec(`INSERT INTO users (id, telegram_id, role, rider_app_last_seen_at) VALUES (42, 9001, 'rider', ?)`, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx := context.Background()
	if !ShouldSkipRiderTripTelegramNotify(ctx, db, riderID) {
		t.Fatal("expected skip when rider app was seen recently")
	}

	old := time.Now().UTC().Add(-3 * time.Hour).Format("2006-01-02 15:04:05")
	_, _ = db.Exec(`UPDATE users SET rider_app_last_seen_at = ? WHERE id = ?`, old, riderID)
	if ShouldSkipRiderTripTelegramNotify(ctx, db, riderID) {
		t.Fatal("stale app last seen should not skip without http action source")
	}
}

func TestShouldSkipRiderTripTelegramNotify_HTTPActionSource(t *testing.T) {
	db := openRiderTelegramNotifyTestDB(t)
	defer db.Close()

	const riderID int64 = 7
	_, err := db.Exec(`INSERT INTO users (id, telegram_id, role) VALUES (7, 7007, 'rider')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx := auth.WithUser(
		auth.WithActionSource(context.Background(), auth.ActionSourceHTTPApp),
		&auth.User{UserID: riderID, Role: domain.RoleRider},
	)
	if !ShouldSkipRiderTripTelegramNotify(ctx, db, riderID) {
		t.Fatal("http app action source should skip even without recent app activity")
	}
}

func openRiderTelegramNotifyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			telegram_id INTEGER,
			role TEXT,
			rider_app_last_seen_at TEXT
		);`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}
