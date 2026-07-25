package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "modernc.org/sqlite"
	"taxi-mvp/internal/repositories"
	"taxi-mvp/internal/services"
)

type fakeTelegramSender struct {
	sendErr error
	sent    int
}

func (f *fakeTelegramSender) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.sent++
	return tgbotapi.Message{}, f.sendErr
}

func setupOTPTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:otp_test?mode=memory&cache=shared")
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
	);`)
	exec(`CREATE TABLE drivers (
		user_id INTEGER PRIMARY KEY,
		verification_status TEXT,
		phone TEXT
	);`)
	exec(`CREATE TABLE driver_login_codes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		code TEXT NOT NULL,
		used INTEGER NOT NULL DEFAULT 0,
		failed_attempts INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		expires_at TEXT NOT NULL
	);`)
	exec(`CREATE TABLE driver_auth_sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		expires_at INTEGER NOT NULL,
		revoked INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
	);`)
	return db
}

func performJSONRequest(r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestDriverAuthRequestCode_NotRegisteredIs403WithCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	// No users/drivers inserted => not registered.
	bot := &fakeTelegramSender{}
	r := gin.New()
	r.POST("/auth/request-code", DriverAuthRequestCode(db, bot))

	rr := performJSONRequest(r, "POST", "/auth/request-code", map[string]string{"phone": "+998901234567"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["code"] != errDriverAuthNotRegistered {
		t.Fatalf("code=%v want %v body=%s", out["code"], errDriverAuthNotRegistered, rr.Body.String())
	}
}

func TestDriverAuthRequestCode_RiderOnlyPhoneIs403WithCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	// Rider exists with phone, but no drivers row.
	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (10, 'rider', 111, '998990708446')`)
	if err != nil {
		t.Fatal(err)
	}

	bot := &fakeTelegramSender{}
	r := gin.New()
	r.POST("/auth/request-code", DriverAuthRequestCode(db, bot))

	rr := performJSONRequest(r, "POST", "/auth/request-code", map[string]string{"phone": "998990708446"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["code"] != errDriverAuthNotRegistered {
		t.Fatalf("code=%v want %v body=%s", out["code"], errDriverAuthNotRegistered, rr.Body.String())
	}
}

func TestDriverAuthRequestCode_InvalidPhoneIs400WithCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	bot := &fakeTelegramSender{}
	r := gin.New()
	r.POST("/auth/request-code", DriverAuthRequestCode(db, bot))

	rr := performJSONRequest(r, "POST", "/auth/request-code", map[string]string{"phone": "   "})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["code"] != errDriverAuthInvalidPhone {
		t.Fatalf("code=%v want %v body=%s", out["code"], errDriverAuthInvalidPhone, rr.Body.String())
	}
}

