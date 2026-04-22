package handlers

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	driverOTPDigits            = 6
	errDriverAuthNotRegistered = "DRIVER_NOT_REGISTERED"
	errDriverAuthInvalidCode   = "INVALID_CODE"
	errDriverAuthRateLimited   = "RATE_LIMITED"
	errDriverAuthNoTelegram    = "NO_TELEGRAM"
	errDriverAuthTelegramFail  = "TELEGRAM_SEND_FAILED"
	errDriverAuthInvalidPhone  = "INVALID_PHONE"
	errDriverAuthInvalidBody   = "INVALID_BODY"
	errDriverAuthInternal      = "INTERNAL_ERROR"
	errDriverAuthTelegramDown  = "TELEGRAM_UNAVAILABLE"
)

func writeAPIError(c *gin.Context, status int, code, message string) {
	if strings.TrimSpace(code) == "" {
		code = errDriverAuthInternal
	}
	if strings.TrimSpace(message) == "" {
		message = "operation failed"
	}
	// Keep backward compatibility: include "error" field in addition to "code".
	c.JSON(status, gin.H{"ok": false, "code": code, "message": message, "error": code})
}

// DriverAuthRequestCodeBody is the JSON body for POST /auth/request-code.
type DriverAuthRequestCodeBody struct {
	Phone string `json:"phone"`
}

// DriverAuthVerifyCodeBody is the JSON body for POST /auth/verify-code.
type DriverAuthVerifyCodeBody struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

// normalizePhoneDigits returns digits-only canonical form for matching (Uzbek mobiles: 9 digits → 998 prefix).
func normalizePhoneDigits(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) == 9 && d[0] == '9' {
		return "998" + d
	}
	return d
}

type driverLookup struct {
	UserID             int64
	TelegramID         int64
	UserRole           string
	VerificationStatus sql.NullString
	HasDriverRow       bool
}

