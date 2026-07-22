package auth

import (
	"database/sql"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/domain"
)

// RiderAccessVerifier validates native rider access JWTs.
type RiderAccessVerifier interface {
	VerifyAccessToken(token string) (int64, error)
}

// TryRiderBearerAuth sets RoleRider from Authorization: Bearer when valid.
// Missing or invalid Bearer continues so Mini App initData can authenticate.
func TryRiderBearerAuth(db *sql.DB, tokens RiderAccessVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if tokens == nil || db == nil {
			c.Next()
			return
		}
		if u := UserFromContext(c.Request.Context()); u != nil {
			c.Next()
			return
		}
		h := strings.TrimSpace(c.GetHeader("Authorization"))
		if !strings.HasPrefix(h, "Bearer ") {
			c.Next()
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if token == "" {
			c.Next()
			return
		}
		userID, err := tokens.VerifyAccessToken(token)
		if err != nil || userID <= 0 {
			c.Next()
			return
		}
		var role string
		if err := db.QueryRowContext(c.Request.Context(), `SELECT role FROM users WHERE id = ?1`, userID).Scan(&role); err != nil {
			c.Next()
			return
		}
		if strings.TrimSpace(role) != domain.RoleRider {
			c.Next()
			return
		}
		c.Request = c.Request.WithContext(WithUser(c.Request.Context(), &User{
			UserID:         userID,
			TelegramUserID: 0,
			Role:           domain.RoleRider,
		}))
		c.Next()
	}
}
