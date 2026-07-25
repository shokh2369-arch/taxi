package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"

	_ "modernc.org/sqlite"
)

func setupDispatchContractDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:dispatch_contract?mode=memory&cache=shared")
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
	);`)
	exec(`CREATE TABLE drivers (
		user_id INTEGER PRIMARY KEY,
		last_lat REAL,
		last_lng REAL,
		last_seen_at TEXT
	);`)
	exec(`CREATE TABLE ride_requests (
		id TEXT PRIMARY KEY,
		rider_user_id INTEGER NOT NULL,
		pickup_lat REAL NOT NULL,
		pickup_lng REAL NOT NULL,
		radius_km REAL NOT NULL,
		status TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		assigned_driver_user_id INTEGER,
		assigned_at TEXT
	);`)
	exec(`CREATE TABLE request_notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL,
		driver_user_id INTEGER NOT NULL,
		chat_id INTEGER NOT NULL,
		message_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);`)
	exec(`CREATE TABLE trips (
		id TEXT PRIMARY KEY,
		request_id TEXT NOT NULL,
		driver_user_id INTEGER NOT NULL,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL
	);`)
	exec(`CREATE TABLE legal_documents (
		document_type TEXT NOT NULL,
		version INTEGER NOT NULL,
		is_active INTEGER NOT NULL DEFAULT 1,
		content TEXT
	);`)
	exec(`CREATE TABLE legal_acceptances (
		user_id INTEGER NOT NULL,
		document_type TEXT NOT NULL,
		version INTEGER NOT NULL,
		PRIMARY KEY (user_id, document_type)
	);`)
	return db
}

func seedDriverLegalOK(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO legal_documents (document_type, version, is_active, content) VALUES
		('driver_terms', 1, 1, 'x'),
		('privacy_policy_driver', 1, 1, 'y')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO legal_acceptances (user_id, document_type, version) VALUES (?1, 'driver_terms', 1), (?1, 'privacy_policy_driver', 1)`, userID)
	if err != nil {
		t.Fatal(err)
	}
}

func injectDriverContext(c *gin.Context, userID int64) {
	c.Request = c.Request.WithContext(auth.WithUser(c.Request.Context(), &auth.User{
		UserID: userID,
		Role:   domain.RoleDriver,
	}))
}

// TestDriverAvailableRequests_JSONContract locks the JSON shape for GET /driver/available-requests.
func TestDriverAvailableRequests_JSONContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupDispatchContractDB(t)
	defer db.Close()

	const driverID int64 = 1
	_, _ = db.Exec(`INSERT INTO users (id, telegram_id, role) VALUES (1, 100, 'driver'), (2, 200, 'rider')`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, last_lat, last_lng, last_seen_at) VALUES (1, 41.0, 69.0, '2026-01-01 12:00:00')`)
	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at) VALUES
		('req-offer', 2, 41.01, 69.01, 3.0, 'PENDING', '2099-12-31 23:59:59')`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, chat_id, message_id, status) VALUES ('req-offer', 1, 0, 0, 'SENT')`)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/driver/available-requests", nil)
	req = req.WithContext(context.Background())
	c.Request = req
	injectDriverContext(c, driverID)

	DriverAvailableRequests(db, nil, nil)(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	aliases := []string{"available_requests", "requests", "pending_requests", "queue", "orders", "jobs"}
	var first []byte
	for _, k := range aliases {
		b, ok := resp[k]
		if !ok {
			t.Fatalf("missing key %q", k)
		}
		if first == nil {
			first = b
		} else if string(first) != string(b) {
			t.Fatalf("alias %q differs from available_requests", k)
		}
	}

	var offers []map[string]interface{}
	if err := json.Unmarshal(first, &offers); err != nil {
		t.Fatal(err)
	}
	if len(offers) != 1 {
		t.Fatalf("len(offers) = %d", len(offers))
	}
	o := offers[0]
	for _, k := range []string{"request_id", "pickup_lat", "pickup_lng", "distance_km", "radius_km"} {
		if _, ok := o[k]; !ok {
			t.Fatalf("offer missing %q", k)
		}
	}
	if o["request_id"] != "req-offer" {
		t.Fatalf("request_id = %v", o["request_id"])
	}

	if string(resp["assigned_trip"]) != "null" {
		t.Fatalf("assigned_trip want null, got %s", resp["assigned_trip"])
	}

	// Second case: active trip stub present
	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at) VALUES
		('req-trip', 2, 41.0, 69.0, 3.0, 'ASSIGNED', '2099-12-31 23:59:59')`)
	_, _ = db.Exec(`INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status) VALUES ('trip-active', 'req-trip', 1, 2, 'WAITING')`)

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	req2 := httptest.NewRequest(http.MethodGet, "/driver/available-requests", nil)
	c2.Request = req2
	injectDriverContext(c2, driverID)

	DriverAvailableRequests(db, nil, nil)(c2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d", w2.Code)
	}
	var resp2 map[string]interface{}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	at, ok := resp2["assigned_trip"].(map[string]interface{})
	if !ok {
		t.Fatalf("assigned_trip = %#v", resp2["assigned_trip"])
	}
	if at["trip_id"] != "trip-active" || at["status"] != "WAITING" {
		t.Fatalf("assigned_trip = %#v", at)
	}
}

