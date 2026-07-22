package services

import (
	"context"
	"database/sql"
	"testing"

	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"

	_ "modernc.org/sqlite"
)

func setupDispatchQueueTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:dispatch_queue_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE ride_requests (
		id TEXT PRIMARY KEY,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		expires_at TEXT NOT NULL
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
	return db
}

func TestCloseDriverQueueOffersExcept_LogsAcceptedByOther(t *testing.T) {
	db := setupDispatchQueueTestDB(t)
	defer db.Close()
	ctx := context.Background()

	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, expires_at) VALUES ('r1', 1, 'ASSIGNED', '2099-01-01')`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status) VALUES ('r1', 10, 'SENT'), ('r1', 20, 'SENT')`)

	n := CloseDriverQueueOffersExcept(ctx, db, "r1", 10, "accepted_by_other", false)
	if n != 1 {
		t.Fatalf("closed = %d, want 1", n)
	}
	var st string
	if err := db.QueryRow(`SELECT status FROM request_notifications WHERE request_id='r1' AND driver_user_id=20`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != domain.NotificationStatusTimeout {
		t.Fatalf("other driver status = %q", st)
	}
	if err := db.QueryRow(`SELECT status FROM request_notifications WHERE request_id='r1' AND driver_user_id=10`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != domain.NotificationStatusAccepted && st != domain.NotificationStatusSent {
		t.Fatalf("accepter status = %q", st)
	}
}

func TestExpireStaleDriverQueueOffers_ClosesWhenRequestExpired(t *testing.T) {
	db := setupDispatchQueueTestDB(t)
	defer db.Close()
	ctx := context.Background()

	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, expires_at) VALUES ('r-exp', 1, 'PENDING', '2000-01-01')`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status) VALUES ('r-exp', 5, 'SENT')`)

	n := ExpireStaleDriverQueueOffers(ctx, db, &config.Config{})
	if n != 1 {
		t.Fatalf("closed = %d, want 1", n)
	}
}

// Offers older than DispatchOfferVisibleSeconds must be closed even while the request stays PENDING.
func TestExpireStaleDriverQueueOffers_ClosesOffersPastVisibilityWindow(t *testing.T) {
	db := setupDispatchQueueTestDB(t)
	defer db.Close()
	ctx := context.Background()

	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, expires_at) VALUES ('r-vis', 1, 'PENDING', '2099-01-01')`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status, created_at)
		VALUES ('r-vis', 5, 'SENT', datetime('now', '-120 seconds')),
		       ('r-vis', 6, 'SENT', datetime('now', '-10 seconds'))`)

	n := ExpireStaleDriverQueueOffers(ctx, db, &config.Config{DispatchOfferVisibleSeconds: 90})
	if n != 1 {
		t.Fatalf("closed = %d, want 1 (only the 120s-old offer)", n)
	}
	var st string
	if err := db.QueryRow(`SELECT status FROM request_notifications WHERE request_id='r-vis' AND driver_user_id=5`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != domain.NotificationStatusTimeout {
		t.Fatalf("old offer status = %q, want TIMEOUT", st)
	}
	if err := db.QueryRow(`SELECT status FROM request_notifications WHERE request_id='r-vis' AND driver_user_id=6`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != domain.NotificationStatusSent {
		t.Fatalf("fresh offer status = %q, want SENT", st)
	}
}
