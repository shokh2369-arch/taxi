package services

import (
	"context"
	"database/sql"
	"testing"

	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"

	_ "modernc.org/sqlite"
)

func setupTryAssignTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:try_assign_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		telegram_id INTEGER NOT NULL DEFAULT 0,
		role TEXT NOT NULL DEFAULT 'driver'
	)`)
	exec(`CREATE TABLE legal_documents (
		document_type TEXT NOT NULL,
		version INTEGER NOT NULL,
		is_active INTEGER NOT NULL DEFAULT 1,
		content TEXT,
		PRIMARY KEY (document_type, version)
	)`)
	exec(`CREATE TABLE legal_acceptances (
		user_id INTEGER NOT NULL,
		document_type TEXT NOT NULL,
		version INTEGER NOT NULL,
		PRIMARY KEY (user_id, document_type)
	)`)
	exec(`CREATE TABLE ride_requests (
		id TEXT PRIMARY KEY,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		assigned_driver_user_id INTEGER,
		assigned_at TEXT
	)`)
	exec(`CREATE TABLE request_notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL,
		driver_user_id INTEGER NOT NULL,
		chat_id INTEGER NOT NULL DEFAULT 0,
		message_id INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	exec(`CREATE TABLE trips (
		id TEXT PRIMARY KEY,
		request_id TEXT UNIQUE NOT NULL,
		driver_user_id INTEGER NOT NULL,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL
	)`)
	// Driver legal acceptances so TryAssign passes the legal gate.
	exec(`INSERT INTO legal_documents (document_type, version, is_active, content) VALUES
		('driver_terms', 1, 1, 'x'),
		('privacy_policy_driver', 1, 1, 'y')`)
	exec(`INSERT INTO legal_acceptances (user_id, document_type, version) VALUES
		(1, 'driver_terms', 1), (1, 'privacy_policy_driver', 1)`)
	exec(`INSERT INTO users (id, telegram_id, role) VALUES (1, 100, 'driver')`)
	return db
}

func TestTryAssign_Success(t *testing.T) {
	db := setupTryAssignTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// rider_user_id 999 has no users row so Telegram notification paths are skipped (nil bots).
	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, expires_at)
		VALUES ('req-1', 999, 'PENDING', '2099-12-31 23:59:59')`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status) VALUES ('req-1', 1, 'SENT')`)

	svc := NewAssignmentService(db, nil, nil, &config.Config{})
	assigned, tripID, err := svc.TryAssign(ctx, "req-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !assigned || tripID == "" {
		t.Fatalf("assigned=%v tripID=%q", assigned, tripID)
	}

	var reqStatus string
	var assignedDriver sql.NullInt64
	if err := db.QueryRow(`SELECT status, assigned_driver_user_id FROM ride_requests WHERE id='req-1'`).Scan(&reqStatus, &assignedDriver); err != nil {
		t.Fatal(err)
	}
	if reqStatus != domain.RequestStatusAssigned || !assignedDriver.Valid || assignedDriver.Int64 != 1 {
		t.Fatalf("request status=%q assigned_driver=%v", reqStatus, assignedDriver)
	}
	var tripStatus string
	if err := db.QueryRow(`SELECT status FROM trips WHERE id=?1`, tripID).Scan(&tripStatus); err != nil {
		t.Fatal(err)
	}
	if tripStatus != domain.TripStatusWaiting {
		t.Fatalf("trip status=%q", tripStatus)
	}
	var notifStatus string
	if err := db.QueryRow(`SELECT status FROM request_notifications WHERE request_id='req-1' AND driver_user_id=1`).Scan(&notifStatus); err != nil {
		t.Fatal(err)
	}
	if notifStatus != domain.NotificationStatusAccepted {
		t.Fatalf("notification status=%q", notifStatus)
	}
}

// TestTryAssign_TripInsertFailureRollsBackAssignment: if the trips INSERT fails, the ride_requests
// ASSIGNED update and the ACCEPTED notification must roll back (single transaction), so the request
// stays dispatchable instead of being stuck ASSIGNED without a trip.
func TestTryAssign_TripInsertFailureRollsBackAssignment(t *testing.T) {
	db := setupTryAssignTestDB(t)
	defer db.Close()
	ctx := context.Background()

	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, expires_at)
		VALUES ('req-fail', 999, 'PENDING', '2099-12-31 23:59:59')`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status) VALUES ('req-fail', 1, 'SENT')`)
	// Force the trips INSERT inside TryAssign to fail.
	if _, err := db.Exec(`DROP TABLE trips`); err != nil {
		t.Fatal(err)
	}

	svc := NewAssignmentService(db, nil, nil, &config.Config{})
	assigned, tripID, err := svc.TryAssign(ctx, "req-fail", 1)
	if err == nil {
		t.Fatal("expected error from failing trips insert")
	}
	if assigned || tripID != "" {
		t.Fatalf("assigned=%v tripID=%q, want false/empty", assigned, tripID)
	}

	var reqStatus string
	if err := db.QueryRow(`SELECT status FROM ride_requests WHERE id='req-fail'`).Scan(&reqStatus); err != nil {
		t.Fatal(err)
	}
	if reqStatus != domain.RequestStatusPending {
		t.Fatalf("request status=%q, want PENDING (rolled back)", reqStatus)
	}
	var notifStatus string
	if err := db.QueryRow(`SELECT status FROM request_notifications WHERE request_id='req-fail' AND driver_user_id=1`).Scan(&notifStatus); err != nil {
		t.Fatal(err)
	}
	if notifStatus != domain.NotificationStatusSent {
		t.Fatalf("notification status=%q, want SENT (rolled back)", notifStatus)
	}
}
