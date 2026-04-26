package handlers

import (
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
	"taxi-mvp/internal/utils"
)

// OpenAPI (informal) — Driver dispatch HTTP
//
// GET /driver/available-requests:
//   200: { assigned_trip: null | { trip_id, status }, available_requests, requests, pending_requests, queue, orders, jobs: Offer[] }
//   Offer: { request_id, trip_id?, pickup_lat, pickup_lng, distance_km, radius_km, expires_at? }
//
// POST /driver/accept-request:
//   body: { trip_id?, request_id? }
//   200: { ok, trip_id, request_id?, assigned?, result?, status? } | idempotent already_assigned
//   400: { ok: false, error, request_id? } | { error: invalid body | ... }
//   403/404: trip_id-only branch
//   409: { ok: false, error: "request no longer available", request_id }
//   503: assignment unavailable
//
// DriverAcceptRequestBody is accepted for POST /driver/accept-request. At least one of trip_id or request_id should be set.
type DriverAcceptRequestBody struct {
	TripID    string `json:"trip_id"`
	RequestID string `json:"request_id"`
}

// DriverAvailableOffer is one pending offer for the driver (same underlying rows as Telegram dispatch).
type DriverAvailableOffer struct {
	RequestID  string  `json:"request_id"`
	TripID     string  `json:"trip_id,omitempty"`
	PickupLat  float64 `json:"pickup_lat"`
	PickupLng  float64 `json:"pickup_lng"`
	DistanceKm float64 `json:"distance_km"`
	RadiusKm   float64 `json:"radius_km"`
	EstimatedPrice int64 `json:"estimated_price"`
	ExpiresAt  string  `json:"expires_at,omitempty"`
}

// DriverAssignedTripStub is optional context for an in-progress assignment (Flutter may call GET /trip/:id for full detail).
type DriverAssignedTripStub struct {
	TripID string `json:"trip_id"`
	Status string `json:"status"`
}

// DriverAvailableRequests returns pending offers (request_notifications SENT + PENDING request) and optional active trip stub.
func DriverAvailableRequests(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		ctx := c.Request.Context()
		driverID := u.UserID

		var lastLat, lastLng sql.NullFloat64
		var appLat, appLng sql.NullFloat64
		var appLast sql.NullString
		var appActive sql.NullInt64
		err := db.QueryRowContext(ctx, `
			SELECT last_lat, last_lng, app_lat, app_lng, app_last_seen_at, COALESCE(app_location_active, 0)
			FROM drivers WHERE user_id = ?1`, driverID).Scan(&lastLat, &lastLng, &appLat, &appLng, &appLast, &appActive)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such column") {
			// Backward compatible: DB not migrated yet; fall back to Telegram-only fields.
			_ = db.QueryRowContext(ctx, `SELECT last_lat, last_lng FROM drivers WHERE user_id = ?1`, driverID).Scan(&lastLat, &lastLng)
			appLat, appLng = sql.NullFloat64{}, sql.NullFloat64{}
			appLast = sql.NullString{}
			appActive = sql.NullInt64{Int64: 0, Valid: true}
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
			qNew := `
				SELECT r.id, r.pickup_lat, r.pickup_lng, r.radius_km, COALESCE(r.estimated_price, 0), COALESCE(r.expires_at,'')
				FROM request_notifications n
				JOIN ride_requests r ON r.id = n.request_id
				WHERE n.driver_user_id = ?1 AND n.status = ?2
				  AND r.status = ?3 AND r.expires_at > datetime('now')`
			qLegacy := `
				SELECT r.id, r.pickup_lat, r.pickup_lng, r.radius_km, COALESCE(r.expires_at,'')
				FROM request_notifications n
				JOIN ride_requests r ON r.id = n.request_id
				WHERE n.driver_user_id = ?1 AND n.status = ?2
				  AND r.status = ?3 AND r.expires_at > datetime('now')`
			rows, err := db.QueryContext(ctx, qNew, driverID, domain.NotificationStatusSent, domain.RequestStatusPending)
			newColsOK := true
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such column") {
				newColsOK = false
				rows, err = db.QueryContext(ctx, qLegacy, driverID, domain.NotificationStatusSent, domain.RequestStatusPending)
			}
			if err != nil {
				return nil, err
			}
			defer rows.Close()

			var offers []DriverAvailableOffer
			for rows.Next() {
				var o DriverAvailableOffer
				if newColsOK {
					if err := rows.Scan(&o.RequestID, &o.PickupLat, &o.PickupLng, &o.RadiusKm, &o.EstimatedPrice, &o.ExpiresAt); err != nil {
						continue
					}
				} else {
					if err := rows.Scan(&o.RequestID, &o.PickupLat, &o.PickupLng, &o.RadiusKm, &o.ExpiresAt); err != nil {
						continue
					}
					o.EstimatedPrice = 0
				}
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		if waitSec > 0 && len(offers) == 0 {
			deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					break
				default:
				}
				time.Sleep(500 * time.Millisecond)
				offers, err = queryOffers()
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
					return
				}
				if len(offers) > 0 {
					break
				}
			}
		}

		var assigned *DriverAssignedTripStub
		var tripID, status string
		err = db.QueryRowContext(ctx, `
			SELECT id, status FROM trips
			WHERE driver_user_id = ?1 AND status IN ('WAITING','ARRIVED','STARTED') LIMIT 1`,
			driverID).Scan(&tripID, &status)
		if err == nil && tripID != "" {
			assigned = &DriverAssignedTripStub{TripID: tripID, Status: status}
		}

		resp := gin.H{
			"assigned_trip":      assigned,
			"available_requests": offers,
			"requests":           offers,
			"pending_requests":   offers,
			"queue":              offers,
			"orders":             offers,
			"jobs":               offers,
		}
		c.JSON(http.StatusOK, resp)
	}
}

// DriverAcceptRequest delegates to AssignmentService.TryAssign (same as driver bot accept). Schedules start reminder on success.
func DriverAcceptRequest(db *sql.DB, assignSvc *services.AssignmentService, tripSvc *services.TripService) gin.HandlerFunc {
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
				c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "trip not found", "trip_id": req.TripID})
				return
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
				return
			}
			if driverUserID != driverID {
				c.JSON(http.StatusForbidden, gin.H{"ok": false, "error": "not assigned to this trip"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"ok": true, "trip_id": req.TripID, "status": st, "result": "already_assigned"})
			return
		}

		if assignSvc == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assignment unavailable"})
			return
		}
		assigned, tripID, err := assignSvc.TryAssign(ctx, req.RequestID, driverID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error(), "request_id": req.RequestID})
			return
		}
		if !assigned {
			c.JSON(http.StatusConflict, gin.H{"ok": false, "error": "request no longer available", "request_id": req.RequestID})
			return
		}
		if tripSvc != nil {
			tripSvc.ScheduleStartReminder(ctx, tripID, driverID)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "trip_id": tripID, "request_id": req.RequestID, "assigned": true})
	}
}