func TestDriverAuthRequestCode_ApprovedDriverIs200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	// Seed approved driver with telegram id and phone.
	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'driver', 999, '998901234567')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO drivers (user_id, verification_status, phone) VALUES (1, 'approved', '998901234567')`)
	if err != nil {
		t.Fatal(err)
	}

	bot := &fakeTelegramSender{}
	r := gin.New()
	r.POST("/auth/request-code", DriverAuthRequestCode(db, bot))

	rr := performJSONRequest(r, "POST", "/auth/request-code", map[string]string{"phone": "+998 90 123-45-67"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if bot.sent != 1 {
		t.Fatalf("telegram sends=%d want 1", bot.sent)
	}
}

func TestDriverAuthRequestCode_PrefersDriverWhenPhoneShared(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	// Rider user with the same phone (should not block driver OTP).
	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (10, 'rider', 111, '998990708446')`)
	if err != nil {
		t.Fatal(err)
	}

	// Approved driver user with the same phone.
	_, err = db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (11, 'driver', 222, '998990708446')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO drivers (user_id, verification_status, phone) VALUES (11, 'approved', '998990708446')`)
	if err != nil {
		t.Fatal(err)
	}

	bot := &fakeTelegramSender{}
	r := gin.New()
	r.POST("/auth/request-code", DriverAuthRequestCode(db, bot))

	rr := performJSONRequest(r, "POST", "/auth/request-code", map[string]string{"phone": "998990708446"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if bot.sent != 1 {
		t.Fatalf("telegram sends=%d want 1", bot.sent)
	}
}

func TestDriverAuthRequestCode_AllowsApprovedDriverEvenIfUsersRoleRider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	// users.role is "rider" but drivers row exists and is approved (matches production log case).
	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (41, 'rider', 333, '998990708446')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO drivers (user_id, verification_status, phone) VALUES (41, 'approved', '998990708446')`)
	if err != nil {
		t.Fatal(err)
	}

	bot := &fakeTelegramSender{}
	r := gin.New()
	r.POST("/auth/request-code", DriverAuthRequestCode(db, bot))

	rr := performJSONRequest(r, "POST", "/auth/request-code", map[string]string{"phone": "998990708446"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if bot.sent != 1 {
		t.Fatalf("telegram sends=%d want 1", bot.sent)
	}
}

func TestDriverAuthVerifyCode_NotRegisteredIs403WithCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	tokens := services.NewDriverAuthTokenService(repositories.NewDriverAuthSessionsRepo(db))
	r := gin.New()
	r.POST("/auth/verify-code", DriverAuthVerifyCode(db, tokens))

	rr := performJSONRequest(r, "POST", "/auth/verify-code", map[string]string{"phone": "+998901234567", "code": "123456"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["code"] != errDriverAuthNotRegistered {
		t.Fatalf("code=%v want %v body=%s", out["code"], errDriverAuthNotRegistered, rr.Body.String())
	}
}

func TestDriverAuthVerifyCode_InvalidCodeIs400WithCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'driver', 999, '998901234567')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO drivers (user_id, verification_status, phone) VALUES (1, 'approved', '998901234567')`)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a code different from what we submit.
	_, err = db.Exec(`INSERT INTO driver_login_codes (user_id, code, used, created_at, expires_at) VALUES (1, '999999', 0, ?1, ?2)`,
		time.Now().UTC().Format("2006-01-02 15:04:05"),
		time.Now().UTC().Add(2*time.Minute).Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatal(err)
	}

	tokens := services.NewDriverAuthTokenService(repositories.NewDriverAuthSessionsRepo(db))
	r := gin.New()
	r.POST("/auth/verify-code", DriverAuthVerifyCode(db, tokens))

	rr := performJSONRequest(r, "POST", "/auth/verify-code", map[string]string{"phone": "998901234567", "code": "123456"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["code"] != errDriverAuthInvalidCode {
		t.Fatalf("code=%v want %v body=%s", out["code"], errDriverAuthInvalidCode, rr.Body.String())
	}
}

func TestDriverAuthVerifyCode_IssuesBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'driver', 999, '998901234567')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO drivers (user_id, verification_status, phone) VALUES (1, 'approved', '998901234567')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO driver_login_codes (user_id, code, used, created_at, expires_at) VALUES (1, '123456', 0, ?1, ?2)`,
		time.Now().UTC().Format("2006-01-02 15:04:05"),
		time.Now().UTC().Add(2*time.Minute).Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatal(err)
	}

	tokens := services.NewDriverAuthTokenService(repositories.NewDriverAuthSessionsRepo(db))
	r := gin.New()
	r.POST("/auth/verify-code", DriverAuthVerifyCode(db, tokens))

	rr := performJSONRequest(r, "POST", "/auth/verify-code", map[string]string{"phone": "998901234567", "code": "123456"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["access_token"] == nil || out["access_token"] == "" {
		t.Fatalf("missing access_token body=%s", rr.Body.String())
	}
	if out["token_type"] != "Bearer" {
		t.Fatalf("token_type=%v", out["token_type"])
	}
	if out["driver_id"] != float64(1) {
		t.Fatalf("driver_id=%v", out["driver_id"])
	}
}

func TestDriverAuthVerifyCode_InvalidatesAfterFiveFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	_, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'driver', 999, '998901234567')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO drivers (user_id, verification_status, phone) VALUES (1, 'approved', '998901234567')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO driver_login_codes (user_id, code, used, created_at, expires_at) VALUES (1, '123456', 0, ?1, ?2)`,
		time.Now().UTC().Format("2006-01-02 15:04:05"),
		time.Now().UTC().Add(2*time.Minute).Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatal(err)
	}

	tokens := services.NewDriverAuthTokenService(repositories.NewDriverAuthSessionsRepo(db))
	r := gin.New()
	r.POST("/auth/verify-code", DriverAuthVerifyCode(db, tokens))

	for i := 0; i < 5; i++ {
		rr := performJSONRequest(r, "POST", "/auth/verify-code", map[string]string{"phone": "998901234567", "code": "000000"})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d status=%d body=%s", i+1, rr.Code, rr.Body.String())
		}
	}
	var used int
	if err := db.QueryRow(`SELECT used FROM driver_login_codes WHERE user_id = 1`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Fatalf("used=%d want 1 after 5 failures", used)
	}
	// Correct code should now fail (code invalidated).
	rr := performJSONRequest(r, "POST", "/auth/verify-code", map[string]string{"phone": "998901234567", "code": "123456"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("after invalidate status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestNormalizePhoneDigits(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"+998901234567", "998901234567"},
		{"998901234567", "998901234567"},
		{"901234567", "998901234567"},
		{"  +998 90 123 45 67 ", "998901234567"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizePhoneDigits(tt.in); got != tt.want {
			t.Errorf("normalizePhoneDigits(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Concurrent wrong guesses must all be counted. The previous implementation read
// failed_attempts, incremented in Go and wrote back an absolute value, so N
// simultaneous requests all read the same number and all wrote the same +1 —
// the 5-attempt lockout never tripped and the code space could be probed at
// full concurrency.
func TestDriverAuthVerifyCode_ConcurrentWrongGuessesConsumeCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupOTPTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'driver', 999, '998901234567')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO drivers (user_id, verification_status, phone) VALUES (1, 'approved', '998901234567')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO driver_login_codes (user_id, code, used, created_at, expires_at) VALUES (1, '999999', 0, ?1, ?2)`,
		time.Now().UTC().Format("2006-01-02 15:04:05"),
		time.Now().UTC().Add(2*time.Minute).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	tokens := services.NewDriverAuthTokenService(repositories.NewDriverAuthSessionsRepo(db))
	r := gin.New()
	r.POST("/auth/verify-code", DriverAuthVerifyCode(db, tokens))

	const guesses = driverAuthMaxFailedAttempts
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < guesses; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			performJSONRequest(r, "POST", "/auth/verify-code", map[string]string{"phone": "998901234567", "code": "123456"})
		}()
	}
	close(start)
	wg.Wait()

	var attempts, used int
	if err := db.QueryRow(`SELECT COALESCE(failed_attempts,0), used FROM driver_login_codes WHERE user_id = 1`).Scan(&attempts, &used); err != nil {
		t.Fatal(err)
	}
	if attempts != guesses {
		t.Errorf("failed_attempts = %d, want %d (lost increments mean the lockout can be bypassed)", attempts, guesses)
	}
	if used != 1 {
		t.Errorf("used = %d, want 1 (code should be consumed at %d attempts)", used, driverAuthMaxFailedAttempts)
	}

	// The consumed code must no longer be usable, even with the right value.
	rr := performJSONRequest(r, "POST", "/auth/verify-code", map[string]string{"phone": "998901234567", "code": "999999"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("consumed code still accepted: status=%d body=%s", rr.Code, rr.Body.String())
	}
}
