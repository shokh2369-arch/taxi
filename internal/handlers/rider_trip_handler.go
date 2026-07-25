package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
)

// RiderTripDeps wires native rider trip HTTP handlers (Bearer auth).
type RiderTripDeps struct {
	DB           *sql.DB
	RiderAuthSvc *services.RiderAuthService
	TripSvc      *services.TripService
}

// RegisterRiderTripRoutes mounts Bearer-authenticated trip routes under /v1/rider/trips/*.
//
// This is intended for the native Flutter rider app, which does not have Telegram init_data.
func RegisterRiderTripRoutes(r *gin.Engine, deps RiderTripDeps) {
	if r == nil || deps.DB == nil || deps.RiderAuthSvc == nil || deps.TripSvc == nil {
		return
	}
	bearer := RequireRiderBearerAuth(deps.RiderAuthSvc, deps.DB)
	g := r.Group("/v1/rider")
	g.Use(bearer)

	// Discover current active trip (after driver accepts/dispatch assigns a trip).
	g.GET("/trips/active", riderAppGetActiveTrip(deps))

	// Trip history: terminal trips for the authenticated rider (pagination).
	// Query: limit (default 20, max 50), cursor (opaque: rowid of last item from previous page).
	g.GET("/trips", riderAppListTrips(deps))

	// Preferred: no body, trip id in path.
	g.POST("/trips/:id/cancel", riderAppCancelTripByPath(deps))
	// Convenience: body { "trip_id": "..." }.
	g.POST("/trips/cancel", riderAppCancelTripByBody(deps))
}

type riderTripCancelBody struct {
	TripID string `json:"trip_id"`
}

func riderAppListTrips(deps RiderTripDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		ctx := c.Request.Context()

		limit := 20
		if s := strings.TrimSpace(c.Query("limit")); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 50 {
			limit = 50
		}

		var cursorRowid int64
		if s := strings.TrimSpace(c.Query("cursor")); s != "" {
			if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
				cursorRowid = v
			}
		}

		// Fetch limit+1 to detect next page (SQLite rowid pagination).
		fetch := limit + 1
		var rows *sql.Rows
		var err error
		if cursorRowid > 0 {
			rows, err = deps.DB.QueryContext(ctx, `
				SELECT t.rowid, t.id, t.status, t.finished_at, t.cancelled_at, t.fare_amount, t.distance_m, t.request_id,
				       r.pickup_lat, r.pickup_lng, r.drop_lat, r.drop_lng
				FROM trips t
				LEFT JOIN ride_requests r ON r.id = t.request_id
				WHERE t.rider_user_id = ?1
				  AND t.status IN ('FINISHED','CANCELLED','CANCELLED_BY_DRIVER','CANCELLED_BY_RIDER')
				  AND t.rowid < ?2
				ORDER BY t.rowid DESC
				LIMIT ?3`, uid, cursorRowid, fetch)
		} else {
			rows, err = deps.DB.QueryContext(ctx, `
				SELECT t.rowid, t.id, t.status, t.finished_at, t.cancelled_at, t.fare_amount, t.distance_m, t.request_id,
				       r.pickup_lat, r.pickup_lng, r.drop_lat, r.drop_lng
				FROM trips t
				LEFT JOIN ride_requests r ON r.id = t.request_id
				WHERE t.rider_user_id = ?1
				  AND t.status IN ('FINISHED','CANCELLED','CANCELLED_BY_DRIVER','CANCELLED_BY_RIDER')
				ORDER BY t.rowid DESC
				LIMIT ?2`, uid, fetch)
		}
		if err != nil {
			writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
			return
		}
		defer rows.Close()

		type pickupDrop struct {
			Lat float64 `json:"lat"`
			Lng float64 `json:"lng"`
		}
		var out []gin.H
		for rows.Next() {
			var rowid int64
			var id, status, requestID string
			var finishedAt, cancelledAt sql.NullString
			// fare_amount / distance_m are nullable in production; scanning into int64 500s the whole page.
			var fareAmount, distanceM sql.NullInt64
			var pickupLat, pickupLng, dropLat, dropLng sql.NullFloat64
			if err := rows.Scan(&rowid, &id, &status, &finishedAt, &cancelledAt, &fareAmount, &distanceM, &requestID,
				&pickupLat, &pickupLng, &dropLat, &dropLng); err != nil {
				writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
				return
			}
			finStr, canStr := "", ""
			if finishedAt.Valid {
				finStr = tripHistoryTimeToRFC3339(finishedAt.String)
			}
			if cancelledAt.Valid {
				canStr = tripHistoryTimeToRFC3339(cancelledAt.String)
			}
			item := gin.H{
				"id":           id,
				"trip_id":      id,
				"tripId":       id,
				"status":       status,
				"request_id":   requestID,
				"requestId":    requestID,
				"fare_amount":  fareAmount.Int64,
				"fareAmount":   fareAmount.Int64,
				"distance_m":   distanceM.Int64,
				"distanceM":    distanceM.Int64,
				"finished_at":  finStr,
				"finishedAt":   finStr,
				"cancelled_at": canStr,
				"cancelledAt":  canStr,
				"_rowid":       rowid, // internal; strip before return
			}
			if pickupLat.Valid && pickupLng.Valid {
				p := pickupDrop{Lat: pickupLat.Float64, Lng: pickupLng.Float64}
				item["pickup"] = p
			}
			if dropLat.Valid && dropLng.Valid {
				d := pickupDrop{Lat: dropLat.Float64, Lng: dropLng.Float64}
				item["drop"] = d
			}
			out = append(out, item)
		}
		if err := rows.Err(); err != nil {
			writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
			return
		}

		var nextCursor interface{}
		if len(out) > limit {
			// Remove the extra row used to detect pagination.
			last := out[limit-1]
			out = out[:limit]
			if rid, ok := last["_rowid"].(int64); ok {
				nextCursor = strconv.FormatInt(rid, 10)
			}
		}
		for i := range out {
			delete(out[i], "_rowid")
		}

		c.JSON(http.StatusOK, gin.H{
			"trips":       out,
			"next_cursor": nextCursor,
			"nextCursor":  nextCursor,
		})
	}
}