// TestDriverAcceptRequest_JSONContract locks HTTP status + JSON bodies for POST /driver/accept-request.
func TestDriverAcceptRequest_JSONContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("trip_id_only_already_assigned", func(t *testing.T) {
		db := setupDispatchContractDB(t)
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO users (id, telegram_id) VALUES (1, 100), (2, 200)`)
		_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at) VALUES
			('req-1', 2, 41.0, 69.0, 3.0, 'ASSIGNED', '2099-12-31 23:59:59')`)
		_, _ = db.Exec(`INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status) VALUES ('t-1', 'req-1', 1, 2, 'WAITING')`)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		body := bytes.NewBufferString(`{"trip_id":"t-1"}`)
		c.Request = httptest.NewRequest(http.MethodPost, "/driver/accept-request", body)
		c.Request.Header.Set("Content-Type", "application/json")
		injectDriverContext(c, 1)

		DriverAcceptRequest(db, nil, nil, nil, nil)(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var got map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["ok"] != true || got["result"] != "already_assigned" || got["trip_id"] != "t-1" || got["status"] != "WAITING" {
			t.Fatalf("body = %#v", got)
		}
	})

	t.Run("request_id_assignment_unavailable", func(t *testing.T) {
		db := setupDispatchContractDB(t)
		defer db.Close()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/driver/accept-request", bytes.NewBufferString(`{"request_id":"any"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		injectDriverContext(c, 1)

		DriverAcceptRequest(db, nil, nil, nil, nil)(c)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", w.Code)
		}
	})

	t.Run("conflict_request_not_available", func(t *testing.T) {
		db := setupDispatchContractDB(t)
		defer db.Close()
		seedDriverLegalOK(t, db, 1)
		_, _ = db.Exec(`INSERT INTO users (id, telegram_id) VALUES (1, 100)`)
		_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at) VALUES
			('req-exp', 999, 41.0, 69.0, 3.0, 'PENDING', '2000-01-01 00:00:00')`)
		_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, chat_id, message_id, status) VALUES ('req-exp', 1, 0, 0, 'SENT')`)

		assignSvc := services.NewAssignmentService(db, nil, nil, &config.Config{})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/driver/accept-request", bytes.NewBufferString(`{"request_id":"req-exp"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		injectDriverContext(c, 1)

		DriverAcceptRequest(db, assignSvc, nil, nil, nil)(c)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var got map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got["ok"] != false || got["error"] != "request no longer available" || got["request_id"] != "req-exp" {
			t.Fatalf("body = %#v", got)
		}
	})

	t.Run("success_try_assign", func(t *testing.T) {
		db := setupDispatchContractDB(t)
		defer db.Close()
		seedDriverLegalOK(t, db, 1)
		_, _ = db.Exec(`INSERT INTO users (id, telegram_id) VALUES (1, 100)`)
		// rider_user_id has no users row so TryAssign skips Telegram sends (nil bots in test).
		_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at) VALUES
			('req-ok', 999, 41.0, 69.0, 3.0, 'PENDING', '2099-12-31 23:59:59')`)
		_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, chat_id, message_id, status) VALUES ('req-ok', 1, 0, 0, 'SENT')`)

		assignSvc := services.NewAssignmentService(db, nil, nil, &config.Config{})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/driver/accept-request", bytes.NewBufferString(`{"request_id":"req-ok"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		injectDriverContext(c, 1)

		DriverAcceptRequest(db, assignSvc, nil, nil, nil)(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var got map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["ok"] != true || got["assigned"] != true || got["request_id"] != "req-ok" {
			t.Fatalf("body = %#v", got)
		}
		tid, _ := got["trip_id"].(string)
		if tid == "" {
			t.Fatal("missing trip_id")
		}
		if _, err := uuid.Parse(tid); err != nil {
			t.Fatalf("trip_id not uuid: %q", tid)
		}
	})

	t.Run("missing_body_fields", func(t *testing.T) {
		db := setupDispatchContractDB(t)
		defer db.Close()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/driver/accept-request", bytes.NewBufferString(`{}`))
		c.Request.Header.Set("Content-Type", "application/json")
		injectDriverContext(c, 1)
		DriverAcceptRequest(db, nil, nil, nil, nil)(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", w.Code)
		}
	})
}

