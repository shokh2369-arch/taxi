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

// TryTripScopedDriverID authenticates an approved driver from X-Driver-Id or ?driver_id=
// for read-only trip surfaces (GET /trip/:id), even when ENABLE_DRIVER_ID_HEADER is false.
// Mutating driver routes must keep using TryDriverIDHeader gated by the env flag.
// Invalid/unknown ids are ignored (request continues) so anonymous trip map loads still work.
func TryTripScopedDriverID(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if u := UserFromContext(c.Request.Context()); u != nil {
			c.Next()
			return
		}
		raw := strings.TrimSpace(c.GetHeader(HeaderDriverID))
		if raw == "" {
			for _, key := range []string{"driver_id", "x_driver_id"} {
				if q := strings.TrimSpace(c.Query(key)); q != "" {
					raw = q
					break
				}
			}
		}
		if raw == "" || db == nil {
			c.Next()
			return
		}
		uid, st := ResolveDriverUserForXDriverID(c.Request.Context(), db, raw)
		if st != XDriverIDOK {
			c.Next()
			return
		}
		c.Request = c.Request.WithContext(WithUser(c.Request.Context(), &User{
			UserID:         uid,
			TelegramUserID: 0,
			Role:           domain.RoleDriver,
		}))
		c.Next()
	}
}

// TryOptionalMiniAppAuthDriverOrRider is like RequireMiniAppAuthDriverOrRider but does not
// abort when initData is missing. Used by GET /trip/:id so legacy Mini App / native clients
// that omit auth (trip UUID capability URL) still load the map, while initData/Bearer still
// establish identity for participant ACL when present.
func TryOptionalMiniAppAuthDriverOrRider(db *sql.DB, driverBotToken, riderBotToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if u := UserFromContext(c.Request.Context()); u != nil {
			c.Next()
			return
		}
		initData := c.GetHeader(HeaderInitData)
		if initData == "" {
			initData = c.Query("init_data")
		}
		if initData == "" {
			c.Next()
			return
		}
		try := func(token string) bool {
			telegramUserID, err := VerifyMiniAppInitData(token, initData)
			if err != nil {
				return false
			}
			userID, role, err := ResolveUserFromTelegramID(c.Request.Context(), db, telegramUserID)
			if err != nil {
				return false
			}
			c.Request = c.Request.WithContext(WithUser(c.Request.Context(), &User{
				UserID:         userID,
				TelegramUserID: telegramUserID,
				Role:           role,
			}))
			return true
		}
		_ = try(driverBotToken) || try(riderBotToken)
		c.Next()
	}
}
