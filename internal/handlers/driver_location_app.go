package handlers

import (
	"database/sql"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/logger"
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
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		var req DriverAppLocationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		if !validLatLng(req.Lat, req.Lng) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid lat/lng"})
			return
		}

		ctx := c.Request.Context()
		driverID := u.UserID
		nowStr := time.Now().UTC().Format("2006-01-02 15:04:05")

		// Strictly update only the app_* fields. Do NOT touch Telegram fields.
		_, err := db.ExecContext(ctx, `
			UPDATE drivers
			SET app_lat = ?1, app_lng = ?2, app_last_seen_at = ?3, app_location_active = 1
			WHERE user_id = ?4`,
			req.Lat, req.Lng, nowStr, driverID)
		if err != nil {
			log.Printf("driver_app_location: update failed driver_user_id=%d err=%v", driverID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

