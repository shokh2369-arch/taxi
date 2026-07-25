package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	riderbot "taxi-mvp/internal/bot/riderauth"
	"taxi-mvp/internal/repositories"
)

// Rider native-auth service errors. Each one maps to a specific HTTP status +
// JSON body in internal/handlers/rider_auth_handler.go.
var (
	ErrRiderAuthInvalidPhone     = errors.New("rider auth: invalid phone")
	ErrRiderAuthPhoneNotFound    = errors.New("rider auth: phone not registered")
	ErrRiderAuthTelegramNotLink  = errors.New("rider auth: telegram not linked")
	ErrRiderAuthCodeRecentlySent = errors.New("rider auth: code recently sent")
	ErrRiderAuthTooManyCodes     = errors.New("rider auth: too many codes")
	ErrRiderAuthBotBlocked       = errors.New("rider auth: bot blocked")
	ErrRiderAuthSendFailed       = errors.New("rider auth: send failed")
	ErrRiderAuthInvalidCode      = errors.New("rider auth: invalid code")
	ErrRiderAuthCodeExpired      = errors.New("rider auth: code expired")
	ErrRiderAuthTooManyAttempts  = errors.New("rider auth: too many attempts")
	ErrRiderAuthInternal         = errors.New("rider auth: internal")
)

// RiderAuthCodeRecentError is returned by RequestCode when the per-phone 60s
// throttle hits. Its RetryAfter field tells the rider app how long to wait.
type RiderAuthCodeRecentError struct {
	RetryAfter time.Duration
}

func (e *RiderAuthCodeRecentError) Error() string {
	return fmt.Sprintf("rider auth: code recently sent, retry after %s", e.RetryAfter)
}

// Is allows errors.Is(err, ErrRiderAuthCodeRecentlySent) to keep working.
func (e *RiderAuthCodeRecentError) Is(target error) bool {
	return target == ErrRiderAuthCodeRecentlySent
}

// Tunables for the rider auth flow. Exposed as a struct so tests can shorten
// timeouts without touching prod constants.
type RiderAuthConfig struct {
	CodeTTL              time.Duration // default 5m
	PerPhoneCooldown     time.Duration // default 60s
	PerPhoneHourlyMax    int           // default 5
	PerPhoneHourlyWindow time.Duration // default 1h
	MaxAttempts          int           // default 5 — after this many wrong codes the row is consumed
}

// DefaultRiderAuthConfig matches the spec.
//
// The throttles are on by default. Without them the login code is a small
// numeric secret with unlimited retries: an attacker who knows a phone number
// can request fresh codes in a loop and walk the whole code space. Callers that
// genuinely need no throttling must opt out explicitly with RiderAuthThrottleOff.
func DefaultRiderAuthConfig() RiderAuthConfig {
	return RiderAuthConfig{
		CodeTTL:              5 * time.Minute,
		PerPhoneCooldown:     60 * time.Second,
		PerPhoneHourlyMax:    5,
		PerPhoneHourlyWindow: time.Hour,
		MaxAttempts:          5,
	}
}

// RiderAuthThrottleOff disables a per-phone throttle when passed explicitly.
// Zero means "not set" (the default applies), so this sentinel is the only way
// to turn a limit off on purpose.
const RiderAuthThrottleOff = -1

// RiderAuthService implements the native rider login flow:
//
//	request-code  -> generate 6-digit OTP, persist hashed, deliver via rider bot
//	verify-code   -> compare hash, consume row, issue tokens
//	refresh       -> rotate refresh token via RiderAuthTokenService
//	logout        -> revoke all refresh tokens for the user
type RiderAuthService struct {
	db       *sql.DB
	codes    *repositories.RiderLoginCodesRepo
	tokens   *RiderAuthTokenService
	bot      riderbot.LoginCodeSender
	cfg      RiderAuthConfig
	now      func() time.Time
	randCode func() (string, error) // override-able for tests
}

