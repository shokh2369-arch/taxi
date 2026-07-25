package services

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupRetentionDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:retention_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`CREATE TABLE trips (id TEXT PRIMARY KEY, status TEXT NOT NULL)`,
		`CREATE TABLE trip_locations (trip_id TEXT NOT NULL, lat REAL, lng REAL, ts TEXT NOT NULL)`,
		`CREATE TABLE ride_requests (id TEXT PRIMARY KEY, status TEXT NOT NULL)`,
		`CREATE TABLE request_notifications (request_id TEXT NOT NULL, driver_user_id INTEGER NOT NULL, status TEXT, created_at TEXT NOT NULL)`,
		`CREATE TABLE rider_abuse_events (rider_user_id INTEGER NOT NULL, trip_id TEXT, created_at TEXT NOT NULL)`,
		`CREATE TABLE rider_login_codes (phone TEXT, consumed INTEGER NOT NULL DEFAULT 0, expires_at INTEGER NOT NULL)`,
		`CREATE TABLE driver_login_codes (user_id INTEGER, used INTEGER NOT NULL DEFAULT 0, expires_at TEXT NOT NULL)`,
		`CREATE TABLE rider_auth_sessions (user_id INTEGER, revoked INTEGER NOT NULL DEFAULT 0, refresh_expires_at INTEGER NOT NULL)`,
		`CREATE TABLE driver_auth_sessions (user_id INTEGER, revoked INTEGER NOT NULL DEFAULT 0, expires_at INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return db
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Old rows for closed trips go; recent rows and rows for live trips stay.
func TestRetentionSweep_TripLocations(t *testing.T) {
	db := setupRetentionDB(t)
	if _, err := db.Exec(`INSERT INTO trips (id, status) VALUES ('finished','FINISHED'), ('live','STARTED')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO trip_locations (trip_id, lat, lng, ts) VALUES
		('finished', 1, 1, datetime('now','-200 days')),
		('finished', 1, 1, datetime('now','-1 days')),
		('live',     1, 1, datetime('now','-200 days'))`); err != nil {
		t.Fatal(err)
	}

	runRetentionSweep(context.Background(), db)

	if got := count(t, db, "trip_locations"); got != 2 {
		t.Errorf("trip_locations = %d, want 2 (only the old finished-trip point removed)", got)
	}
	var live int
	if err := db.QueryRow(`SELECT COUNT(*) FROM trip_locations WHERE trip_id='live'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Errorf("points for an in-progress trip must never be deleted, got %d", live)
	}
}

func TestRetentionSweep_RequestNotifications(t *testing.T) {
	db := setupRetentionDB(t)
	if _, err := db.Exec(`INSERT INTO ride_requests (id, status) VALUES ('old','EXPIRED'), ('pending','PENDING')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status, created_at) VALUES
		('old',     1, 'TIMEOUT', datetime('now','-60 days')),
		('old',     2, 'TIMEOUT', datetime('now','-1 days')),
		('pending', 3, 'SENT',    datetime('now','-60 days'))`); err != nil {
		t.Fatal(err)
	}

	runRetentionSweep(context.Background(), db)

	if got := count(t, db, "request_notifications"); got != 2 {
		t.Errorf("request_notifications = %d, want 2", got)
	}
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_notifications WHERE request_id='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Error("offers for a still-PENDING request must not be deleted")
	}
}

func TestRetentionSweep_CredentialsAndSessions(t *testing.T) {
	db := setupRetentionDB(t)
	if _, err := db.Exec(`INSERT INTO rider_login_codes (phone, consumed, expires_at) VALUES
		('a', 1, strftime('%s','now') + 600),
		('b', 0, strftime('%s','now') - 600),
		('c', 0, strftime('%s','now') + 600)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO driver_auth_sessions (user_id, revoked, expires_at) VALUES
		(1, 1, strftime('%s','now') + 600),
		(2, 0, strftime('%s','now') - 600),
		(3, 0, strftime('%s','now') + 600)`); err != nil {
		t.Fatal(err)
	}

	runRetentionSweep(context.Background(), db)

	if got := count(t, db, "rider_login_codes"); got != 1 {
		t.Errorf("rider_login_codes = %d, want 1 (only the live unconsumed code)", got)
	}
	if got := count(t, db, "driver_auth_sessions"); got != 1 {
		t.Errorf("driver_auth_sessions = %d, want 1 (only the live session)", got)
	}
}

// A missing table on an un-migrated database must not stop the other sweeps.
func TestRetentionSweep_ToleratesMissingTables(t *testing.T) {
	db, err := sql.Open("sqlite", "file:retention_missing?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE rider_abuse_events (rider_user_id INTEGER, trip_id TEXT, created_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO rider_abuse_events (rider_user_id, trip_id, created_at) VALUES (1,'t',datetime('now','-60 days'))`); err != nil {
		t.Fatal(err)
	}

	runRetentionSweep(context.Background(), db) // must not panic

	if got := count(t, db, "rider_abuse_events"); got != 0 {
		t.Errorf("rider_abuse_events = %d, want 0 — later sweeps must still run", got)
	}
}

func TestRetentionEnvDaysZeroDisablesSweep(t *testing.T) {
	db := setupRetentionDB(t)
	if _, err := db.Exec(`INSERT INTO trips (id, status) VALUES ('finished','FINISHED')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO trip_locations (trip_id, lat, lng, ts) VALUES ('finished',1,1,datetime('now','-500 days'))`); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RETENTION_TRIP_LOCATIONS_DAYS", "0")
	runRetentionSweep(context.Background(), db)

	if got := count(t, db, "trip_locations"); got != 1 {
		t.Errorf("trip_locations = %d, want 1 — retention 0 must disable the sweep", got)
	}
}
