package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
	"taxi-mvp/internal/utils"
	"taxi-mvp/internal/ws"
)

// Cap each Turso round-trip so a wedged connection fails fast instead of
// waiting for the mobile client's ~15s timeout and then returning 500.
const availableRequestsQueryTimeout = 8 * time.Second

// OpenAPI (informal) — Driver dispatch HTTP
//
// GET /driver/available-requests:
//
//	200: { assigned_trip: null | { trip_id, status }, available_requests, requests, pending_requests, queue, orders, jobs: Offer[] }
//	Offer: { request_id, trip_id?, pickup_lat, pickup_lng, distance_km, radius_km, expires_at? }
//
// POST /driver/accept-request:
//
//	body: { trip_id?, request_id? }
//	200: { ok, trip_id, request_id?, assigned?, result?, status? } | idempotent already_assigned
//	400: { ok: false, error, request_id? } | { error: invalid body | ... }
//	403/404: trip_id-only branch
//	409: { ok: false, error: "request no longer available", request_id }
//	409: { ok: false, error: "driver_has_active_trip", message, active_trip_id, request_id }
//	503: assignment unavailable
//
// DriverAcceptRequestBody is accepted for POST /driver/accept-request. At least one of trip_id or request_id should be set.
type DriverAcceptRequestBody struct {
	TripID    string `json:"trip_id"`
	RequestID string `json:"request_id"`
}

// DriverAvailableOffer is one pending offer for the driver (same underlying rows as Telegram dispatch).
type DriverAvailableOffer struct {
	RequestID      string  `json:"request_id"`
	TripID         string  `json:"trip_id,omitempty"`
	PickupLat      float64 `json:"pickup_lat"`
	PickupLng      float64 `json:"pickup_lng"`
	DistanceKm     float64 `json:"distance_km"`
	RadiusKm       float64 `json:"radius_km"`
	EstimatedPrice int64   `json:"estimated_price"`
	ExpiresAt      string  `json:"expires_at,omitempty"`
}

// DriverAssignedTripStub is optional context for an in-progress assignment (Flutter may call GET /trip/:id for full detail).
type DriverAssignedTripStub struct {
	TripID string `json:"trip_id"`
	Status string `json:"status"`
}

