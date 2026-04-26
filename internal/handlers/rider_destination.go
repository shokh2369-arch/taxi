package handlers

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
	"taxi-mvp/internal/utils"
)

type riderSetDestinationBody struct {
	RequestID string  `json:"request_id"`
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	Name      string  `json:"name"`
	InitData  string  `json:"init_data"`
}

func isMissingColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column")
}

func validLatLngRiderDestination(lat, lng float64) bool {
	return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
}

// RiderSetDestination sets drop point + estimated_price for a PENDING ride request using Telegram Mini App init_data.
// This is additive and does not touch trip lifecycle or settlement logic.
// It does NOT dispatch to drivers; the rider must confirm after seeing the estimate.
func RiderSetDestination(db *sql.DB, cfg *config.Config, riderBot *tgbotapi.BotAPI, fareSvc *services.FareService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if db == nil || cfg == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "service unavailable"})
			return
		}
		var body riderSetDestinationBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid body"})
			return
		}
		body.RequestID = strings.TrimSpace(body.RequestID)
		body.Name = strings.TrimSpace(body.Name)
		body.InitData = strings.TrimSpace(body.InitData)
		if body.RequestID == "" || body.InitData == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "request_id and init_data required"})
			return
		}
		if !validLatLngRiderDestination(body.Lat, body.Lng) {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid lat/lng"})
			return
		}

		tgID, err := auth.VerifyMiniAppInitData(cfg.RiderBotToken, body.InitData)
		if err != nil || tgID == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "invalid init_data"})
			return
		}

		ctx := c.Request.Context()

		var riderUserID int64
		if err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE telegram_id = ?1`, tgID).Scan(&riderUserID); err != nil || riderUserID == 0 {
			c.JSON(http.StatusForbidden, gin.H{"ok": false, "error": "rider not found"})
			return
		}

		// Load request and validate ownership + status.
		var pickupLat, pickupLng float64
		var status string
		var expiresAt string
		var dropLat, dropLng sql.NullFloat64
		err = db.QueryRowContext(ctx, `
			SELECT pickup_lat, pickup_lng, status, COALESCE(expires_at,''), drop_lat, drop_lng
			FROM ride_requests
			WHERE id = ?1 AND rider_user_id = ?2`,
			body.RequestID, riderUserID).Scan(&pickupLat, &pickupLng, &status, &expiresAt, &dropLat, &dropLng)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "request not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": "db error"})
			return
		}
		if status != domain.RequestStatusPending {
			c.JSON(http.StatusConflict, gin.H{"ok": false, "error": "request not pending"})
			return
		}
		// NOTE: We intentionally do NOT block destination selection when expires_at has passed.
		// Render free instances can sleep and the Mini App can open late; once the rider confirms destination,
		// we refresh expires_at from "now" so dispatch/confirm remains race-safe.
		// Allow overwriting destination until rider confirms the estimate.
		var confirmed int
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(destination_confirmed, 0) FROM ride_requests WHERE id = ?1`, body.RequestID).Scan(&confirmed); err != nil {
			// Backward compatible: column may not exist yet.
			confirmed = 0
		}
		if confirmed == 1 {
			c.JSON(http.StatusConflict, gin.H{"ok": false, "error": "destination already confirmed"})
			return
		}

		// Estimate price using existing pricing logic.
		distanceKm := utils.HaversineMeters(pickupLat, pickupLng, body.Lat, body.Lng) / 1000
		var est int64
		if fareSvc != nil {
			if v, err := fareSvc.CalculateFare(ctx, distanceKm); err == nil && v > 0 {
				est = v
			}
		}
		if est <= 0 {
			est = utils.CalculateFareRounded(float64(cfg.StartingFee), float64(cfg.PricePerKm), distanceKm)
		}

		// Reset TTL from destination selection moment, so users can browse without expiring.
		ttl := "+120 seconds"
		if cfg != nil && cfg.RequestExpiresSeconds > 0 {
			ttl = "+" + strconv.Itoa(cfg.RequestExpiresSeconds) + " seconds"
		}

		// Persist destination and estimate. If schema is not migrated yet, fail fast with a clear error.
		// Prefer update with destination_confirmed reset; fall back if column missing.
		_, err = db.ExecContext(ctx, `
			UPDATE ride_requests
			SET drop_lat = ?1, drop_lng = ?2, drop_name = ?3, estimated_price = ?4, expires_at = datetime('now', ?5),
			    destination_confirmed = 0
			WHERE id = ?6 AND rider_user_id = ?7 AND status = ?8
			  AND COALESCE(destination_confirmed, 0) = 0`,
			body.Lat, body.Lng, body.Name, est, ttl, body.RequestID, riderUserID, domain.RequestStatusPending)
		if err != nil && isMissingColumnErr(err) {
			_, err = db.ExecContext(ctx, `
				UPDATE ride_requests
				SET drop_lat = ?1, drop_lng = ?2, drop_name = ?3, estimated_price = ?4, expires_at = datetime('now', ?5)
				WHERE id = ?6 AND rider_user_id = ?7 AND status = ?8`,
				body.Lat, body.Lng, body.Name, est, ttl, body.RequestID, riderUserID, domain.RequestStatusPending)
		}
		if err != nil {
			if isMissingColumnErr(err) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "db migration required"})
				return
			}
			log.Printf("rider_destination: update request=%s err=%v", body.RequestID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": "update failed"})
			return
		}

		// Notify rider in Telegram to confirm the estimate (works even if WebAppData sendData doesn't arrive).
		if riderBot != nil {
			kb := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("✅ Тасдиқлаш", "req_confirm:"+body.RequestID),
					tgbotapi.NewInlineKeyboardButtonData("◀️ Ўзгартириш", "req_change:"+body.RequestID),
				),
			)
			m := tgbotapi.NewMessage(tgID, "💰 Тахминий нарх: "+strconv.FormatInt(est, 10)+"\n\nТасдиқлайсизми?")
			m.ReplyMarkup = kb
			if _, err := riderBot.Send(m); err != nil {
				log.Printf("rider_destination: notify rider chat=%d err=%v", tgID, err)
			}
		}

		c.JSON(http.StatusOK, gin.H{"ok": true, "estimated_price": est})
	}
}

