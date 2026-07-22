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
// when a valid opaque OTP session token is presented. On missing or invalid token,
// continues so rider Bearer / initData / X-Driver-Id can run.
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
			c.Next()
			return
		}
		var ver sql.NullString
		err = db.QueryRowContext(c.Request.Context(), `
			SELECT verification_status FROM drivers WHERE user_id = ?1`, userID).Scan(&ver)
		if err != nil || !strings.EqualFold(strings.TrimSpace(ver.String), "approved") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "driver not approved"})
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
	return strings.TrimSpace(c.GetHeader(HeaderDriverSession))
}