// DriverAvailableRequests returns pending offers (request_notifications SENT + PENDING request) and optional active trip stub.
// Offers are only visible within DispatchOfferVisibleSeconds of request_notifications.created_at.
func DriverAvailableRequests(db *sql.DB, cfg *config.Config, fareSvc *services.FareService) gin.HandlerFunc {
	offerVisibleModifier := fmt.Sprintf("-%d seconds", services.DispatchOfferVisibleSeconds(cfg))
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		reqCtx := c.Request.Context()
		driverID := u.UserID

		var lastLat, lastLng sql.NullFloat64
		var appLat, appLng sql.NullFloat64
		var appLast sql.NullString
		var appActive sql.NullInt64
		locCtx, locCancel := context.WithTimeout(reqCtx, availableRequestsQueryTimeout)
		err := db.QueryRowContext(locCtx, `
			SELECT last_lat, last_lng, app_lat, app_lng, app_last_seen_at, COALESCE(app_location_active, 0)
			FROM drivers WHERE user_id = ?1`, driverID).Scan(&lastLat, &lastLng, &appLat, &appLng, &appLast, &appActive)
		locCancel()
		if err != nil && isMissingColumnErrHandlers(err) {
			// Backward compatible: DB not migrated yet; fall back to Telegram-only fields.
			fbCtx, fbCancel := context.WithTimeout(reqCtx, availableRequestsQueryTimeout)
			_ = db.QueryRowContext(fbCtx, `SELECT last_lat, last_lng FROM drivers WHERE user_id = ?1`, driverID).Scan(&lastLat, &lastLng)
			fbCancel()
			appLat, appLng = sql.NullFloat64{}, sql.NullFloat64{}
			appLast = sql.NullString{}
			appActive = sql.NullInt64{Int64: 0, Valid: true}
			err = nil
		}
		if err != nil {
			if writeAvailableRequestsErr(c, driverID, "driver_location", err) {
				return
			}
		}
		loc := services.EffectiveDriverLocation{
			AppLat:            appLat,
			AppLng:            appLng,
			AppLastSeenAt:     appLast,
			AppLocationActive: appActive,
			LastLat:           lastLat,
			LastLng:           lastLng,
		}
		eLat, eLng := services.GetEffectiveDriverLocation(loc)
		log.Println("Driver location source:", services.GetEffectiveDriverLocationSource(loc))

		// Optional long-polling: if wait_sec is provided, block up to that many seconds
		// and return immediately when at least one offer becomes available.
		waitSec := 0
		if s := strings.TrimSpace(c.Query("wait_sec")); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				waitSec = n
			}
		}
		if waitSec > 25 {
			waitSec = 25
		}

		queryOffers := func() ([]DriverAvailableOffer, error) {
			qCtx, cancel := context.WithTimeout(reqCtx, availableRequestsQueryTimeout)
			defer cancel()
			qNew := `
				SELECT r.id, COALESCE(t.id, ''), r.pickup_lat, r.pickup_lng, r.radius_km, COALESCE(r.estimated_price, 0), COALESCE(r.expires_at,'')
				FROM request_notifications n
				JOIN ride_requests r ON r.id = n.request_id
				LEFT JOIN trips t ON t.request_id = r.id AND t.driver_user_id = n.driver_user_id
				  AND t.status IN ('WAITING','ARRIVED','STARTED')
				WHERE n.driver_user_id = ?1 AND n.status = ?2
				  AND r.status = ?3 AND r.expires_at > datetime('now')
				  AND n.created_at > datetime('now', ?4)`
			qLegacy := `
				SELECT r.id, r.pickup_lat, r.pickup_lng, r.radius_km, COALESCE(r.expires_at,'')
				FROM request_notifications n
				JOIN ride_requests r ON r.id = n.request_id
				WHERE n.driver_user_id = ?1 AND n.status = ?2
				  AND r.status = ?3 AND r.expires_at > datetime('now')
				  AND n.created_at > datetime('now', ?4)`
			rows, err := db.QueryContext(qCtx, qNew, driverID, domain.NotificationStatusSent, domain.RequestStatusPending, offerVisibleModifier)
			newColsOK := true
			if err != nil && isMissingColumnErrHandlers(err) {
				newColsOK = false
				rows, err = db.QueryContext(qCtx, qLegacy, driverID, domain.NotificationStatusSent, domain.RequestStatusPending, offerVisibleModifier)
			}
			if err != nil {
				return nil, err
			}
			defer rows.Close()

			var offers []DriverAvailableOffer
			for rows.Next() {
				var o DriverAvailableOffer
				if newColsOK {
					if err := rows.Scan(&o.RequestID, &o.TripID, &o.PickupLat, &o.PickupLng, &o.RadiusKm, &o.EstimatedPrice, &o.ExpiresAt); err != nil {
						continue
					}
				} else {
					if err := rows.Scan(&o.RequestID, &o.PickupLat, &o.PickupLng, &o.RadiusKm, &o.ExpiresAt); err != nil {
						continue
					}
					o.EstimatedPrice = 0
				}
				o.ExpiresAt = formatExpiresAtRFC3339(o.ExpiresAt)
				if eLat != 0 || eLng != 0 {
					o.DistanceKm = utils.HaversineMeters(eLat, eLng, o.PickupLat, o.PickupLng) / 1000
				}
				offers = append(offers, o)
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			return offers, nil
		}

		offers, err := queryOffers()
		if err != nil {
			if writeAvailableRequestsErr(c, driverID, "offers", err) {
				return
			}
			offers = nil
		}
		if waitSec > 0 && len(offers) == 0 {
			deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
			hub := ws.DispatchHubDefault
			for time.Now().Before(deadline) && len(offers) == 0 {
				if err := reqCtx.Err(); err != nil {
					break
				}
				// The 1-second cap existed because the dispatch wake reached only one
				// waiter, so every other long poll had to re-query on a timer. The wake
				// is now a broadcast, so wait for the real signal and cap only loosely
				// as a safety net. This is what makes wait_sec=25 a genuine long poll
				// instead of disguised 1-second polling.
				remaining := time.Until(deadline)
				waitSlice := remaining
				if waitSlice > 5*time.Second {
					waitSlice = 5 * time.Second
				}
				if hub != nil {
					hub.WaitForDispatchChange(reqCtx, waitSlice)
				} else {
					select {
					case <-reqCtx.Done():
					case <-time.After(200 * time.Millisecond):
					}
				}
				offers, err = queryOffers()
				if err != nil {
					if writeAvailableRequestsErr(c, driverID, "offers_wait", err) {
						return
					}
					offers = nil
					break
				}
			}
		}

		// Deterministic pick. This LIMIT 1 previously had no ORDER BY, so when a
		// driver held more than one active trip SQLite was free to return either —
		// and the reported assigned_trip.status flipped between them from poll to
		// poll. That is the "button changes then reverts" symptom; it was never
		// replica lag (there is one Turso primary and no read cache), it was a
		// nondeterministic row choice over rows that should not have coexisted.
		//
		// TryAssign now refuses to give a driver a second active trip, so new
		// duplicates cannot appear; the ORDER BY makes legacy rows stable too, and
		// picking the furthest-progressed trip means a just-tapped ARRIVED never
		// reads back as WAITING.
		var assigned *DriverAssignedTripStub
		var tripID, status string
		assignCtx, assignCancel := context.WithTimeout(reqCtx, availableRequestsQueryTimeout)
		err = db.QueryRowContext(assignCtx, `
			SELECT t.id, t.status FROM trips t
			JOIN ride_requests r ON r.id = t.request_id
			WHERE t.driver_user_id = ?1 AND t.status IN ('WAITING','ARRIVED','STARTED')
			  AND r.status = ?2
			ORDER BY CASE t.status
			           WHEN 'STARTED' THEN 0
			           WHEN 'ARRIVED' THEN 1
			           ELSE 2
			         END,
			         t.id
			LIMIT 1`,
			driverID, domain.RequestStatusAssigned).Scan(&tripID, &status)
		assignCancel()
		if err == nil && tripID != "" {
			assigned = &DriverAssignedTripStub{TripID: tripID, Status: status}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) && writeAvailableRequestsErr(c, driverID, "assigned_trip", err) {
			return
		}

		// A driver should never have more than one active trip. If legacy rows put
		// them in that state, say so rather than silently picking one.
		var activeCount int
		countCtx, countCancel := context.WithTimeout(reqCtx, availableRequestsQueryTimeout)
		if err := db.QueryRowContext(countCtx, `
			SELECT COUNT(*) FROM trips
			WHERE driver_user_id = ?1 AND status IN ('WAITING','ARRIVED','STARTED')`,
			driverID).Scan(&activeCount); err == nil && activeCount > 1 {
			log.Printf("driver_dispatch: driver %d has %d active trips; serving %s (%s)", driverID, activeCount, tripID, status)
		}
		countCancel()

		// Stable balance shape. Clients were scrapping a long list of aliases to
		// find these; total/promo/cash in so'm is the contract from here on.
		bal := loadDriverBalance(reqCtx, db, driverID)
		tariff := driverTariffFromSettings(reqCtx, cfg, fareSvc)

		resp := gin.H{
			"assigned_trip":      assigned,
			"available_requests": offers,
			"requests":           offers,
			"pending_requests":   offers,
			"queue":              offers,
			"orders":             offers,
			"jobs":               offers,
			"total_balance":      bal.Total,
			"promo_balance":      bal.Promo,
			"cash_balance":       bal.Cash,
			"balance":            bal.Total, // legacy alias
			// The rate the driver is working under, so a wallet screen can show it
			// without a second call. Full tariff is at GET /driver/tariff.
			"commission_percent": tariff.CommissionPercent,
			"commission_charged": tariff.CommissionCharged,
		}

		// This endpoint is polled every few seconds per online driver and is mostly
		// unchanged between polls, so support conditional requests: an idle poll
		// costs a hash comparison and a 304 instead of a full body.
		etag, unchanged := writeETag(c, resp)
		if unchanged {
			// AbortWithStatus, not Status: gin defers WriteHeader, so c.Status alone
			// would leave the recorder/response at 200 with an empty body.
			c.AbortWithStatus(http.StatusNotModified)
			return
		}
		if etag != "" {
			c.Header("ETag", etag)
		}
		c.JSON(http.StatusOK, resp)
	}
}