// NewRiderAuthService wires the service. Any of bot, codes, tokens may NOT
// be nil in production. cfg defaults are used when zero-valued fields are
// passed.
func NewRiderAuthService(db *sql.DB, codes *repositories.RiderLoginCodesRepo, tokens *RiderAuthTokenService, bot riderbot.LoginCodeSender, cfg RiderAuthConfig) *RiderAuthService {
	def := DefaultRiderAuthConfig()
	if cfg.CodeTTL == 0 {
		cfg.CodeTTL = def.CodeTTL
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = def.MaxAttempts
	}
	// Throttles default to on; RiderAuthThrottleOff opts out explicitly.
	if cfg.PerPhoneCooldown == 0 {
		cfg.PerPhoneCooldown = def.PerPhoneCooldown
	}
	if cfg.PerPhoneHourlyMax == 0 {
		cfg.PerPhoneHourlyMax = def.PerPhoneHourlyMax
	}
	if cfg.PerPhoneHourlyWindow == 0 {
		cfg.PerPhoneHourlyWindow = def.PerPhoneHourlyWindow
	}
	if cfg.PerPhoneCooldown == RiderAuthThrottleOff {
		cfg.PerPhoneCooldown = 0
	}
	if cfg.PerPhoneHourlyMax == RiderAuthThrottleOff {
		cfg.PerPhoneHourlyMax = 0
	}
	if cfg.PerPhoneHourlyWindow == RiderAuthThrottleOff {
		cfg.PerPhoneHourlyWindow = 0
	}
	return &RiderAuthService{
		db:       db,
		codes:    codes,
		tokens:   tokens,
		bot:      bot,
		cfg:      cfg,
		now:      time.Now,
		randCode: generateLoginCode,
	}
}

// CodeTTL exposes the configured TTL (used by handler logging only).
func (s *RiderAuthService) CodeTTL() time.Duration { return s.cfg.CodeTTL }

// SetCodeGeneratorForTest overrides the OTP source. Tests that need to know
// the plaintext code (e.g. handler-level integration tests) call this with
// a deterministic generator. NOT for production use — callers should keep
// the default crypto/rand-backed generator.
func (s *RiderAuthService) SetCodeGeneratorForTest(fn func() (string, error)) {
	if fn == nil {
		return
	}
	s.randCode = fn
}

// RequestCodeResult is returned by RequestCode on success. It deliberately
// does NOT carry the plaintext code — the code only travels via Telegram.
type RequestCodeResult struct {
	UserID     int64
	TelegramID int64
}

// RequestCode is the implementation of POST /v1/rider/auth/request-code:
//
//  1. Normalize phone to E.164.
//  2. Look up users.phone (rejects with ErrRiderAuthPhoneNotFound or
//     ErrRiderAuthTelegramNotLink as appropriate).
//  3. Apply per-phone rate limits.
//  4. Generate a 6-digit code, persist sha256(salt + code).
//  5. Send via the rider bot. On 403 (bot blocked), the row is invalidated
//     and ErrRiderAuthBotBlocked is returned.
//
// The plaintext code never leaves this function except as a Telegram message.
func (s *RiderAuthService) RequestCode(ctx context.Context, rawPhone string) (*RequestCodeResult, error) {
	phone, ok := NormalizeUzbekPhone(rawPhone)
	if !ok {
		return nil, ErrRiderAuthInvalidPhone
	}
	uid, tgID, err := s.lookupRiderByPhone(ctx, phone)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()

	// Per-phone rate limiting (on by default; see DefaultRiderAuthConfig).
	if s.cfg.PerPhoneCooldown > 0 {
		last, err := s.codes.LastCreatedAtForPhone(ctx, phone)
		if err != nil {
			s.logOutcome("request_code_lookup_error", phone, uid, tgID, 0, "")
			return nil, ErrRiderAuthInternal
		}
		if last > 0 {
			elapsed := now.Sub(time.Unix(last, 0))
			if elapsed < s.cfg.PerPhoneCooldown {
				retry := s.cfg.PerPhoneCooldown - elapsed
				s.logOutcome("code_recently_sent", phone, uid, tgID, 0, fmt.Sprintf("retry_after=%ds", int(retry.Seconds())))
				return nil, &RiderAuthCodeRecentError{RetryAfter: retry}
			}
		}
	}
	if s.cfg.PerPhoneHourlyMax > 0 && s.cfg.PerPhoneHourlyWindow > 0 {
		since := now.Add(-s.cfg.PerPhoneHourlyWindow).Unix()
		count, err := s.codes.CountSinceForPhone(ctx, phone, since)
		if err != nil {
			s.logOutcome("request_code_count_error", phone, uid, tgID, 0, "")
			return nil, ErrRiderAuthInternal
		}
		if count >= s.cfg.PerPhoneHourlyMax {
			s.logOutcome("too_many_codes", phone, uid, tgID, 0, fmt.Sprintf("hourly=%d", count))
			return nil, ErrRiderAuthTooManyCodes
		}
	}

	// Generate code + per-row salt, then store hash only.
	code, err := s.randCode()
	if err != nil {
		s.logOutcome("code_gen_error", phone, uid, tgID, 0, "")
		return nil, ErrRiderAuthInternal
	}
	salt, err := generateSalt()
	if err != nil {
		s.logOutcome("salt_gen_error", phone, uid, tgID, 0, "")
		return nil, ErrRiderAuthInternal
	}
	codeHash := hashCode(code, salt)
	expiresAt := now.Add(s.cfg.CodeTTL).Unix()

	// Invalidate any older live rows so we never have two valid codes.
	_ = s.codes.InvalidateActiveForPhone(ctx, phone)

	rowID, err := s.codes.Insert(ctx, phone, codeHash, salt, expiresAt)
	if err != nil {
		s.logOutcome("code_store_error", phone, uid, tgID, 0, "")
		return nil, ErrRiderAuthInternal
	}

	outcome, sendErr := riderbot.SendLoginCode(s.bot, tgID, code)
	switch outcome {
	case riderbot.LoginCodeSent:
		s.logOutcome("code_sent", phone, uid, tgID, 0, "")
		return &RequestCodeResult{UserID: uid, TelegramID: tgID}, nil
	case riderbot.LoginCodeBotBlocked:
		_ = s.codes.MarkConsumed(ctx, rowID)
		s.logOutcome("bot_blocked", phone, uid, tgID, 0, fmt.Sprintf("tg_err=%q", errMsgOf(sendErr)))
		return nil, ErrRiderAuthBotBlocked
	default:
		_ = s.codes.MarkConsumed(ctx, rowID)
		s.logOutcome("send_failed", phone, uid, tgID, 0, fmt.Sprintf("tg_err=%q", errMsgOf(sendErr)))
		return nil, ErrRiderAuthSendFailed
	}
}

