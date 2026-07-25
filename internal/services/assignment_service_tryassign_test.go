package services

import (
	"context"
	"database/sql"
	"testing"
	"time"

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

// A driver holding an active trip must not be given a second one. The request
// CAS only guarantees one driver per request; without an accept-time check the
// same driver could accept two different requests, and because the driver app
// surfaces a single assigned trip the second rider would wait indefinitely.
func TestTryAssign_DriverWithActiveTripIsNotAssignedAgain(t *testing.T) {
	ctx := context.Background()

	for _, active := range []string{domain.TripStatusWaiting, domain.TripStatusArrived, domain.TripStatusStarted} {
		t.Run(active, func(t *testing.T) {
			db := setupTryAssignTestDB(t)
			defer db.Close()

			// Driver 1 is already on a trip in this status.
			if _, err := db.Exec(`INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status)
				VALUES ('trip-existing', 'req-existing', 1, 998, ?1)`, active); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, expires_at)
				VALUES ('req-2', 999, 'PENDING', '2099-12-31 23:59:59')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status) VALUES ('req-2', 1, 'SENT')`); err != nil {
				t.Fatal(err)
			}

			svc := NewAssignmentService(db, nil, nil, &config.Config{})
			assigned, tripID, err := svc.TryAssign(ctx, "req-2", 1)
			if err != nil {
				t.Fatal(err)
			}
			if assigned || tripID != "" {
				t.Fatalf("assigned=%v tripID=%q, want false/empty for a driver already on a %s trip", assigned, tripID, active)
			}

			// The request must stay dispatchable for other drivers.
			var reqStatus string
			if err := db.QueryRow(`SELECT status FROM ride_requests WHERE id='req-2'`).Scan(&reqStatus); err != nil {
				t.Fatal(err)
			}
			if reqStatus != domain.RequestStatusPending {
				t.Fatalf("request status=%q, want PENDING so another driver can take it", reqStatus)
			}
			var trips int
			if err := db.QueryRow(`SELECT COUNT(*) FROM trips WHERE driver_user_id = 1`).Scan(&trips); err != nil {
				t.Fatal(err)
			}
			if trips != 1 {
				t.Fatalf("driver has %d trips, want 1", trips)
			}
		})
	}
}

// A driver whose previous trip is finished or cancelled is free to take a new one.
func TestTryAssign_DriverWithClosedTripCanBeAssigned(t *testing.T) {
	ctx := context.Background()

	for _, closed := range []string{domain.TripStatusFinished, domain.TripStatusCancelledByRider, domain.TripStatusCancelledByDriver} {
		t.Run(closed, func(t *testing.T) {
			db := setupTryAssignTestDB(t)
			defer db.Close()

			if _, err := db.Exec(`INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status)
				VALUES ('trip-old', 'req-old', 1, 998, ?1)`, closed); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, expires_at)
				VALUES ('req-3', 999, 'PENDING', '2099-12-31 23:59:59')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status) VALUES ('req-3', 1, 'SENT')`); err != nil {
				t.Fatal(err)
			}

			svc := NewAssignmentService(db, nil, nil, &config.Config{})
			assigned, tripID, err := svc.TryAssign(ctx, "req-3", 1)
			if err != nil {
				t.Fatal(err)
			}
			if !assigned || tripID == "" {
				t.Fatalf("assigned=%v tripID=%q, want a new trip after a %s trip", assigned, tripID, closed)
			}
		})
	}
}

// The shipped defaults made radius expansion unreachable: a 5-minute delay
// against a 120-second request TTL, with the query also requiring the request to
// still be unexpired. The delay must be clamped inside the window.
func TestRadiusExpansionWindow_ClampedToRequestTTL(t *testing.T) {
	cases := []struct {
		name         string
		ttlSeconds   int
		expansionMin int
		wantTTL      time.Duration
		wantDelay    time.Duration
	}{
		{"shipped defaults are clamped", 120, 5, 120 * time.Second, 48 * time.Second},
		{"unset expansion uses 40% of ttl", 120, 0, 120 * time.Second, 48 * time.Second},
		{"a delay inside the window is honoured", 600, 1, 600 * time.Second, time.Minute},
		{"ttl unset falls back to 120s", 0, 5, 120 * time.Second, 48 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &AssignmentService{cfg: &config.Config{
				RequestExpiresSeconds:  tc.ttlSeconds,
				RadiusExpansionMinutes: tc.expansionMin,
			}}
			ttl, delay := s.radiusExpansionWindow()
			if ttl != tc.wantTTL {
				t.Errorf("ttl = %s, want %s", ttl, tc.wantTTL)
			}
			if delay != tc.wantDelay {
				t.Errorf("delay = %s, want %s", delay, tc.wantDelay)
			}
			if delay >= ttl {
				t.Errorf("delay %s must be strictly inside the %s window or expansion can never fire", delay, ttl)
			}
		})
	}
}

// Abandoned requests (pickup sent, destination never confirmed) must be retired,
// otherwise the one-pending-per-rider rule locks the rider out permanently.
func TestExpireAbandonedRequests(t *testing.T) {
	db := setupTryAssignTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := db.Exec(`ALTER TABLE ride_requests ADD COLUMN destination_confirmed INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE ride_requests ADD COLUMN created_at TEXT NOT NULL DEFAULT (datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, expires_at, destination_confirmed, created_at) VALUES
		('abandoned', 1, 'PENDING', '2099-12-31 23:59:59', 0, datetime('now','-2 hours')),
		('browsing',  2, 'PENDING', '2099-12-31 23:59:59', 0, datetime('now')),
		('confirmed', 3, 'PENDING', '2099-12-31 23:59:59', 1, datetime('now','-2 hours'))`); err != nil {
		t.Fatal(err)
	}

	svc := NewAssignmentService(db, nil, nil, &config.Config{})
	svc.expireAbandonedRequests(ctx)

	statusOf := func(id string) string {
		var st string
		if err := db.QueryRow(`SELECT status FROM ride_requests WHERE id = ?1`, id).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if got := statusOf("abandoned"); got != domain.RequestStatusExpired {
		t.Errorf("stale unconfirmed request status = %q, want EXPIRED — otherwise the rider stays locked out", got)
	}
	if got := statusOf("browsing"); got != domain.RequestStatusPending {
		t.Errorf("a rider still choosing a destination must not be cut off, got %q", got)
	}
	if got := statusOf("confirmed"); got != domain.RequestStatusPending {
		t.Errorf("confirmed requests are handled by the normal expiry path, got %q", got)
	}
}
