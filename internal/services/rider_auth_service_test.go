package services

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"taxi-mvp/internal/repositories"

	_ "modernc.org/sqlite"
)

// fakeRiderBot is a minimal LoginCodeSender for tests. It captures every
// outgoing message so we can assert chat id, parse mode, and that the
// plaintext code is present (deliberately, since the message is the only
// channel the code travels over).
type fakeRiderBot struct {
	mu        sync.Mutex
	sent      []tgbotapi.MessageConfig
	sendErr   error
	failFirst bool // first Send returns sendErr; subsequent Sends succeed
	calls     int
}

func (f *fakeRiderBot) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	mc, ok := c.(tgbotapi.MessageConfig)
	if ok {
		f.sent = append(f.sent, mc)
	}
	if f.sendErr != nil && (!f.failFirst || f.calls == 1) {
		return tgbotapi.Message{}, f.sendErr
	}
	return tgbotapi.Message{}, nil
}

func setupRiderAuthDB(t *testing.T, name string) *sql.DB {
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

func newRiderAuthSvc(t *testing.T, db *sql.DB, bot *fakeRiderBot, codeOverride string) *RiderAuthService {
	t.Helper()
	codes := repositories.NewRiderLoginCodesRepo(db)
	sessions := repositories.NewRiderAuthSessionsRepo(db)
	tokens := NewRiderAuthTokenService(sessions, "test-rider-bot-token-XYZ")
	svc := NewRiderAuthService(db, codes, tokens, bot, RiderAuthConfig{
		CodeTTL:              5 * time.Minute,
		PerPhoneCooldown:     60 * time.Second,
		PerPhoneHourlyMax:    5,
		PerPhoneHourlyWindow: time.Hour,
		MaxAttempts:          5,
	})
	if codeOverride != "" {
		svc.randCode = func() (string, error) { return codeOverride, nil }
	}
	return svc
}

func TestNormalizeUzbekPhone(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"+998901234567", "+998901234567", true},
		{"998901234567", "+998901234567", true},
		{"901234567", "+998901234567", true},
		{"  +998 90 123-45-67 ", "+998901234567", true},
		{"123", "", false},
		{"", "", false},
		{"+1234567890123", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeUzbekPhone(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("NormalizeUzbekPhone(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestRequestCode_DeliversCodeViaTelegram(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_send")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}

	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "123456")

	res, err := svc.RequestCode(context.Background(), "+998901234567")
	if err != nil {
		t.Fatalf("RequestCode: %v", err)
	}
	if res.UserID != 1 || res.TelegramID != 555 {
		t.Fatalf("res=%+v", res)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("sent=%d want 1", len(bot.sent))
	}
	msg := bot.sent[0]
	if msg.ChatID != 555 {
		t.Fatalf("chat id=%d want 555", msg.ChatID)
	}
	if msg.ParseMode != "HTML" {
		t.Fatalf("parse mode=%q want HTML", msg.ParseMode)
	}
	if !strings.Contains(msg.Text, "<code>123456</code>") {
		t.Fatalf("rendered message missing code: %q", msg.Text)
	}

	// Acceptance: a row was persisted in rider_login_codes.
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM rider_login_codes WHERE phone='+998901234567' AND consumed=0`).Scan(&n)
	if n != 1 {
		t.Fatalf("active rows=%d want 1", n)
	}
}

func TestRequestCode_PhoneNotRegistered(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_no_phone")
	defer db.Close()
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "123456")

	_, err := svc.RequestCode(context.Background(), "+998901234567")
	if !errors.Is(err, ErrRiderAuthPhoneNotFound) {
		t.Fatalf("want ErrRiderAuthPhoneNotFound, got %v", err)
	}
	if len(bot.sent) != 0 {
		t.Fatalf("should not have sent, got %d", len(bot.sent))
	}
}

func TestRequestCode_TelegramNotLinked(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_no_tg")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'rider', 0, '+998901234567')`); err != nil {
		t.Fatal(err)
	}
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "123456")

	_, err := svc.RequestCode(context.Background(), "998901234567")
	if !errors.Is(err, ErrRiderAuthTelegramNotLink) {
		t.Fatalf("want ErrRiderAuthTelegramNotLink, got %v", err)
	}
	if len(bot.sent) != 0 {
		t.Fatalf("should not have sent, got %d", len(bot.sent))
	}
}

