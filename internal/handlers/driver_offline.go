package handlers

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/logger"
)

// DriverManualOffline handles POST /driver/offline — same DB effect as Telegram live location end
// (handleLiveLocationUpdate when live_period <= 0): driver is not eligible for dispatch until they
// go online again (HTTP location + PulseDriverOnlineFromHTTP, or Telegram live).
// Native apps must call this when the driver toggles OFFLINE; stopping POST /driver/location alone
// leaves live_location_active / is_active stale for up to ~90s and admin maps still show "online + live".
func DriverManualOffline(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			logger.AuthFailure("driver auth required")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		ctx := c.Request.Context()
		_, err := db.ExecContext(ctx, `
			UPDATE drivers
			SET is_active = 0, manual_offline = 1,
			    live_location_active = 0, last_live_location_at = NULL,
			    app_location_active = 0
			WHERE user_id = ?1`, u.UserID)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such column") {
			// Backward compatible for tests / unmigrated DB: app_location_active may not exist yet.
			_, err = db.ExecContext(ctx, `
				UPDATE drivers
				SET is_active = 0, manual_offline = 1,
				    live_location_active = 0, last_live_location_at = NULL
				WHERE user_id = ?1`, u.UserID)
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
