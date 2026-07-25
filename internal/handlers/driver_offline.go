package handlers

import (
	"database/sql"
	"log"
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

// DriverGoOnline clears a manual OFFLINE so the driver receives orders again.
//
// The counterpart to POST /driver/offline. It exists because location pings no
// longer clear manual_offline: a driver app reports position in the background,
// so treating any ping as "I'm back on shift" made the OFFLINE toggle undo
// itself. Going online is now an explicit action.
//
// Telegram live-location sharing also clears the flag, so a driver whose app has
// not been updated yet is never permanently stuck offline.
func DriverGoOnline(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			logger.AuthFailure("driver auth required")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		ctx := c.Request.Context()
		if _, err := db.ExecContext(ctx,
			`UPDATE drivers SET manual_offline = 0 WHERE user_id = ?1`, u.UserID); err != nil {
			log.Printf("driver_online: clear manual_offline driver_user_id=%d: %v", u.UserID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to go online"})
			return
		}
		// The driver still has to satisfy the usual dispatch gates (approved,
		// legal accepted, balance, fresh location), so report what they are
		// rather than implying orders will start arriving.
		c.JSON(http.StatusOK, gin.H{
			"ok":             true,
			"manual_offline": false,
			"note":           "Orders resume once your location is fresh and your balance is positive.",
		})
	}
}
