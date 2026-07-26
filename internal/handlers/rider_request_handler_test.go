package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/repositories"
	"taxi-mvp/internal/services"

	_ "modernc.org/sqlite"
)

func setupRiderRequestHandlerDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared")
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
		content TEXT
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
		pickup_lat REAL NOT NULL,
		pickup_lng REAL NOT NULL,
		radius_km REAL NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		expires_at TEXT NOT NULL,
		assigned_driver_user_id INTEGER,
		assigned_at TEXT,
		drop_lat REAL,
		drop_lng REAL,
		drop_name TEXT,
		estimated_price INTEGER NOT NULL DEFAULT 0,
		destination_confirmed INTEGER NOT NULL DEFAULT 0,
		pickup_grid TEXT,
		radius_expanded_at TEXT
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
	exec(`CREATE TABLE trips (
		id TEXT PRIMARY KEY,
		request_id TEXT UNIQUE NOT NULL,
		driver_user_id INTEGER NOT NULL,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		started_at TEXT,
		finished_at TEXT,
		distance_m INTEGER NOT NULL DEFAULT 0,
		fare_amount INTEGER NOT NULL DEFAULT 0
	)`)
	exec(`CREATE TABLE rider_login_codes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		phone      TEXT NOT NULL,
		code_hash  TEXT NOT NULL,
		salt       TEXT NOT NULL,
		expires_at INTEGER NOT NULL,
		attempts   INTEGER NOT NULL DEFAULT 0,
		consumed   INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
	)`)
	exec(`CREATE TABLE rider_auth_sessions (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id            INTEGER NOT NULL,
		refresh_hash       TEXT NOT NULL UNIQUE,
		refresh_expires_at INTEGER NOT NULL,
		revoked            INTEGER NOT NULL DEFAULT 0,
		created_at         INTEGER NOT NULL DEFAULT (strftime('%s','now'))
	)`)
	return db
}

func seedRiderLegalAndUser(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO legal_documents (document_type, version, is_active, content) VALUES
		('user_terms', 1, 1, 'x'),
		('privacy_policy_user', 1, 1, 'y')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO legal_acceptances (user_id, document_type, version) VALUES
		(?1, 'user_terms', 1), (?1, 'privacy_policy_user', 1)`, userID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (?1, ?2, 999001, '+998901112233')`,
		userID, domain.RoleRider)
	if err != nil {
		t.Fatal(err)
	}
}

func newRiderRequestTestEngine(t *testing.T, db *sql.DB) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		RequestExpiresSeconds: 3600,
		MatchRadiusKm:         3,
		StartingFee:           4000,
		PricePerKm:            1500,
		InfiniteDriverBalance: true,
	}
	codes := repositories.NewRiderLoginCodesRepo(db)
	sessions := repositories.NewRiderAuthSessionsRepo(db)
	tokens := services.NewRiderAuthTokenService(sessions, "test-rider-bot-token")
	riderAuthSvc := services.NewRiderAuthService(db, codes, tokens, fakeRiderBotHandler{}, services.RiderAuthConfig{})
	matchSvc := services.NewMatchService(db, (*tgbotapi.BotAPI)(nil), cfg)
	riderReqSvc := services.NewRiderRequestAppService(db, cfg, matchSvc)
	r := gin.New()
	RegisterRiderRequestRoutes(r, RiderRequestDeps{
		DB: db, Cfg: cfg, RiderAuthSvc: riderAuthSvc, RiderReqSvc: riderReqSvc,
	})
	toks, err := tokens.Issue(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return r, toks.AccessToken
}

func TestRiderRequest_NoBearer_401(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_no_bearer")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, _ := newRiderRequestTestEngine(t, db)

	rr := postJSON(r, "/v1/rider/requests", map[string]any{
		"pickup_lat": 41.3, "pickup_lng": 69.28,
	}, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	code, _ := decodeRiderRequestErr(t, rr)
	if code != "invalid_token" {
		t.Fatalf("code=%q body=%s", code, rr.Body.String())
	}
}

func decodeRiderRequestErr(t *testing.T, rr *httptest.ResponseRecorder) (code, msg string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	return env.Error.Code, env.Error.Message
}

func TestRiderRequest_DuplicatePending_409(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_dup")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	rr1 := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.3, "pickup_lng": 69.28}, h)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first create status=%d body=%s", rr1.Code, rr1.Body.String())
	}
	rr2 := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.31, "pickup_lng": 69.29}, h)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("second create status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	code, _ := decodeRiderRequestErr(t, rr2)
	if code != "duplicate_pending" {
		t.Fatalf("code=%q body=%s", code, rr2.Body.String())
	}
}