func lookupDriverByPhoneDigits(ctx context.Context, db *sql.DB, digits string) (*driverLookup, error) {
	if digits == "" {
		return nil, sql.ErrNoRows
	}
	var out driverLookup
	var ver sql.NullString
	var driverUserID sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT u.id, u.telegram_id, u.role,
		       d.verification_status, d.user_id
		FROM users u
		LEFT JOIN drivers d ON d.user_id = u.id
		WHERE (
			replace(replace(replace(coalesce(u.phone, ''), '+', ''), ' ', ''), '-', '') = ?1
			OR replace(replace(replace(coalesce(d.phone, ''), '+', ''), ' ', ''), '-', '') = ?1
		)
		LIMIT 1`,
		digits).Scan(&out.UserID, &out.TelegramID, &out.UserRole, &ver, &driverUserID)
	if err != nil {
		return nil, err
	}
	out.VerificationStatus = ver
	out.HasDriverRow = driverUserID.Valid && driverUserID.Int64 != 0
	return &out, nil
}

func isApprovedDriver(l *driverLookup) bool {
	if l == nil {
		return false
	}
	if l.UserID == 0 || !l.HasDriverRow || l.UserRole != "driver" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(l.VerificationStatus.String), "approved")
}

func generateOTP6() (string, error) {
	var n uint32
	if err := binary.Read(rand.Reader, binary.BigEndian, &n); err != nil {
		return "", err
	}
	v := int(n % 1000000)
	return fmt.Sprintf("%06d", v), nil
}

// telegramSender is implemented by *tgbotapi.BotAPI; narrowed for tests.
type telegramSender interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
}

// DriverAuthRequestCode sends a 6-digit OTP via the existing driver Telegram bot.
func DriverAuthRequestCode(db *sql.DB, driverBot telegramSender) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body DriverAuthRequestCodeBody
		if err := c.ShouldBindJSON(&body); err != nil {
			writeAPIError(c, http.StatusBadRequest, errDriverAuthInvalidBody, "invalid body")
			return
		}
		digits := normalizePhoneDigits(body.Phone)
		if digits == "" {
			writeAPIError(c, http.StatusBadRequest, errDriverAuthInvalidPhone, "invalid phone")
			return
		}
		ctx := c.Request.Context()
		lookup, err := lookupDriverByPhoneDigits(ctx, db, digits)
		if err == sql.ErrNoRows || !isApprovedDriver(lookup) {
			// One structured reject log line (no OTP/code values).
			log.Printf("driver_auth_request_code_reject phone=%s user_found=%v driver_approved=%v status=%d code=%s",
				digits, err != sql.ErrNoRows, isApprovedDriver(lookup), http.StatusForbidden, errDriverAuthNotRegistered)
			writeAPIError(c, http.StatusForbidden, errDriverAuthNotRegistered, "driver not registered or not approved")
			return
		}
		if err != nil {
			log.Printf("driver_auth_request_code_reject phone=%s user_found=%v driver_approved=%v status=%d code=%s detail=%v",
				digits, false, false, http.StatusInternalServerError, errDriverAuthInternal, err)
			writeAPIError(c, http.StatusInternalServerError, errDriverAuthInternal, "lookup failed")
			return
		}
		userID := lookup.UserID
		telegramID := lookup.TelegramID
		if telegramID == 0 {
			log.Printf("driver_auth_request_code_reject phone=%s user_found=%v driver_approved=%v status=%d code=%s",
				digits, true, true, http.StatusBadRequest, errDriverAuthNoTelegram)
			writeAPIError(c, http.StatusBadRequest, errDriverAuthNoTelegram, "driver has no Telegram linked")
			return
		}
		if driverBot == nil {
			log.Printf("driver_auth_request_code_reject phone=%s user_found=%v driver_approved=%v status=%d code=%s",
				digits, true, true, http.StatusServiceUnavailable, errDriverAuthTelegramDown)
			writeAPIError(c, http.StatusServiceUnavailable, errDriverAuthTelegramDown, "telegram unavailable")
			return
		}

		var recent int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM driver_login_codes
			WHERE user_id = ? AND created_at > datetime('now', '-30 seconds')`,
			userID).Scan(&recent); err == nil && recent > 0 {
			log.Printf("driver_auth_request_code_reject phone=%s user_found=%v driver_approved=%v status=%d code=%s",
				digits, true, true, http.StatusTooManyRequests, errDriverAuthRateLimited)
			writeAPIError(c, http.StatusTooManyRequests, errDriverAuthRateLimited, "rate limited")
			return
		}

		code, err := generateOTP6()
		if err != nil {
			log.Printf("driver_auth_request_code_reject phone=%s user_found=%v driver_approved=%v status=%d code=%s detail=%v",
				digits, true, true, http.StatusInternalServerError, errDriverAuthInternal, err)
			writeAPIError(c, http.StatusInternalServerError, errDriverAuthInternal, "otp generation failed")
			return
		}

		_, _ = db.ExecContext(ctx, `UPDATE driver_login_codes SET used = 1 WHERE user_id = ? AND used = 0`, userID)

		res, err := db.ExecContext(ctx, `
			INSERT INTO driver_login_codes (user_id, code, expires_at)
			VALUES (?, ?, datetime('now', '+3 minutes'))`,
			userID, code)
		if err != nil {
			log.Printf("driver_auth: request-code storage error: %v", err)
			writeAPIError(c, http.StatusInternalServerError, errDriverAuthInternal, "storage failed")
			return
		}
		id, _ := res.LastInsertId()

		msg := tgbotapi.NewMessage(telegramID, fmt.Sprintf("Your YettiQanot login code: %s (expires in 3 minutes)", code))
		if _, err := driverBot.Send(msg); err != nil {
			_, _ = db.ExecContext(ctx, `DELETE FROM driver_login_codes WHERE id = ?`, id)
			writeAPIError(c, http.StatusBadGateway, errDriverAuthTelegramFail, "telegram send failed")
			return
		}

		// Do not log OTP or code value.
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// DriverAuthVerifyCode validates OTP and returns internal driver user id for X-Driver-Id.
func DriverAuthVerifyCode(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body DriverAuthVerifyCodeBody
		if err := c.ShouldBindJSON(&body); err != nil {
			writeAPIError(c, http.StatusBadRequest, errDriverAuthInvalidBody, "invalid body")
			return
		}
		digits := normalizePhoneDigits(body.Phone)
		codeIn := strings.TrimSpace(body.Code)
		if digits == "" || len(codeIn) != driverOTPDigits {
			writeAPIError(c, http.StatusBadRequest, errDriverAuthInvalidCode, "invalid code")
			return
		}
		ctx := c.Request.Context()
		lookup, err := lookupDriverByPhoneDigits(ctx, db, digits)
		if err == sql.ErrNoRows || !isApprovedDriver(lookup) {
			writeAPIError(c, http.StatusForbidden, errDriverAuthNotRegistered, "driver not registered or not approved")
			return
		}
		if err != nil {
			log.Printf("driver_auth: verify-code lookup error: %v", err)
			writeAPIError(c, http.StatusInternalServerError, errDriverAuthInternal, "lookup failed")
			return
		}
		userID := lookup.UserID

		var rowID int64
		var stored string
		err = db.QueryRowContext(ctx, `
			SELECT id, code FROM driver_login_codes
			WHERE user_id = ? AND used = 0 AND expires_at > datetime('now')
			ORDER BY id DESC LIMIT 1`,
			userID).Scan(&rowID, &stored)
		if err == sql.ErrNoRows {
			writeAPIError(c, http.StatusBadRequest, errDriverAuthInvalidCode, "invalid code")
			return
		}
		if err != nil {
			log.Printf("driver_auth: verify-code code row error: %v", err)
			writeAPIError(c, http.StatusInternalServerError, errDriverAuthInternal, "lookup failed")
			return
		}

		if subtle.ConstantTimeCompare([]byte(stored), []byte(codeIn)) != 1 {
			writeAPIError(c, http.StatusBadRequest, errDriverAuthInvalidCode, "invalid code")
			return
		}

		_, err = db.ExecContext(ctx, `UPDATE driver_login_codes SET used = 1 WHERE id = ?`, rowID)
		if err != nil {
			writeAPIError(c, http.StatusInternalServerError, errDriverAuthInternal, "update failed")
			return
		}

		c.JSON(http.StatusOK, gin.H{"driver_id": userID})
	}
}