func formatExpiresAtRFC3339(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339, raw); err2 == nil {
			return t2.UTC().Format(time.RFC3339)
		}
		return raw
	}
	return t.UTC().Format(time.RFC3339)
}

// writeAvailableRequestsErr logs the real Turso/SQL error. Returns true when the
// handler should stop (client gone or hard failure). Client disconnects are not
// reported as HTTP 500 — that was the ~14s "query failed" noise under poll races.
func writeAvailableRequestsErr(c *gin.Context, driverID int64, step string, err error) bool {
	if err == nil {
		return false
	}
	// Client hung up mid-poll (or replaced the long-poll) — do not surface as 500.
	if errors.Is(err, context.Canceled) || errors.Is(c.Request.Context().Err(), context.Canceled) {
		log.Printf("driver_available_requests: driver=%d step=%s canceled: %v", driverID, step, err)
		c.Abort()
		return true
	}
	log.Printf("driver_available_requests: driver=%d step=%s err=%v", driverID, step, err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
	return true
}

// DriverAcceptRequest delegates to AssignmentService.TryAssign (same as driver bot accept). Schedules start reminder on success.
func DriverAcceptRequest(db *sql.DB, assignSvc *services.AssignmentService, tripSvc *services.TripService, cfg *config.Config, fareSvc *services.FareService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		var req DriverAcceptRequestBody
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		req.RequestID = strings.TrimSpace(req.RequestID)
		req.TripID = strings.TrimSpace(req.TripID)
		if req.RequestID == "" && req.TripID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "trip_id or request_id required"})
			return
		}
		ctx := c.Request.Context()
		driverID := u.UserID

		if req.RequestID == "" && req.TripID != "" {
			var driverUserID int64
			var st string
			err := db.QueryRowContext(ctx, `SELECT driver_user_id, status FROM trips WHERE id = ?1`, req.TripID).Scan(&driverUserID, &st)
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"ok": false, "code": CodeNotFound, "message": "trip not found", "error": "trip not found", "trip_id": req.TripID})
				return
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
				return
			}
			if driverUserID != driverID {
				c.JSON(http.StatusForbidden, gin.H{"ok": false, "code": CodeNotAssignedToTrip, "message": "not assigned to this trip", "error": "not assigned to this trip"})
				return
			}
			body := gin.H{"ok": true, "trip_id": req.TripID, "status": st, "result": "already_assigned", "assigned": true}
			if snap := loadDriverTripSnapshot(ctx, db, cfg, fareSvc, req.TripID); snap != nil {
				body["trip"] = snap
			}
			c.JSON(http.StatusOK, body)
			return
		}

		if assignSvc == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assignment unavailable"})
			return
		}
		assigned, tripID, err := assignSvc.TryAssign(ctx, req.RequestID, driverID)
		if err != nil {
			log.Printf("driver_accept_request failed request_id=%s driver_id=%d err=%v", req.RequestID, driverID, err)
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "code": CodeInternalError, "message": "assignment failed", "error": "assignment_failed", "request_id": req.RequestID})
			return
		}
		if !assigned {
			// TryAssign refuses for two very different reasons: another driver took
			// the request, or this driver already has an active trip. In the second
			// case the offer stays visible (it rolls back to PENDING/SENT), so
			// without a distinct code the driver taps accept, sees "no longer
			// available", watches the offer reappear, and loops with no idea the
			// cause is their own unfinished trip.
			if activeTripID, ok := driverActiveTripID(ctx, db, driverID); ok {
				c.JSON(http.StatusConflict, gin.H{
					"ok":             false,
					"code":           CodeDriverHasActiveTrip,
					"error":          "driver_has_active_trip",
					"message":        "Finish or cancel your current trip before accepting a new one.",
					"active_trip_id": activeTripID,
					"request_id":     req.RequestID,
				})
				return
			}
			c.JSON(http.StatusConflict, gin.H{"ok": false, "code": CodeRequestUnavailable, "message": "request no longer available", "error": "request no longer available", "request_id": req.RequestID})
			return
		}
		if tripSvc != nil {
			tripSvc.ScheduleStartReminder(ctx, tripID, driverID)
		}
		// Return the trip inline (coordinates, fare, rider phone) so the client can
		// render the pickup screen from this one response instead of chaining a
		// GET /trip/:id straight after every accept.
		body := gin.H{"ok": true, "trip_id": tripID, "request_id": req.RequestID, "assigned": true}
		if snap := loadDriverTripSnapshot(ctx, db, cfg, fareSvc, tripID); snap != nil {
			body["trip"] = snap
		}
		c.JSON(http.StatusOK, body)
	}
}