func TestRiderRequest_HappyPath_CreateDestinationConfirm(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_happy")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	rr := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.3, "pickup_lng": 69.28}, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var createOut struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &createOut); err != nil || createOut.RequestID == "" {
		t.Fatalf("decode create: %v body=%s", err, rr.Body.String())
	}

	path := "/v1/rider/requests/" + createOut.RequestID + "/destination"
	rr2 := postJSON(r, path, map[string]any{
		"drop_lat": 41.31, "drop_lng": 69.29, "drop_name": "Test",
	}, h)
	if rr2.Code != http.StatusOK {
		t.Fatalf("destination status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	var destOut struct {
		OK             bool  `json:"ok"`
		EstimatedPrice int64 `json:"estimated_price"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &destOut); err != nil || !destOut.OK || destOut.EstimatedPrice <= 0 {
		t.Fatalf("destination body: %v %+v raw=%s", err, destOut, rr2.Body.String())
	}

	var estDB int64
	var destConf int
	err := db.QueryRow(`SELECT estimated_price, COALESCE(destination_confirmed,0) FROM ride_requests WHERE id = ?1`, createOut.RequestID).Scan(&estDB, &destConf)
	if err != nil {
		t.Fatal(err)
	}
	if destConf != 0 {
		t.Fatalf("destination_confirmed before confirm: %d", destConf)
	}
	if estDB != destOut.EstimatedPrice {
		t.Fatalf("db estimated_price=%d json=%d", estDB, destOut.EstimatedPrice)
	}

	pathConfirm := "/v1/rider/requests/" + createOut.RequestID + "/confirm"
	rr3 := postJSON(r, pathConfirm, map[string]any{}, h)
	if rr3.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", rr3.Code, rr3.Body.String())
	}
	var confirmed int
	err = db.QueryRow(`SELECT COALESCE(destination_confirmed,0) FROM ride_requests WHERE id = ?1`, createOut.RequestID).Scan(&confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed != 1 {
		t.Fatalf("destination_confirmed=%d want 1", confirmed)
	}
}

// A drop within ~100 m of pickup is a mis-tap, not a trip: the server must
// reject it with a structured invalid_coordinates even though the app also
// blocks it client-side.
func TestRiderRequest_DestinationTooCloseToPickup_400(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_too_close")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	rr := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.3, "pickup_lng": 69.28}, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var createOut struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &createOut); err != nil || createOut.RequestID == "" {
		t.Fatalf("decode create: %v body=%s", err, rr.Body.String())
	}

	// ~55 m north of pickup (0.0005° latitude).
	path := "/v1/rider/requests/" + createOut.RequestID + "/destination"
	rr2 := postJSON(r, path, map[string]any{
		"drop_lat": 41.3005, "drop_lng": 69.28, "drop_name": "Same corner",
	}, h)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("too-close destination status=%d want 400 body=%s", rr2.Code, rr2.Body.String())
	}
	var errOut struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &errOut); err != nil || errOut.Error.Code != "invalid_coordinates" {
		t.Fatalf("want error.code=invalid_coordinates got %s", rr2.Body.String())
	}

	// The request must remain usable: a real destination still succeeds.
	rr3 := postJSON(r, path, map[string]any{
		"drop_lat": 41.31, "drop_lng": 69.29, "drop_name": "Real",
	}, h)
	if rr3.Code != http.StatusOK {
		t.Fatalf("real destination after rejection status=%d body=%s", rr3.Code, rr3.Body.String())
	}
}

func TestRiderRequest_CamelCaseBody_EstimatedPriceAlias(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_camel")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	rr := postJSON(r, "/v1/rider/requests", map[string]any{"pickupLat": 41.3, "pickupLng": 69.28}, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var createOut struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &createOut); err != nil || createOut.RequestID == "" {
		t.Fatalf("decode create requestId: %v body=%s", err, rr.Body.String())
	}

	path := "/v1/rider/requests/" + createOut.RequestID + "/destination"
	rr2 := postJSON(r, path, map[string]any{
		"dropLat": 41.31, "dropLng": 69.29, "dropName": "Test",
	}, h)
	if rr2.Code != http.StatusOK {
		t.Fatalf("destination status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	var destOut struct {
		OK             bool  `json:"ok"`
		EstimatedPrice int64 `json:"estimatedPrice"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &destOut); err != nil || !destOut.OK || destOut.EstimatedPrice <= 0 {
		t.Fatalf("destination body (camel estimatedPrice): %v %+v raw=%s", err, destOut, rr2.Body.String())
	}
}

func TestRiderRequest_ConfirmBeforeDestination_409(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_confirm_early")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	rr := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.3, "pickup_lng": 69.28}, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var createOut struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &createOut); err != nil {
		t.Fatal(err)
	}
	pathConfirm := "/v1/rider/requests/" + createOut.RequestID + "/confirm"
	rr3 := postJSON(r, pathConfirm, map[string]any{}, h)
	if rr3.Code != http.StatusConflict {
		t.Fatalf("confirm status=%d body=%s", rr3.Code, rr3.Body.String())
	}
}

