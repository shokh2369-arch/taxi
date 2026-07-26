package handlers

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
)

// RequireRiderBearerAuth validates Authorization: Bearer <access_token> from
// the native rider auth flow (/v1/rider/auth/verify-code) and attaches
// auth.User with RoleRider and UserID. TelegramUserID is left 0 (not used by
// these handlers).
func RequireRiderBearerAuth(svc *services.RiderAuthService, db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if svc == nil || db == nil {
			writeRiderAPIError(c, http.StatusServiceUnavailable, "service_unavailable", "Xizmat vaqtincha ishlamayapti.")
			c.Abort()
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		if token == "" {
			writeRiderAPIError(c, http.StatusUnauthorized, "invalid_token", "Kirish talab qilinadi. Qaytadan tizimga kiring.")
			c.Abort()
			return
		}
		userID, err := svc.VerifyAccessToken(token)
		if err != nil || userID <= 0 {
			writeRiderAPIError(c, http.StatusUnauthorized, "invalid_token", "Sessiya muddati tugagan yoki token noto‘g‘ri. Qaytadan tizimga kiring.")
			c.Abort()
			return
		}
		// Existence only — do not gate on users.role. The same phone/Telegram row
		// is often flipped to role=driver when the person also opens the driver bot
		// (UPSERT overwrites role). Rider OTP still issues tokens for that row;
		// requiring role=rider then 403s every /v1/rider/* call after a successful
		// refresh (matches production: refresh 200 → trips/active 403).
		// Credential type is the authority here, same as driver OTP ignoring role.
		var exists int
		if err := db.QueryRowContext(c.Request.Context(), `SELECT 1 FROM users WHERE id = ?1`, userID).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				writeRiderAPIError(c, http.StatusForbidden, "forbidden", "Foydalanuvchi topilmadi.")
				c.Abort()
				return
			}
			writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
			c.Abort()
			return
		}
		ctx := c.Request.Context()
		nowStr := time.Now().UTC().Format("2006-01-02 15:04:05")
		_, _ = db.ExecContext(ctx, `UPDATE users SET rider_app_last_seen_at = ?1 WHERE id = ?2`, nowStr, userID)
		ctx = auth.WithActionSource(ctx, auth.ActionSourceHTTPApp)
		c.Request = c.Request.WithContext(auth.WithUser(ctx, &auth.User{
			UserID:         userID,
			TelegramUserID: 0,
			Role:           domain.RoleRider,
		}))
		c.Next()
	}
}

// writeRiderAPIError writes { "error": { "code", "message" } } for v1 rider routes.
func writeRiderAPIError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// writeRiderAPIErrorWithDetails is writeRiderAPIError plus extra fields inside the
// error object, for cases where the client can act on the detail (for example the
// id of the pending request that is blocking a new one).
func writeRiderAPIErrorWithDetails(c *gin.Context, status int, code, message string, details gin.H) {
	body := gin.H{"code": code, "message": message}
	for k, v := range details {
		body[k] = v
	}
	c.JSON(status, gin.H{"error": body})
}