// driverActiveTripID returns the driver's current active trip, if any.
// Used only to explain why an accept was refused.
func driverActiveTripID(ctx context.Context, db *sql.DB, driverID int64) (string, bool) {
	var tripID string
	err := db.QueryRowContext(ctx, `
		SELECT id FROM trips
		WHERE driver_user_id = ?1 AND status IN (?2, ?3, ?4)
		ORDER BY id LIMIT 1`,
		driverID, domain.TripStatusWaiting, domain.TripStatusArrived, domain.TripStatusStarted).Scan(&tripID)
	if err != nil {
		return "", false
	}
	return tripID, tripID != ""
}

// DriverBalance is the stable wallet shape returned with dispatch responses.
// All values are whole so'm.
type DriverBalance struct {
	Total int64 `json:"total_balance"`
	Promo int64 `json:"promo_balance"`
	Cash  int64 `json:"cash_balance"`
}

// loadDriverBalance reads the driver's wallet, tolerating databases where the
// promo/cash split (migration 035) has not been applied.
func loadDriverBalance(ctx context.Context, db *sql.DB, driverID int64) DriverBalance {
	var b DriverBalance
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(balance, 0), COALESCE(promo_balance, 0), COALESCE(cash_balance, 0)
		FROM drivers WHERE user_id = ?1`, driverID).Scan(&b.Total, &b.Promo, &b.Cash)
	if err != nil && isMissingColumnErrHandlers(err) {
		_ = db.QueryRowContext(ctx, `SELECT COALESCE(balance, 0) FROM drivers WHERE user_id = ?1`, driverID).Scan(&b.Total)
	}
	return b
}

func isMissingColumnErrHandlers(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column")
}

// writeETag hashes the response and compares it with If-None-Match.
// Returns the computed ETag and whether the client's copy is still current.
func writeETag(c *gin.Context, body any) (etag string, notModified bool) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	etag = `W/"` + hex.EncodeToString(sum[:16]) + `"`
	if match := strings.TrimSpace(c.GetHeader("If-None-Match")); match != "" {
		for _, candidate := range strings.Split(match, ",") {
			if strings.TrimSpace(candidate) == etag {
				c.Header("ETag", etag)
				return etag, true
			}
		}
	}
	return etag, false
}