func riderAppGetActiveTrip(deps RiderTripDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		ctx := c.Request.Context()

		// "Active trip" matches the rest of the backend: WAITING / ARRIVED / STARTED.
		var tripID, status string
		err := deps.DB.QueryRowContext(ctx, `
			SELECT id, status
			FROM trips
			WHERE rider_user_id = ?1
			  AND status IN ('WAITING','ARRIVED','STARTED')
			ORDER BY rowid DESC
			LIMIT 1
		`, uid).Scan(&tripID, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
			return
		}
		if err == nil {
			c.JSON(http.StatusOK, gin.H{
				"trip": gin.H{
					"id":     tripID,
					"status": status,
				},
			})
			return
		}

		// After confirm, trip row may not exist until a driver accepts — surface dispatch state so clients do not see trip: null only.
		var pendingID string
		qStrict := `
			SELECT id FROM ride_requests
			WHERE rider_user_id = ?1 AND status = 'PENDING' AND expires_at > datetime('now')
			  AND drop_lat IS NOT NULL AND drop_lng IS NOT NULL AND COALESCE(estimated_price, 0) > 0
			  AND COALESCE(destination_confirmed, 0) = 1
			ORDER BY rowid DESC LIMIT 1`
		qLegacy := `
			SELECT id FROM ride_requests
			WHERE rider_user_id = ?1 AND status = 'PENDING' AND expires_at > datetime('now')
			  AND drop_lat IS NOT NULL AND drop_lng IS NOT NULL AND COALESCE(estimated_price, 0) > 0
			ORDER BY rowid DESC LIMIT 1`
		err2 := deps.DB.QueryRowContext(ctx, qStrict, uid).Scan(&pendingID)
		if err2 != nil && riderTripIsMissingColumnErr(err2) {
			err2 = deps.DB.QueryRowContext(ctx, qLegacy, uid).Scan(&pendingID)
		}
		if err2 == nil && strings.TrimSpace(pendingID) != "" {
			c.JSON(http.StatusOK, gin.H{
				"trip": nil,
				"pending_dispatch": gin.H{
					"request_id": pendingID,
					"status":     "PENDING_DISPATCH",
				},
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"trip": nil})
	}
}

func riderTripIsMissingColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column")
}

func riderAppCancelTripByPath(deps RiderTripDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		tripID := strings.TrimSpace(c.Param("id"))
		if tripID == "" {
			writeRiderAPIError(c, http.StatusBadRequest, "invalid_body", "Yuborilgan ma‘lumot noto‘g‘ri.")
			return
		}
		riderAppCancelTrip(c, deps, uid, tripID)
	}
}

func riderAppCancelTripByBody(deps RiderTripDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		var body riderTripCancelBody
		if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.TripID) == "" {
			writeRiderAPIError(c, http.StatusBadRequest, "invalid_body", "Yuborilgan ma‘lumot noto‘g‘ri.")
			return
		}
		riderAppCancelTrip(c, deps, uid, strings.TrimSpace(body.TripID))
	}
}

func riderAppCancelTrip(c *gin.Context, deps RiderTripDeps, riderUserID int64, tripID string) {
	ctx := c.Request.Context()

	// Ensure the trip exists and belongs to the authenticated rider.
	ok, err := auth.AuthorizeTripAccess(ctx, deps.DB, riderUserID, tripID, domain.RoleRider)
	if err != nil {
		writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
		return
	}
	if !ok {
		// Avoid leaking existence; treat as not_found (same style as rider requests).
		writeRiderAPIError(c, http.StatusNotFound, "not_found", "Safar topilmadi.")
		return
	}

	result, err := deps.TripSvc.CancelByRider(ctx, tripID, riderUserID)
	if err != nil {
		mapRiderTripCancelError(c, tripID, err)
		return
	}

	// Keep response stable and close to the non-v1 /trip/cancel/rider result,
	// but wrapped in v1 rider JSON conventions.
	if result == nil || result.Result == "noop" {
		c.JSON(http.StatusOK, gin.H{"ok": true, "result": "noop"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"trip_id": tripID,
		"status":  result.Status,
		"result":  result.Result,
	})
}

func mapRiderTripCancelError(c *gin.Context, tripID string, err error) {
	switch {
	case errors.Is(err, domain.ErrTripNotFound):
		writeRiderAPIError(c, http.StatusNotFound, "not_found", "Safar topilmadi.")
	case errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrAlreadyFinished),
		errors.Is(err, domain.ErrAlreadyCancelled):
		writeRiderAPIError(c, http.StatusConflict, "conflict", "Safarni bekor qilib bo‘lmaydi.")
	default:
		writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
	}
}
