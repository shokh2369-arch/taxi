package handlers

import (
	"database/sql"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/logger"
	"taxi-mvp/internal/utils"
)

// DriverAppLocationRequest is the JSON body for POST /driver/location/app. driver_id comes from auth context.
type DriverAppLocationRequest struct {
	Lat       float64 `json:"lat" binding:"required"`
	Lng       float64 `json:"lng" binding:"required"`
	Accuracy  float64 `json:"accuracy"`  // optional; stored client-side only today
	Timestamp float64 `json:"timestamp"` // optional; accepted for forward compatibility
}

func validLatLng(lat, lng float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lng) || math.IsInf(lat, 0) || math.IsInf(lng, 0) {
		return false
	}
	return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
}

// DriverAppLocation updates ONLY native-app location fields on drivers (additive; Telegram fields untouched).
// POST /driver/location/app
func DriverAppLocation(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			logger.AuthFailure("driver auth required")
			c.JSON(http.StatusUnauthorized, gin.H{"code": "UNAUTHORIZED", "message": "driver auth required"})
			return
		}
		var req DriverAppLocationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_BODY", "message": "invalid body"})
			return
		}
		if !validLatLng(req.Lat, req.Lng) {
			c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_LAT_LNG", "message": "invalid lat/lng"})
			return
		}

		ctx := c.Request.Context()
		driverID := u.UserID
		nowStr := time.Now().UTC().Format("2006-01-02 15:04:05")
		gridID := utils.GridID(req.Lat, req.Lng)

		// Ensure drivers row exists for this user (some setups create users first).
		_, _ = db.ExecContext(ctx, `INSERT OR IGNORE INTO drivers (user_id) VALUES (?1)`, driverID)

		// Update app_* fields and mark driver online for dispatch (is_active/manual_offline/grid/last_seen_at).
		// Telegram live fields remain untouched (last_lat/last_lng/live_location_active/last_live_location_at).
		res, err := db.ExecContext(ctx, `
			UPDATE drivers
			SET app_lat = ?1, app_lng = ?2, app_last_seen_at = ?3, app_location_active = 1,
			    last_seen_at = ?3, grid_id = ?4, is_active = 1, manual_offline = 0
			WHERE user_id = ?5`,
			req.Lat, req.Lng, nowStr, gridID, driverID)
		if err != nil {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column") {
				log.Printf("driver_app_location: schema missing driver_user_id=%d err=%v", driverID, err)
				c.JSON(http.StatusServiceUnavailable, gin.H{"code": "MIGRATION_REQUIRED", "message": "driver app location fields not migrated"})
				return
			}
			log.Printf("driver_app_location: update failed driver_user_id=%d err=%v", driverID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": "UPDATE_FAILED", "message": "failed to update driver location"})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"code": "DRIVER_NOT_FOUND", "message": "driver not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