func TestRiderRequest_LegalRequired_403(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_legal")
	defer db.Close()
	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, ?, 999001, '+998901112233')`, domain.RoleRider)
	if err != nil {
		t.Fatal(err)
	}
	// No legal_documents / acceptances
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	rr := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.3, "pickup_lng": 69.28}, h)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	code, _ := decodeRiderRequestErr(t, rr)
	if code != "legal_required" {
		t.Fatalf("code=%q", code)
	}
}

func TestRiderRequest_CancelPending_ByPathAndBody_Idempotent(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_cancel_pending")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	// Create request.
	rr := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.3, "pickup_lng": 69.28}, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var createOut struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &createOut); err != nil || createOut.RequestID == "" {
		t.Fatalf("decode create: %v body=%s", err, rr.Body.String())
	}

	// Cancel by path.
	path := "/v1/rider/requests/" + createOut.RequestID + "/cancel"
	rr2 := postJSON(r, path, map[string]any{}, h)
	if rr2.Code != http.StatusOK {
		t.Fatalf("cancel path status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	var out1 struct {
		OK        bool   `json:"ok"`
		RequestID string `json:"request_id"`
		TripID    any    `json:"trip_id"`
		Status    string `json:"status"`
		Result    string `json:"result"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &out1); err != nil {
		t.Fatalf("decode cancel path: %v body=%s", err, rr2.Body.String())
	}
	if !out1.OK || out1.RequestID != createOut.RequestID || out1.Status != domain.RequestStatusCancelled || out1.Result != "updated" {
		t.Fatalf("unexpected cancel response: %+v raw=%s", out1, rr2.Body.String())
	}

	// Cancel again by body => noop.
	rr3 := postJSON(r, "/v1/rider/requests/cancel", map[string]any{"request_id": createOut.RequestID}, h)
	if rr3.Code != http.StatusOK {
		t.Fatalf("cancel body status=%d body=%s", rr3.Code, rr3.Body.String())
	}
	var out2 struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(rr3.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode cancel body: %v body=%s", err, rr3.Body.String())
	}
	if !out2.OK || out2.Status != domain.RequestStatusCancelled || out2.Result != "noop" {
		t.Fatalf("unexpected cancel body noop: %+v raw=%s", out2, rr3.Body.String())
	}
}

func TestRiderRequest_Cancel_NotYourRequest_403(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_cancel_not_yours")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	seedRiderLegalAndUser(t, db, 2)
	// Seed a pending request for user 2.
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at)
		VALUES ('req-2', 2, 41.3, 69.28, 3, ?1, datetime('now','+1 hour'))`, domain.RequestStatusPending); err != nil {
		t.Fatal(err)
	}
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	rr := postJSON(r, "/v1/rider/requests/req-2/cancel", map[string]any{}, h)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	code, _ := decodeRiderRequestErr(t, rr)
	if code != "not_your_request" {
		t.Fatalf("code=%q body=%s", code, rr.Body.String())
	}
}

// duplicate_pending must identify the request that is blocking the new one.
//
// Without it the client can only tell the rider to wait, and if the blocking
// request was abandoned before confirming a destination that wait is up to the
// 30-minute server-side timeout — which reads as the app being broken. With the
// id and the confirmed flag, the client can offer to cancel and start over.
func TestRiderRequest_DuplicatePendingIdentifiesBlockingRequest(t *testing.T) {
	db := setupRiderRequestHandlerDB(t, "rider_req_dup_details")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, token := newRiderRequestTestEngine(t, db)
	h := map[string]string{"Authorization": "Bearer " + token}

	first := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.3, "pickup_lng": 69.28}, h)
	if first.Code != http.StatusOK {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	var created struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	second := postJSON(r, "/v1/rider/requests", map[string]any{"pickup_lat": 41.31, "pickup_lng": 69.29}, h)
	if second.Code != http.StatusConflict {
		t.Fatalf("second create status=%d body=%s", second.Code, second.Body.String())
	}

	var env struct {
		Error struct {
			Code                 string `json:"code"`
			PendingRequestID     string `json:"pending_request_id"`
			DestinationConfirmed bool   `json:"destination_confirmed"`
		} `json:"error"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, second.Body.String())
	}
	if env.Error.Code != "duplicate_pending" {
		t.Fatalf("code=%q, want duplicate_pending", env.Error.Code)
	}
	if env.Error.PendingRequestID != created.RequestID {
		t.Errorf("pending_request_id = %q, want the blocking request %q", env.Error.PendingRequestID, created.RequestID)
	}
	if env.Error.DestinationConfirmed {
		t.Error("a freshly created request has no confirmed destination; the flag should be false")
	}
}