// The reported "button changes then reverts" bug.
//
// assigned_trip was selected with LIMIT 1 and no ORDER BY, so when a driver held
// more than one active trip SQLite could return either one — and the status the
// app read flipped between polls. It was never replica lag: there is a single
// Turso primary and no read cache. Ordering makes the answer stable, and
// preferring the furthest-progressed trip means a just-tapped ARRIVED never reads
// back as WAITING.
func TestDriverAvailableRequests_AssignedTripIsStableAcrossPolls(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupDispatchContractDB(t)
	defer db.Close()

	// Legacy state: the same driver on two active trips at once.
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, pickup_lat, pickup_lng, radius_km, expires_at)
		VALUES ('req-a', 55, 'ASSIGNED', 41.30, 69.28, 3, '2099-01-01 00:00:00'),
		       ('req-b', 56, 'ASSIGNED', 41.31, 69.29, 3, '2099-01-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status) VALUES
		('trip-waiting', 'req-a', 1, 55, 'WAITING'),
		('trip-arrived', 'req-b', 1, 56, 'ARRIVED')`); err != nil {
		t.Fatal(err)
	}

	_, _ = db.Exec(`INSERT INTO users (id, telegram_id, role) VALUES (1, 100, 'driver'), (55, 550, 'rider'), (56, 560, 'rider')`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, last_lat, last_lng, last_seen_at) VALUES (1, 41.0, 69.0, '2026-01-01 12:00:00')`)

	poll := func() map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/driver/available-requests", nil).WithContext(context.Background())
		injectDriverContext(c, 1)
		DriverAvailableRequests(db, nil, nil)(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return body
	}

	var firstTrip, firstStatus string
	for i := 0; i < 8; i++ {
		body := poll()
		assigned, ok := body["assigned_trip"].(map[string]any)
		if !ok || assigned == nil {
			t.Fatalf("poll %d: assigned_trip missing: %v", i, body["assigned_trip"])
		}
		tripID, _ := assigned["trip_id"].(string)
		status, _ := assigned["status"].(string)
		if i == 0 {
			firstTrip, firstStatus = tripID, status
			continue
		}
		if tripID != firstTrip || status != firstStatus {
			t.Fatalf("poll %d returned %s/%s, first poll returned %s/%s — assigned_trip must not flip between polls",
				i, tripID, status, firstTrip, firstStatus)
		}
	}

	// The furthest-progressed trip wins, so a tapped ARRIVED is never masked by a
	// stale WAITING row.
	if firstStatus != "ARRIVED" {
		t.Errorf("assigned_trip.status = %q, want ARRIVED (the further-progressed trip)", firstStatus)
	}
}

// Conditional requests: this endpoint is polled every few seconds per online
// driver and is usually unchanged, so an idle poll should cost a 304 not a body.
func TestDriverAvailableRequests_ETagReturns304WhenUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupDispatchContractDB(t)
	defer db.Close()

	_, _ = db.Exec(`INSERT INTO users (id, telegram_id, role) VALUES (1, 100, 'driver')`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, last_lat, last_lng, last_seen_at) VALUES (1, 41.0, 69.0, '2026-01-01 12:00:00')`)

	call := func(ifNoneMatch string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		req := httptest.NewRequest(http.MethodGet, "/driver/available-requests", nil).WithContext(context.Background())
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		c.Request = req
		injectDriverContext(c, 1)
		DriverAvailableRequests(db, nil, nil)(c)
		return w
	}

	first := call("")
	if first.Code != http.StatusOK {
		t.Fatalf("first call status = %d", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag header on the first response")
	}

	second := call(etag)
	if second.Code != http.StatusNotModified {
		t.Fatalf("unchanged poll status = %d, want 304 (body was %d bytes)", second.Code, second.Body.Len())
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 must have an empty body, got %d bytes", second.Body.Len())
	}

	// A real change must break the ETag.
	_, _ = db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at)
		VALUES ('req-new', 2, 41.01, 69.01, 3.0, 'PENDING', '2099-12-31 23:59:59')`)
	_, _ = db.Exec(`INSERT INTO request_notifications (request_id, driver_user_id, chat_id, message_id, status)
		VALUES ('req-new', 1, 0, 0, 'SENT')`)

	third := call(etag)
	if third.Code != http.StatusOK {
		t.Fatalf("after a new offer the poll must return 200, got %d", third.Code)
	}
}