// errMsgOf returns err.Error() or "<nil>" so that audit logs always have a
// printable string for the tg_err field.
func errMsgOf(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// VerifyCode implements POST /v1/rider/auth/verify-code. On success it
// returns access_token, refresh_token, expires_in. On 5 failed attempts the
// active row is invalidated and ErrRiderAuthTooManyAttempts is returned.
func (s *RiderAuthService) VerifyCode(ctx context.Context, rawPhone, rawCode string) (*RiderAuthTokens, error) {
	phone, ok := NormalizeUzbekPhone(rawPhone)
	if !ok {
		return nil, ErrRiderAuthInvalidPhone
	}
	codeIn := strings.TrimSpace(rawCode)
	if len(codeIn) != riderOTPDigits || !isAllDigits(codeIn) {
		return nil, ErrRiderAuthInvalidCode
	}
	uid, tgID, err := s.lookupRiderByPhone(ctx, phone)
	if err != nil {
		return nil, err
	}

	now := s.now().UTC().Unix()
	row, err := s.codes.GetLatestActiveByPhone(ctx, phone, now)
	if err == repositories.ErrRiderLoginCodeNotFound {
		s.logOutcome("verify_no_active_code", phone, uid, tgID, 0, "")
		return nil, ErrRiderAuthInvalidCode
	}
	if err != nil {
		s.logOutcome("verify_lookup_error", phone, uid, tgID, 0, "")
		return nil, ErrRiderAuthInternal
	}

	// Hash submitted code with the row's salt and compare in constant time.
	submittedHash := hashCode(codeIn, row.Salt)
	if subtle.ConstantTimeCompare([]byte(row.CodeHash), []byte(submittedHash)) != 1 {
		attempts, _ := s.codes.IncAttempts(ctx, row.ID)
		if attempts >= s.cfg.MaxAttempts {
			_ = s.codes.MarkConsumed(ctx, row.ID)
			s.logOutcome("too_many_attempts", phone, uid, tgID, attempts, "")
			return nil, ErrRiderAuthTooManyAttempts
		}
		s.logOutcome("invalid_code", phone, uid, tgID, attempts, "")
		return nil, ErrRiderAuthInvalidCode
	}

	if err := s.codes.MarkConsumed(ctx, row.ID); err != nil {
		s.logOutcome("verify_consume_error", phone, uid, tgID, row.Attempts, "")
		return nil, ErrRiderAuthInternal
	}

	tokens, err := s.tokens.Issue(ctx, uid)
	if err != nil {
		s.logOutcome("verify_issue_error", phone, uid, tgID, row.Attempts, "")
		return nil, ErrRiderAuthInternal
	}
	s.logOutcome("verify_ok", phone, uid, tgID, row.Attempts, "")
	return tokens, nil
}

// Refresh implements POST /v1/rider/auth/refresh.
func (s *RiderAuthService) Refresh(ctx context.Context, refreshToken string) (*RiderAuthTokens, error) {
	tokens, err := s.tokens.Refresh(ctx, refreshToken)
	if err != nil {
		return nil, ErrRiderAuthInvalidCode
	}
	return tokens, nil
}

// Logout implements POST /v1/rider/auth/logout for the given user id.
func (s *RiderAuthService) Logout(ctx context.Context, userID int64) error {
	if err := s.tokens.RevokeAllForUser(ctx, userID); err != nil {
		return ErrRiderAuthInternal
	}
	return nil
}

// VerifyAccessToken parses a bearer access token and returns the rider user id.
func (s *RiderAuthService) VerifyAccessToken(token string) (int64, error) {
	return s.tokens.VerifyAccess(token)
}

// lookupRiderByPhone resolves a normalized E.164 phone to (userID, telegramID).
// The query tolerates legacy rows that were stored without the leading "+".
//
// Returns:
//
//	ErrRiderAuthPhoneNotFound   if no users row matches the phone
//	ErrRiderAuthTelegramNotLink if a row matches but telegram_id is 0/null
func (s *RiderAuthService) lookupRiderByPhone(ctx context.Context, phoneE164 string) (int64, int64, error) {
	if s.db == nil {
		return 0, 0, ErrRiderAuthInternal
	}
	digits := strings.TrimPrefix(phoneE164, "+")
	var (
		userID sql.NullInt64
		tgID   sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.telegram_id
		FROM users u
		WHERE replace(replace(replace(coalesce(u.phone, ''), '+', ''), ' ', ''), '-', '') = ?1
		ORDER BY
			CASE WHEN COALESCE(u.telegram_id, 0) > 0 THEN 1 ELSE 0 END DESC,
			u.id DESC
		LIMIT 1`,
		digits).Scan(&userID, &tgID)
	if err == sql.ErrNoRows {
		s.logOutcome("phone_not_registered", phoneE164, 0, 0, 0, "")
		return 0, 0, ErrRiderAuthPhoneNotFound
	}
	if err != nil {
		log.Printf("rider_auth lookup error phone=%s err=%v", maskPhone(phoneE164), err)
		return 0, 0, ErrRiderAuthInternal
	}
	if !userID.Valid || userID.Int64 <= 0 {
		s.logOutcome("phone_not_registered", phoneE164, 0, 0, 0, "")
		return 0, 0, ErrRiderAuthPhoneNotFound
	}
	if !tgID.Valid || tgID.Int64 == 0 {
		s.logOutcome("telegram_not_linked", phoneE164, userID.Int64, 0, 0, "")
		return userID.Int64, 0, ErrRiderAuthTelegramNotLink
	}
	return userID.Int64, tgID.Int64, nil
}

// logOutcome emits a single structured audit line per request. The plaintext
// code and code-hash are NEVER included. Phone is masked to last-4 digits.
func (s *RiderAuthService) logOutcome(outcome, phone string, userID, tgID int64, attempts int, extra string) {
	suffix := ""
	if extra != "" {
		suffix = " " + extra
	}
	log.Printf("rider_auth outcome=%s phone=%s user_id=%d telegram_id=%d attempts=%d%s",
		outcome, maskPhone(phone), userID, tgID, attempts, suffix)
}

// NormalizeUzbekPhone returns the E.164 form (+998XXXXXXXXX) and true on
// success. It accepts:
//
//	+998XXXXXXXXX     (already E.164)
//	998XXXXXXXXX      (missing leading +)
//	XXXXXXXXX         (9-digit local number, mobile-only)
//
// All other inputs return ("", false).
func NormalizeUzbekPhone(s string) (string, bool) {
	digits := stripNonDigits(s)
	switch len(digits) {
	case 9:
		// Local mobile number: prepend country code.
		return "+998" + digits, true
	case 12:
		if !strings.HasPrefix(digits, "998") {
			return "", false
		}
		return "+" + digits, true
	default:
		return "", false
	}
}

func stripNonDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// maskPhone returns the input with everything except the last 4 digits
// replaced by '*'. For "+998901234567" it returns "*********4567".
func maskPhone(p string) string {
	digits := stripNonDigits(p)
	if len(digits) <= 4 {
		return strings.Repeat("*", len(digits))
	}
	return strings.Repeat("*", len(digits)-4) + digits[len(digits)-4:]
}

// riderOTPDigits is the login code length. Four digits gave only 10,000
// possibilities, which a loop of request-code + 5 guesses can exhaust in
// minutes. Generation and verification must both use this constant — a
// mismatch rejects every real code while tests that inject a fixed code pass.
const riderOTPDigits = 6

// loginCodeSpace is the number of distinct login codes (10^riderOTPDigits).
const loginCodeSpace = 1000000

// generateLoginCode returns a zero-padded 6-digit code. rand.Int is used rather
// than a modulo of a random word so the distribution is uniform.
func generateLoginCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(loginCodeSpace))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// generateSalt returns a 32-character hex string for per-row salting.
func generateSalt() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// hashCode returns hex(sha256(salt || ":" || code)). Pure function so it can
// be used both for storage and constant-time verification.
func hashCode(code, salt string) string {
	h := sha256.New()
	h.Write([]byte(salt))
	h.Write([]byte{':'})
	h.Write([]byte(code))
	return hex.EncodeToString(h.Sum(nil))
}
