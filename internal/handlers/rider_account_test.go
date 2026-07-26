package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/repositories"
	"taxi-mvp/internal/services"
)

// setupRiderAccountDB extends the shared rider-request schema with the columns
// and tables DeleteAccount touches (users.name, rider_app_notifications).
func setupRiderAccountDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db := setupRiderRequestHandlerDB(t, name)
	for _, q := range []string{
		`ALTER TABLE users ADD COLUMN name TEXT`,
		`CREATE TABLE rider_app_notifications (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			rider_user_id INTEGER NOT NULL,
			body TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func newRiderAccountTestEngine(t *testing.T, db *sql.DB) (*gin.Engine, *services.RiderAuthService, *services.RiderAuthTokenService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	codes := repositories.NewRiderLoginCodesRepo(db)
	sessions := repositories.NewRiderAuthSessionsRepo(db)
	tokens := services.NewRiderAuthTokenService(sessions, "test-rider-bot-token")
	svc := services.NewRiderAuthService(db, codes, tokens, fakeRiderBotHandler{}, services.RiderAuthConfig{})
	r := gin.New()
	RegisterRiderAccountRoutes(r, db, svc)
	return r, svc, tokens
}

func doDelete(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestRiderAccount_Delete_ErasesPIIAndRevokesSessions(t *testing.T) {
	db := setupRiderAccountDB(t, "rider_account_delete")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	if _, err := db.Exec(`UPDATE users SET name = 'Test Rider' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO rider_login_codes (phone, code_hash, salt, expires_at) VALUES ('+998901112233','h','s',9999999999)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO rider_app_notifications (rider_user_id, body) VALUES (1, 'hi')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at)
		VALUES ('req-1', 1, 41.3, 69.28, 3, 'PENDING', datetime('now','+120 seconds'))`); err != nil {
		t.Fatal(err)
	}

	r, _, tokens := newRiderAccountTestEngine(t, db)
	pair, err := tokens.Issue(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	rr := doDelete(r, "/v1/rider/account", pair.AccessToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || !out.OK {
		t.Fatalf("body: %s", rr.Body.String())
	}

	var phone, uname sql.NullString
	var tgID int64
	if err := db.QueryRow(`SELECT phone, name, telegram_id FROM users WHERE id = 1`).Scan(&phone, &uname, &tgID); err != nil {
		t.Fatal(err)
	}
	if phone.Valid && phone.String != "" {
		t.Fatalf("phone not erased: %q", phone.String)
	}
	if uname.Valid && uname.String != "" {
		t.Fatalf("name not erased: %q", uname.String)
	}
	if tgID != -1 {
		t.Fatalf("telegram_id = %d want -1 (severed)", tgID)
	}
	var codes, notifs int
	_ = db.QueryRow(`SELECT COUNT(*) FROM rider_login_codes WHERE phone = '+998901112233'`).Scan(&codes)
	_ = db.QueryRow(`SELECT COUNT(*) FROM rider_app_notifications WHERE rider_user_id = 1`).Scan(&notifs)
	if codes != 0 || notifs != 0 {
		t.Fatalf("login codes=%d notifications=%d, want 0/0", codes, notifs)
	}
	var reqStatus string
	_ = db.QueryRow(`SELECT status FROM ride_requests WHERE id = 'req-1'`).Scan(&reqStatus)
	if reqStatus != "CANCELLED" {
		t.Fatalf("pending request status=%q want CANCELLED", reqStatus)
	}

	// Every session is revoked: the refresh token from before deletion is dead.
	if _, err := tokens.Refresh(context.Background(), pair.RefreshToken); err == nil {
		t.Fatalf("refresh must fail after account deletion")
	}
}

func TestRiderAccount_Delete_BlockedDuringActiveTrip(t *testing.T) {
	db := setupRiderAccountDB(t, "rider_account_active_trip")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at)
		VALUES ('req-2', 1, 41.3, 69.28, 3, 'MATCHED', datetime('now','+120 seconds'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status)
		VALUES ('trip-1', 'req-2', 5, 1, 'STARTED')`); err != nil {
		t.Fatal(err)
	}

	r, _, tokens := newRiderAccountTestEngine(t, db)
	pair, err := tokens.Issue(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	rr := doDelete(r, "/v1/rider/account", pair.AccessToken)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete during trip status=%d want 409 body=%s", rr.Code, rr.Body.String())
	}
	var errOut struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &errOut); err != nil || errOut.Error.Code != "invalid_state" {
		t.Fatalf("want error.code=invalid_state got %s", rr.Body.String())
	}
	// Nothing was touched.
	var phone sql.NullString
	if err := db.QueryRow(`SELECT phone FROM users WHERE id = 1`).Scan(&phone); err != nil {
		t.Fatal(err)
	}
	if !phone.Valid || phone.String == "" {
		t.Fatalf("phone must be untouched while trip active")
	}
}

func TestRiderAccount_Delete_NoBearer401(t *testing.T) {
	db := setupRiderAccountDB(t, "rider_account_no_bearer")
	defer db.Close()
	seedRiderLegalAndUser(t, db, 1)
	r, _, _ := newRiderAccountTestEngine(t, db)
	rr := doDelete(r, "/v1/rider/account", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", rr.Code, rr.Body.String())
	}
}
