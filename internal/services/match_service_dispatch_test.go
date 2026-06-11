package services

import (
	"context"
	"database/sql"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"

	_ "modernc.org/sqlite"
)

func setupMatchDispatchTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:match_dispatch_"+t.Name()+"?mode=memory&cache=shared")
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
		role TEXT NOT NULL,
		telegram_id INTEGER NOT NULL DEFAULT 0,
		phone TEXT
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
	exec(`CREATE TABLE drivers (
		user_id INTEGER PRIMARY KEY,
		last_lat REAL,
		last_lng REAL,
		app_lat REAL,
		app_lng REAL,
		app_last_seen_at TEXT,
		app_location_active INTEGER NOT NULL DEFAULT 0,
		is_active INTEGER NOT NULL DEFAULT 0,
		manual_offline INTEGER NOT NULL DEFAULT 0,
		last_seen_at TEXT,
		last_live_location_at TEXT,
		live_location_active INTEGER NOT NULL DEFAULT 0,
		verification_status TEXT NOT NULL DEFAULT 'approved',
		phone TEXT,
		car_type TEXT,
		color TEXT,
		plate TEXT,
		balance INTEGER NOT NULL DEFAULT 1000
	)`)
	exec(`CREATE TABLE ride_requests (
		id TEXT PRIMARY KEY,
		rider_user_id INTEGER NOT NULL,
		pickup_lat REAL NOT NULL,
		pickup_lng REAL NOT NULL,
		radius_km REAL NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		expires_at TEXT NOT NULL,
		drop_lat REAL,
		drop_lng REAL,
		estimated_price INTEGER NOT NULL DEFAULT 0,
		destination_confirmed INTEGER NOT NULL DEFAULT 0
	)`)
	exec(`CREATE TABLE trips (
		id TEXT PRIMARY KEY,
		request_id TEXT UNIQUE NOT NULL,
		driver_user_id INTEGER NOT NULL,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL
	)`)
	exec(`CREATE TABLE request_notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL,
		driver_user_id INTEGER NOT NULL,
		chat_id INTEGER NOT NULL,
		message_id INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'SENT',
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	return db
}

// BroadcastRequest must keep dispatch alive after the caller's HTTP context is canceled
// (native rider app confirm). Regression: bot used context.Background(), app used request ctx.
func TestBroadcastRequest_SurvivesCanceledCallerContext(t *testing.T) {
	db := setupMatchDispatchTestDB(t)
	defer db.Close()

	const requestID = "req-cancel-ctx"
	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, ?, 1001, '+998901112233')`, domain.RoleRider)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (2, ?, 2002, '+998902223344')`, domain.RoleDriver)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO legal_documents (document_type, version, is_active, content) VALUES
		('driver_terms', 1, 1, 'x'),
		('privacy_policy_driver', 1, 1, 'y')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO legal_acceptances (user_id, document_type, version) VALUES
		(2, 'driver_terms', 1), (2, 'privacy_policy_driver', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err = db.Exec(`INSERT INTO drivers (
		user_id, last_lat, last_lng, app_lat, app_lng, app_last_seen_at, app_location_active,
		is_active, last_seen_at, phone, car_type, color, plate, balance, verification_status
	) VALUES (2, 41.30, 69.28, 41.30, 69.28, ?1, 1, 1, ?1, '+998902223344', 'sedan', 'white', '01A001AA', 1000, 'approved')`, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO ride_requests (
		id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at,
		drop_lat, drop_lng, estimated_price, destination_confirmed
	) VALUES (?1, 1, 41.30, 69.28, 3, ?2, datetime('now','+1 hour'), 41.31, 69.29, 5000, 1)`,
		requestID, domain.RequestStatusPending)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{InfiniteDriverBalance: true, DispatchWaitSeconds: 1}
	matchSvc := NewMatchService(db, (*tgbotapi.BotAPI)(nil), cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := matchSvc.BroadcastRequest(ctx, requestID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM request_notifications WHERE request_id = ?1 AND driver_user_id = 2`, requestID).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("expected request_notifications row after BroadcastRequest with canceled caller context")
}