func TestRequestCode_RateLimitedWithin60s(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_rl")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "123456")

	if _, err := svc.RequestCode(context.Background(), "+998901234567"); err != nil {
		t.Fatalf("first RequestCode: %v", err)
	}

	_, err := svc.RequestCode(context.Background(), "+998901234567")
	var recent *RiderAuthCodeRecentError
	if !errors.As(err, &recent) {
		t.Fatalf("want RiderAuthCodeRecentError, got %v", err)
	}
	if !errors.Is(err, ErrRiderAuthCodeRecentlySent) {
		t.Fatalf("errors.Is(ErrRiderAuthCodeRecentlySent) should be true, got %v", err)
	}
	if recent.RetryAfter <= 0 || recent.RetryAfter > 60*time.Second {
		t.Fatalf("retry_after=%s out of range", recent.RetryAfter)
	}
	// Crucially: only one Telegram message went out.
	if len(bot.sent) != 1 {
		t.Fatalf("sent=%d want 1 (second request must NOT send)", len(bot.sent))
	}
}

func TestRequestCode_HourlyCap(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_hour_cap")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}
	// Pre-seed 5 rows in the last hour so the 6th attempt trips the cap.
	now := time.Now().Unix()
	for i := 0; i < 5; i++ {
		if _, err := db.Exec(`INSERT INTO rider_login_codes (phone, code_hash, salt, expires_at, created_at, consumed)
			VALUES ('+998901234567', 'h', 's', ?1, ?2, 1)`, now+300, now-int64(120+i*60)); err != nil {
			t.Fatal(err)
		}
	}
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "123456")

	_, err := svc.RequestCode(context.Background(), "+998901234567")
	if !errors.Is(err, ErrRiderAuthTooManyCodes) {
		t.Fatalf("want ErrRiderAuthTooManyCodes, got %v", err)
	}
	if len(bot.sent) != 0 {
		t.Fatalf("should not send, got %d", len(bot.sent))
	}
}

func TestVerifyCode_Success_IssuesTokens(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_verify_ok")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (7, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "432198")

	if _, err := svc.RequestCode(context.Background(), "+998901234567"); err != nil {
		t.Fatal(err)
	}

	tokens, err := svc.VerifyCode(context.Background(), "998901234567", "432198")
	if err != nil {
		t.Fatalf("VerifyCode: %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatalf("missing tokens: %+v", tokens)
	}
	if tokens.ExpiresIn <= 0 {
		t.Fatalf("expires_in=%d", tokens.ExpiresIn)
	}
	if tokens.TokenType != "Bearer" {
		t.Fatalf("token type=%q", tokens.TokenType)
	}

	// Code row must be consumed now.
	var consumed int
	_ = db.QueryRow(`SELECT consumed FROM rider_login_codes ORDER BY id DESC LIMIT 1`).Scan(&consumed)
	if consumed != 1 {
		t.Fatalf("code row consumed=%d want 1", consumed)
	}

	// Re-using the same code must fail (consumed).
	if _, err := svc.VerifyCode(context.Background(), "+998901234567", "432198"); !errors.Is(err, ErrRiderAuthInvalidCode) {
		t.Fatalf("re-use should fail with invalid_code, got %v", err)
	}
}

func TestVerifyCode_5WrongAttemptsConsumesAndReturnsTooMany(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_verify_5")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (7, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "111122")

	if _, err := svc.RequestCode(context.Background(), "+998901234567"); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 4; i++ {
		_, err := svc.VerifyCode(context.Background(), "+998901234567", "000000")
		if !errors.Is(err, ErrRiderAuthInvalidCode) {
			t.Fatalf("attempt %d: want invalid_code got %v", i, err)
		}
	}
	// 5th wrong attempt invalidates the code.
	_, err := svc.VerifyCode(context.Background(), "+998901234567", "000000")
	if !errors.Is(err, ErrRiderAuthTooManyAttempts) {
		t.Fatalf("5th attempt: want too_many_attempts got %v", err)
	}
	// Now the right code must also fail because the row is consumed.
	_, err = svc.VerifyCode(context.Background(), "+998901234567", "111122")
	if !errors.Is(err, ErrRiderAuthInvalidCode) {
		t.Fatalf("post-lockout right code: want invalid_code got %v", err)
	}
}

func TestRequestCode_BotBlockedConsumesRow(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_block")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}
	bot := &fakeRiderBot{
		sendErr: &tgbotapi.Error{Code: 403, Message: "Forbidden: bot was blocked by the user"},
	}
	svc := newRiderAuthSvc(t, db, bot, "123456")

	_, err := svc.RequestCode(context.Background(), "+998901234567")
	if !errors.Is(err, ErrRiderAuthBotBlocked) {
		t.Fatalf("want ErrRiderAuthBotBlocked got %v", err)
	}

	var live int
	_ = db.QueryRow(`SELECT COUNT(*) FROM rider_login_codes WHERE phone='+998901234567' AND consumed=0`).Scan(&live)
	if live != 0 {
		t.Fatalf("active rows after bot_blocked = %d want 0", live)
	}
}

func TestRefresh_RotatesAndOldRefreshIsRevoked(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_refresh")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (7, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "432198")

	if _, err := svc.RequestCode(context.Background(), "+998901234567"); err != nil {
		t.Fatal(err)
	}
	first, err := svc.VerifyCode(context.Background(), "+998901234567", "432198")
	if err != nil {
		t.Fatalf("VerifyCode: %v", err)
	}

	rotated, err := svc.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rotated.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh token must rotate")
	}
	if rotated.AccessToken == first.AccessToken {
		t.Fatalf("access token must rotate")
	}

	// Old refresh token must no longer work.
	if _, err := svc.Refresh(context.Background(), first.RefreshToken); err == nil {
		t.Fatalf("old refresh must fail after rotation")
	}
}

