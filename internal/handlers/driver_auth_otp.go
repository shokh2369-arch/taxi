package handlers

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	driverOTPDigits            = 6
	errDriverAuthNotRegistered = "NOT_REGISTERED"
	errDriverAuthInvalidCode   = "INVALID_CODE"
	errDriverAuthRateLimited   = "RATE_LIMITED"
	errDriverAuthNoTelegram    = "NO_TELEGRAM"
	errDriverAuthTelegramFail  = "TELEGRAM_SEND_FAILED"
)

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

func findApprovedDriverUserByPhoneDigits(ctx context.Context, db *sql.DB, digits string) (userID, telegramID int64, err error) {
	if digits == "" {
		return 0, 0, sql.ErrNoRows
	}
	err = db.QueryRowContext(ctx, `
		SELECT u.id, u.telegram_id
		FROM users u
		INNER JOIN drivers d ON d.user_id = u.id
		WHERE u.role = 'driver'
		  AND d.verification_status = 'approved'
		  AND (
			replace(replace(replace(coalesce(u.phone, ''), '+', ''), ' ', ''), '-', '') = ?1
			OR replace(replace(replace(coalesce(d.phone, ''), '+', ''), ' ', ''), '-', '') = ?1
		  )
		LIMIT 1`,
		digits).Scan(&userID, &telegramID)
	return userID, telegramID, err
}

func generateOTP6() (string, error) {
	var n uint32
	if err := binary.Read(rand.Reader, binary.BigEndian, &n); err != nil {
		return "", err
	}
	v := int(n % 1000000)
	return fmt.Sprintf("%06d", v), nil
}

// DriverAuthRequestCode sends a 6-digit OTP via the existing driver Telegram bot.
func DriverAuthRequestCode(db *sql.DB, driverBot *tgbotapi.BotAPI) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body DriverAuthRequestCodeBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		digits := normalizePhoneDigits(body.Phone)
		if digits == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid phone"})
			return
		}
		ctx := c.Request.Context()
		userID, telegramID, err := findApprovedDriverUserByPhoneDigits(ctx, db, digits)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errDriverAuthNotRegistered})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
			return
		}
		if telegramID == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": errDriverAuthNoTelegram})
			return
		}
		if driverBot == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "telegram unavailable"})
			return
		}

		var recent int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM driver_login_codes
			WHERE user_id = ?1 AND created_at > datetime('now', '-30 seconds')`,
			userID).Scan(&recent); err == nil && recent > 0 {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": errDriverAuthRateLimited})
			return
		}

		code, err := generateOTP6()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "otp generation failed"})
			return
		}

		_, _ = db.ExecContext(ctx, `UPDATE driver_login_codes SET used = 1 WHERE user_id = ?1 AND used = 0`, userID)

		res, err := db.ExecContext(ctx, `
			INSERT INTO driver_login_codes (user_id, code, expires_at)
			VALUES (?1, ?2, datetime('now', '+3 minutes'))`,
			userID, code)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "storage failed"})
			return
		}
		id, _ := res.LastInsertId()

		msg := tgbotapi.NewMessage(telegramID, fmt.Sprintf("Your YettiQanot login code: %s (expires in 3 minutes)", code))
		if _, err := driverBot.Send(msg); err != nil {
			_, _ = db.ExecContext(ctx, `DELETE FROM driver_login_codes WHERE id = ?1`, id)
			c.JSON(http.StatusBadGateway, gin.H{"error": errDriverAuthTelegramFail})
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		digits := normalizePhoneDigits(body.Phone)
		codeIn := strings.TrimSpace(body.Code)
		if digits == "" || len(codeIn) != driverOTPDigits {
			c.JSON(http.StatusUnauthorized, gin.H{"error": errDriverAuthInvalidCode})
			return
		}
		ctx := c.Request.Context()
		userID, _, err := findApprovedDriverUserByPhoneDigits(ctx, db, digits)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"error": errDriverAuthInvalidCode})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
			return
		}

		var rowID int64
		var stored string
		err = db.QueryRowContext(ctx, `
			SELECT id, code FROM driver_login_codes
			WHERE user_id = ?1 AND used = 0 AND expires_at > datetime('now')
			ORDER BY id DESC LIMIT 1`,
			userID).Scan(&rowID, &stored)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"error": errDriverAuthInvalidCode})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
			return
		}

		if subtle.ConstantTimeCompare([]byte(stored), []byte(codeIn)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": errDriverAuthInvalidCode})
			return
		}

		_, err = db.ExecContext(ctx, `UPDATE driver_login_codes SET used = 1 WHERE id = ?1`, rowID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"driver_id": userID})
	}
}
