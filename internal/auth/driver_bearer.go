package auth

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/domain"
)

// DriverTokenVerifier validates opaque driver bearer tokens issued by OTP verify.
type DriverTokenVerifier interface {
	Verify(ctx context.Context, token string) (int64, error)
}

// TryDriverBearerAuth sets the driver from Authorization: Bearer or X-Driver-Session
// when a valid opaque OTP session token is presented. On a missing token it
// continues so rider Bearer / initData / X-Driver-Id can run.
//
// A token that unambiguously claims a DRIVER session (X-Driver-Session header,
// or a dot-free opaque token in Authorization/access_token — rider JWTs always
// contain dots) but fails verification aborts with 401 AUTH_SESSION_EXPIRED:
// that is the stable signal the app uses to sign the device out, e.g. after a
// login on another device revoked this session (single-session).
func TryDriverBearerAuth(db *sql.DB, tokens DriverTokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if tokens == nil || db == nil {
			c.Next()
			return
		}
		if u := UserFromContext(c.Request.Context()); u != nil && u.Role == domain.RoleDriver {
			c.Next()
			return
		}
		raw := driverBearerFromRequest(c)
		if raw == "" {
			c.Next()
			return
		}
		userID, err := tokens.Verify(c.Request.Context(), raw)
		if err != nil || userID <= 0 {
			if !strings.Contains(raw, ".") {
				// Opaque driver-session shape: expired, revoked, or replaced.
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"ok":      false,
					"code":    "AUTH_SESSION_EXPIRED",
					"error":   "AUTH_SESSION_EXPIRED",
					"message": "session expired or signed in on another device",
				})
				return
			}
			// JWT-shaped token: not a driver session — let rider auth try it.
			c.Next()
			return
		}
		var ver sql.NullString
		err = db.QueryRowContext(c.Request.Context(), `
			SELECT verification_status FROM drivers WHERE user_id = ?1`, userID).Scan(&ver)
		if err != nil || !strings.EqualFold(strings.TrimSpace(ver.String), "approved") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"ok":      false,
				"code":    "DRIVER_NOT_APPROVED",
				"error":   "driver not approved",
				"message": "driver registration pending approval",
			})
			return
		}
		c.Request = c.Request.WithContext(WithUser(c.Request.Context(), &User{
			UserID:         userID,
			TelegramUserID: 0,
			Role:           domain.RoleDriver,
		}))
		c.Next()
	}
}

func driverBearerFromRequest(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	h := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(h, "Bearer ") {
		if t := strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")); t != "" {
			return t
		}
	}
	if t := strings.TrimSpace(c.GetHeader(HeaderDriverSession)); t != "" {
		return t
	}
	// Match /ws ?access_token= fallback used by native clients that cannot set WS headers.
	return strings.TrimSpace(c.Query("access_token"))
}
