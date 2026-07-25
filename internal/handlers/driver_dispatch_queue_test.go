package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/config"

	_ "modernc.org/sqlite"
)

func setupDispatchQueueHandlerDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:dispatch_queue_handler_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, telegram_id INTEGER NOT NULL DEFAULT 0, role TEXT NOT NULL DEFAULT 'driver')`)
	exec(`CREATE TABLE drivers (
		user_id INTEGER PRIMARY KEY,
		last_lat REAL, last_lng REAL,
		app_lat REAL, app_lng REAL,
		app_last_seen_at TEXT, app_location_active INTEGER NOT NULL DEFAULT 0,
		last_seen_at TEXT
	)`)
	exec(`CREATE TABLE ride_requests (
		id TEXT PRIMARY KEY,
		rider_user_id INTEGER NOT NULL,
		pickup_lat REAL NOT NULL,
		pickup_lng REAL NOT NULL,
		radius_km REAL NOT NULL,
		status TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		estimated_price INTEGER NOT NULL DEFAULT 0
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
		request_id TEXT NOT NULL,
		driver_user_id INTEGER NOT NULL,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL
	)`)
	return db
}

func TestDriverAvailableRequests_OfferPersistsWhilePending(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupDispatchQueueHandlerDB(t)
	defer db.Close()

	const driverID int64 = 1
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, _ = db.Exec(`INSERT INTO users (id, role) VALUES (1, 'driver'), (2, 'rider')`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, last_lat, last_lng, app_lat, app_lng, app_last_seen_at, app_location_active, last_seen_at)
		VALUES (1, 41.0, 69.0, 41.0, 69.0, ?1, 1, ?1)`, now)
	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at, estimated_price)
		VALUES ('req-persist', 2, 41.01, 69.01, 3.0, 'PENDING', '2099-12-31 23:59:59', 12000)`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status, created_at)
		VALUES ('req-persist', 1, 'SENT', datetime('now','-30 seconds'))`)

	cfg := &config.Config{DispatchOfferVisibleSeconds: 90}
	handler := DriverAvailableRequests(db, cfg, nil)

	var lastRequestID string
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		req := httptest.NewRequest(http.MethodGet, "/driver/available-requests", nil)
		c.Request = req
		injectDriverContext(c, driverID)
		handler(c)
		if w.Code != http.StatusOK {
			t.Fatalf("poll %d: status=%d body=%s", i, w.Code, w.Body.String())
		}
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		var offers []map[string]interface{}
		if err := json.Unmarshal(resp["queue"], &offers); err != nil {
			t.Fatal(err)
		}
		if len(offers) != 1 {
			t.Fatalf("poll %d: len(queue)=%d", i, len(offers))
		}
		rid, _ := offers[0]["request_id"].(string)
		if rid != "req-persist" {
			t.Fatalf("poll %d: request_id=%q", i, rid)
		}
		if lastRequestID != "" && lastRequestID != rid {
			t.Fatalf("request_id changed between polls: %q -> %q", lastRequestID, rid)
		}
		lastRequestID = rid
		if exp, ok := offers[0]["expires_at"].(string); !ok || exp == "" {
			t.Fatalf("poll %d: missing expires_at RFC3339", i)
		}
		if _, ok := offers[0]["estimated_price"]; !ok {
			t.Fatalf("poll %d: missing estimated_price", i)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Offers older than DispatchOfferVisibleSeconds must not be returned even while the request is PENDING.
func TestDriverAvailableRequests_HidesOffersPastVisibilityWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupDispatchQueueHandlerDB(t)
	defer db.Close()

	const driverID int64 = 1
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, _ = db.Exec(`INSERT INTO users (id, role) VALUES (1, 'driver'), (2, 'rider')`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, last_lat, last_lng, app_lat, app_lng, app_last_seen_at, app_location_active, last_seen_at)
		VALUES (1, 41.0, 69.0, 41.0, 69.0, ?1, 1, ?1)`, now)
	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at, estimated_price)
		VALUES ('req-stale', 2, 41.01, 69.01, 3.0, 'PENDING', '2099-12-31 23:59:59', 12000)`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status, created_at)
		VALUES ('req-stale', 1, 'SENT', datetime('now','-120 seconds'))`)

	cfg := &config.Config{DispatchOfferVisibleSeconds: 90}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/driver/available-requests", nil)
	injectDriverContext(c, driverID)
	DriverAvailableRequests(db, cfg, nil)(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var offers []map[string]interface{}
	if err := json.Unmarshal(resp["queue"], &offers); err != nil {
		t.Fatal(err)
	}
	if len(offers) != 0 {
		t.Fatalf("len(queue)=%d, want 0 (offer is past the visibility window)", len(offers))
	}
}

func TestDriverAvailableRequests_PrecreatedTripStaysInQueue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupDispatchQueueHandlerDB(t)
	defer db.Close()

	_, _ = db.Exec(`INSERT INTO users (id, role) VALUES (1, 'driver'), (2, 'rider')`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, last_lat, last_lng) VALUES (1, 41.0, 69.0)`)
	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at, estimated_price)
		VALUES ('req-trip', 2, 41.0, 69.0, 3.0, 'PENDING', '2099-12-31 23:59:59', 5000)`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, status) VALUES ('req-trip', 1, 'SENT')`)
	_, _ = db.Exec(`INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status) VALUES ('trip-pre', 'req-trip', 1, 2, 'WAITING')`)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/driver/available-requests", nil)
	c.Request = c.Request.WithContext(context.Background())
	injectDriverContext(c, 1)
	DriverAvailableRequests(db, nil, nil)(c)

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	queue, _ := resp["queue"].([]interface{})
	if len(queue) != 1 {
		t.Fatalf("len(queue)=%d", len(queue))
	}
	item := queue[0].(map[string]interface{})
	if item["trip_id"] != "trip-pre" {
		t.Fatalf("trip_id=%v", item["trip_id"])
	}
	if resp["assigned_trip"] != nil {
		t.Fatalf("assigned_trip should be null while request PENDING, got %#v", resp["assigned_trip"])
	}
}