func TestLogout_RevokesRefreshAndAccessIsParseable(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_logout")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (7, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "432198")

	if _, err := svc.RequestCode(context.Background(), "+998901234567"); err != nil {
		t.Fatal(err)
	}
	tokens, err := svc.VerifyCode(context.Background(), "+998901234567", "432198")
	if err != nil {
		t.Fatal(err)
	}

	uid, err := svc.VerifyAccessToken(tokens.AccessToken)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if uid != 7 {
		t.Fatalf("uid=%d want 7", uid)
	}

	if err := svc.Logout(context.Background(), uid); err != nil {
		t.Fatal(err)
	}

	// Refresh must fail post-logout.
	if _, err := svc.Refresh(context.Background(), tokens.RefreshToken); err == nil {
		t.Fatalf("refresh after logout should fail")
	}
}

func TestRequestCode_InvalidPhoneIsRejected(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_bad_phone")
	defer db.Close()
	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "123456")

	_, err := svc.RequestCode(context.Background(), "abc")
	if !errors.Is(err, ErrRiderAuthInvalidPhone) {
		t.Fatalf("want ErrRiderAuthInvalidPhone got %v", err)
	}
	if len(bot.sent) != 0 {
		t.Fatalf("should not send, got %d", len(bot.sent))
	}
}

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"+998901234567": "********4567",
		"4567":          "****",
		"":              "",
		"12":            "**",
	}
	for in, want := range cases {
		if got := maskPhone(in); got != want {
			t.Errorf("maskPhone(%q)=%q want %q", in, got, want)
		}
	}
}

// Round-trip with the REAL code generator, no override.
//
// Every other test in this file injects a fixed code, so all of them kept
// passing when generation moved to 6 digits while VerifyCode still required
// exactly 4 — which made native rider login impossible for every real user.
// This test is the one that fails if the two ever disagree again.
func TestRequestThenVerify_UsesRealGeneratedCode(t *testing.T) {
	db := setupRiderAuthDB(t, "rider_auth_roundtrip")
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO users (id, role, telegram_id, phone) VALUES (1, 'rider', 555, '+998901234567')`); err != nil {
		t.Fatal(err)
	}

	bot := &fakeRiderBot{}
	svc := newRiderAuthSvc(t, db, bot, "") // no override: real generateLoginCode

	if _, err := svc.RequestCode(context.Background(), "+998901234567"); err != nil {
		t.Fatalf("RequestCode: %v", err)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("sent=%d want 1", len(bot.sent))
	}

	code := extractOTPFromMessage(t, bot.sent[0].Text)
	if len(code) != riderOTPDigits {
		t.Fatalf("generated code %q has %d digits, want %d", code, len(code), riderOTPDigits)
	}

	tokens, err := svc.VerifyCode(context.Background(), "+998901234567", code)
	if err != nil {
		t.Fatalf("VerifyCode rejected the code the service itself generated (%q): %v", code, err)
	}
	if tokens == nil || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatalf("tokens=%+v, want a populated token pair", tokens)
	}
}

func extractOTPFromMessage(t *testing.T, text string) string {
	t.Helper()
	m := regexp.MustCompile(`<code>(\d+)</code>`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no code found in delivered message: %q", text)
	}
	return m[1]
}
