package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
	"taxi-mvp/internal/utils"
)

// TripFareForResponse returns fare (display) and fareAmount (nil until FINISHED). For FINISHED trips uses stored fare_amount; otherwise uses computedFare (tiered or legacy).
func TripFareForResponse(status string, fareAmount sql.NullInt64, computedFare int64) (fare int64, fareAmountPtr *int64) {
	if fareAmount.Valid && status == "FINISHED" {
		v := fareAmount.Int64
		return v, &v
	}
	return computedFare, nil
}

// Stable machine codes for driver-facing 4xx responses.
//
// Clients were string-matching localized Uzbek and Russian sentences to tell
// these apart, which breaks the moment any copy is reworded. `code` is the
// contract; `message` is display text and may change freely.
const (
	CodePickupTooFar            = "PICKUP_TOO_FAR"
	CodeLiveLocationInactive    = "LIVE_LOCATION_INACTIVE"
	CodeDriverLocationStale     = "DRIVER_LOCATION_STALE"
	CodeRequestUnavailable      = "REQUEST_UNAVAILABLE"
	CodeDriverHasActiveTrip     = "DRIVER_HAS_ACTIVE_TRIP"
	CodeNotFound                = "NOT_FOUND"
	CodeInvalidTransition       = "INVALID_TRANSITION"
	CodeLegalAcceptanceRequired = "LEGAL_ACCEPTANCE_REQUIRED"
	CodeNotAssignedToTrip       = "NOT_ASSIGNED_TO_TRIP"
	CodeInvalidBody             = "INVALID_BODY"
	CodeInternalError           = "INTERNAL_ERROR"
	CodeSessionRevoked          = "AUTH_SESSION_REVOKED"
)

// writeTripError maps domain errors to HTTP status, a stable code, and a
// localized message. The legacy `error` field is preserved for older clients.
func writeTripError(c *gin.Context, tripID string, err error) {
	write := func(status int, code, message string) {
		c.JSON(status, gin.H{
			"ok":      false,
			"code":    code,
			"message": message,
			"error":   message, // legacy field; clients should read `code`
			"trip_id": tripID,
		})
	}
	switch {
	case errors.Is(err, domain.ErrTripNotFound):
		write(http.StatusNotFound, CodeNotFound, "trip not found")
	case errors.Is(err, domain.ErrInvalidTransition):
		write(http.StatusConflict, CodeInvalidTransition, "invalid transition")
	case errors.Is(err, domain.ErrAlreadyFinished), errors.Is(err, domain.ErrAlreadyCancelled):
		write(http.StatusConflict, CodeInvalidTransition, err.Error())
	case errors.Is(err, domain.ErrTooFarFromPickup):
		write(http.StatusBadRequest, CodePickupTooFar, "Мижозга ҳали етиб бормагансиз. Аввал олиб кетиш нуқтасига етинг.")
	case errors.Is(err, domain.ErrLiveLocationInactive):
		write(http.StatusBadRequest, CodeLiveLocationInactive, "Жонли локация ёқилмаган. Telegramда жонли локацияни уланг.")
	case errors.Is(err, domain.ErrDriverLocationStale):
		write(http.StatusBadRequest, CodeDriverLocationStale, "Локациянгиз эскирган. Жонли локация уланганини текширинг.")
	default:
		write(http.StatusInternalServerError, CodeInternalError, "operation failed")
	}
}

// writeTripResult writes the success response for a trip action.
//
// The body carries the resulting trip inline (status, coordinates, fare,
// rider phone) so a driver client never needs a follow-up GET /trip/:id to render
// the next screen. That second sequential round trip was what made the
// arrived/start/finish buttons feel slow.
//
// snap may be nil (the trip could not be re-read); the minimal shape is then
// returned, since the state change itself already committed. Legacy fields
// trip_id / status / result are unchanged for older clients.
func writeTripResult(c *gin.Context, tripID string, result *services.TripActionResult, snap *DriverTripSnapshot) {
	if result == nil || result.Result == "noop" {
		body := gin.H{"ok": true, "result": "noop"}
		if result != nil && result.Status != "" {
			body["trip_id"] = tripID
			body["status"] = result.Status
		}
		if snap != nil {
			body["trip"] = snap
			mergeTripSnapshotTopLevel(body, snap)
		}
		c.JSON(http.StatusOK, body)
		return
	}
	body := gin.H{"ok": true, "trip_id": tripID, "status": result.Status, "result": result.Result}
	if snap != nil {
		body["trip"] = snap
		mergeTripSnapshotTopLevel(body, snap)
	}
	c.JSON(http.StatusOK, body)
}

// mergeTripSnapshotTopLevel also exposes the snapshot fields at the top level,
// so a client can read either `trip.fare_som` or `fare_som` without branching.
func mergeTripSnapshotTopLevel(body gin.H, snap *DriverTripSnapshot) {
	body["status"] = snap.Status
	body["pickup_lat"] = snap.PickupLat
	body["pickup_lng"] = snap.PickupLng
	body["dropoff_lat"] = snap.DropoffLat
	body["dropoff_lng"] = snap.DropoffLng
	body["fare_som"] = snap.FareSom
	body["distance_km"] = snap.DistanceKm
	body["is_final_fare"] = snap.IsFinalFare
	if snap.RiderPhone != "" {
		body["rider_phone"] = snap.RiderPhone
	}
	if snap.RiderName != "" {
		body["rider_name"] = snap.RiderName
	}
}

// TripStartRequest body for POST /trip/start. driver_id comes from auth context.
type TripStartRequest struct {
	TripID string `json:"trip_id" binding:"required"`
}

// TripFinishRequest body for POST /trip/finish. driver_id comes from auth context.
type TripFinishRequest struct {
	TripID string `json:"trip_id" binding:"required"`
}

// TripCancelDriverRequest body for POST /trip/cancel/driver. driver_id comes from auth context.
type TripCancelDriverRequest struct {
	TripID string `json:"trip_id" binding:"required"`
}

// TripCancelRiderRequest body for POST /trip/cancel/rider. rider_id comes from auth context.
type TripCancelRiderRequest struct {
	TripID string `json:"trip_id" binding:"required"`
}

// LatLng is a point for rider/driver Mini App (pickup, drop, driver position).
type LatLng struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// DriverCar is the canonical nested car object for the rider app.
// Keep fields minimal and stable; clients accept aliases but this is the preferred shape.
type DriverCar struct {
	Make  string `json:"make,omitempty"`
	Model string `json:"model,omitempty"`
	Color string `json:"color,omitempty"`
	Plate string `json:"plate,omitempty"`
}

// DriverLocationObject is the canonical nested location object for the rider app.
// Name avoids collision with handlers.DriverLocation HTTP handler.
type DriverLocationObject struct {
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Heading *int    `json:"heading,omitempty"`
}

// DriverObject is the canonical driver object for rider app /trip polling.
// It intentionally includes lat/lng at top-level as an alias to keep older clients working.
type DriverObject struct {
	ID       string                `json:"id"`
	Name     string                `json:"name,omitempty"`
	Phone    string                `json:"phone,omitempty"`
	Rating   *float64              `json:"rating,omitempty"`
	PhotoURL string                `json:"photo_url,omitempty"`
	Car      *DriverCar            `json:"car,omitempty"`
	Location *DriverLocationObject `json:"location,omitempty"`
	Lat      *float64              `json:"lat,omitempty"`
	Lng      *float64              `json:"lng,omitempty"`
	Heading  *int                  `json:"heading,omitempty"`
}

// TripSummary is the standardized trip object for resync (nested in GET /trip/:id; rider and driver Mini App).
type TripSummary struct {
	ID         string  `json:"id"`     // trip id (string; e.g. UUID)
	Status     string  `json:"status"` // WAITING | ARRIVED | STARTED | FINISHED | CANCELLED_*
	DistanceM  int64   `json:"distance_m,omitempty"`
	DistanceKm float64 `json:"distance_km"`
	Fare       int64   `json:"fare"`                  // current estimate or final stored amount
	FareAmount *int64  `json:"fare_amount,omitempty"` // null until FINISHED
}

// TripInfoResponse is returned by GET /trip/:id for Mini App (rider: track driver; driver: run trip).
// Rider-friendly: trip, pickup, drop, driver as objects; driver_info for display.
type TripInfoResponse struct {
	// New canonical fields (native rider app).
	ID     string        `json:"id"`
	Driver *DriverObject `json:"driver,omitempty"`

	// Legacy fields kept for compatibility (Telegram mini app + older clients).
	TripID   string `json:"trip_id"`
	DriverID int64  `json:"driver_id,omitempty"`
	Status   string `json:"status"`
	Pickup   LatLng `json:"pickup"` // { lat, lng } for rider/driver map
	Drop     LatLng `json:"drop"`   // { lat, lng }
	// DriverLegacy previously used json tag "driver", which collided with the canonical Driver object
	// above and made encoding/json drop BOTH fields. Legacy clients get lat/lng via driver_pos and via
	// top-level lat/lng aliases inside the "driver" object.
	DriverLegacy LatLng       `json:"driver_latlng"` // legacy alias for driver position (older clients)
	DriverPos    LatLng       `json:"driver_pos"`    // { lat, lng } from drivers.last_lat/lng (alias when driver is object)
	DistanceKm   float64      `json:"distance_km"`
	Fare         int64        `json:"fare"`
	Trip         *TripSummary `json:"trip,omitempty"`
	DriverInfo   *struct {
		Phone   string `json:"phone,omitempty"`
		CarType string `json:"car_type,omitempty"`
		Color   string `json:"color,omitempty"`
		Plate   string `json:"plate,omitempty"`
	} `json:"driver_info,omitempty"` // who is coming to pick up the rider
	// Rider (client) info for driver mini app: show who to pick up and call
	RiderPhone string `json:"rider_phone,omitempty"`
	RiderName  string `json:"rider_name,omitempty"`
	RiderInfo  *struct {
		Phone string `json:"phone,omitempty"`
		Name  string `json:"name,omitempty"`
	} `json:"rider_info,omitempty"`
}

// TripStart calls TripService.StartTrip. Requires driver auth; driver may only start their assigned trip.
func TripStart(db *sql.DB, tripSvc *services.TripService, cfg *config.Config, fareSvc *services.FareService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		var req TripStartRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		ctx := c.Request.Context()
		ok, err := auth.AuthorizeTripAccess(ctx, db, u.UserID, req.TripID, u.Role)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "authorization failed"})
			return
		}
		if !ok {
			c.JSON(http.StatusForbidden, gin.H{"error": "not assigned to this trip"})
			return
		}
		result, err := tripSvc.StartTrip(ctx, req.TripID, u.UserID)
		if err != nil {
			writeTripError(c, req.TripID, err)
			return
		}
		writeTripResult(c, req.TripID, result, loadDriverTripSnapshot(ctx, db, cfg, fareSvc, req.TripID))
	}
}

// TripArrivedRequest body for POST /trip/arrived.
type TripArrivedRequest struct {
	TripID string `json:"trip_id" binding:"required"`
}

// TripArrived calls TripService.MarkArrived (driver at pickup). Requires driver auth.
func TripArrived(db *sql.DB, tripSvc *services.TripService, cfg *config.Config, fareSvc *services.FareService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		var req TripArrivedRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		ctx := c.Request.Context()
		ok, err := auth.AuthorizeTripAccess(ctx, db, u.UserID, req.TripID, u.Role)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "authorization failed"})
			return
		}
		if !ok {
			c.JSON(http.StatusForbidden, gin.H{"error": "not assigned to this trip"})
			return
		}
		// "I have arrived" is the driver's own claim about their position, and the
		// distance threshold is never authoritative anyway — the coordinates it
		// checks are self-reported. Gating it only produced a split-brain state:
		// the app showed ARRIVED while the server stayed WAITING, permanently,
		// because the app deliberately ignores proximity rejections here.
		//
		// So arrival is not distance-gated for anyone. Live-location freshness is
		// still required (an arrival from a driver with no recent location is
		// meaningless), and /trip/start keeps its threshold because that is the
		// point the metered fare begins.
		opts := services.MarkArrivedOptions{SkipPickupDistance: true}
		result, err := tripSvc.MarkArrivedWithOpts(ctx, req.TripID, u.UserID, opts)
		if err != nil {
			writeTripError(c, req.TripID, err)
			return
		}
		writeTripResult(c, req.TripID, result, loadDriverTripSnapshot(ctx, db, cfg, fareSvc, req.TripID))
	}
}

// TripFinish calls TripService.FinishTrip. Requires driver auth; driver may only finish their assigned trip.
func TripFinish(db *sql.DB, tripSvc *services.TripService, cfg *config.Config, fareSvc *services.FareService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		var req TripFinishRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		ctx := c.Request.Context()
		ok, err := auth.AuthorizeTripAccess(ctx, db, u.UserID, req.TripID, u.Role)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "authorization failed"})
			return
		}
		if !ok {
			c.JSON(http.StatusForbidden, gin.H{"error": "not assigned to this trip"})
			return
		}
		result, err := tripSvc.FinishTrip(ctx, req.TripID, u.UserID)
		if err != nil {
			writeTripError(c, req.TripID, err)
			return
		}
		writeTripResult(c, req.TripID, result, loadDriverTripSnapshot(ctx, db, cfg, fareSvc, req.TripID))
	}
}

// TripCancelDriver calls TripService.CancelByDriver. Requires driver auth; driver may only cancel their assigned trip.
func TripCancelDriver(db *sql.DB, tripSvc *services.TripService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		var req TripCancelDriverRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		ctx := c.Request.Context()
		ok, err := auth.AuthorizeTripAccess(ctx, db, u.UserID, req.TripID, u.Role)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "authorization failed"})
			return
		}
		if !ok {
			c.JSON(http.StatusForbidden, gin.H{"error": "not assigned to this trip"})
			return
		}
		result, err := tripSvc.CancelByDriver(ctx, req.TripID, u.UserID)
		if err != nil {
			writeTripError(c, req.TripID, err)
			return
		}
		// No snapshot on cancel: the trip is terminal and its ride request may have
		// been requeued to another driver, so there is nothing useful left to render.
		writeTripResult(c, req.TripID, result, nil)
	}
}

// TripCancelRider calls TripService.CancelByRider. Requires rider auth; rider may only cancel their own trip.
func TripCancelRider(db *sql.DB, tripSvc *services.TripService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleRider {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "rider auth required"})
			return
		}
		var req TripCancelRiderRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		ctx := c.Request.Context()
		ok, err := auth.AuthorizeTripAccess(ctx, db, u.UserID, req.TripID, u.Role)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "authorization failed"})
			return
		}
		if !ok {
			c.JSON(http.StatusForbidden, gin.H{"error": "not your trip"})
			return
		}
		result, err := tripSvc.CancelByRider(ctx, req.TripID, u.UserID)
		if err != nil {
			writeTripError(c, req.TripID, err)
			return
		}
		writeTripResult(c, req.TripID, result, nil)
	}
}

// TripInfo returns trip details for Mini App. Uses FareService for tiered fare when set; otherwise config. FINISHED uses stored fare_amount.
// Auth is optional: legacy Mini App / map clients call GET /trip/:id with only the trip UUID (capability URL).
// When a user is authenticated, only trip participants may read (404 for everyone else).
func TripInfo(db *sql.DB, cfg *config.Config, fareSvc *services.FareService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		tripID := c.Param("id")
		if tripID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "trip_id required"})
			return
		}
		ctx := c.Request.Context()
		var pickupLat, pickupLng, dropLat, dropLng sql.NullFloat64
		var driverUserID, riderUserID int64
		var assignedDriverUserID sql.NullInt64
		var status string
		var distanceM int64
		var fareAmount sql.NullInt64
		// Single SELECT: distance_m and fare_amount are the source of truth (live for STARTED, final for FINISHED).
		err := db.QueryRowContext(ctx, `
			SELECT t.status, t.driver_user_id, r.assigned_driver_user_id, t.rider_user_id, t.distance_m, t.fare_amount,
			       r.pickup_lat, r.pickup_lng, r.drop_lat, r.drop_lng
			FROM trips t
			JOIN ride_requests r ON r.id = t.request_id
			WHERE t.id = ?1`, tripID).Scan(&status, &driverUserID, &assignedDriverUserID, &riderUserID, &distanceM, &fareAmount, &pickupLat, &pickupLng, &dropLat, &dropLng)
		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "trip not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		// Effective driver id: normally trips.driver_user_id, but fall back to ride_requests.assigned_driver_user_id
		// to guard against rare inconsistent snapshots where status becomes WAITING but trips.driver_user_id is not yet populated.
		effectiveDriverUserID := driverUserID
		if effectiveDriverUserID == 0 && assignedDriverUserID.Valid && assignedDriverUserID.Int64 > 0 {
			effectiveDriverUserID = assignedDriverUserID.Int64
		}
		// Authenticated non-participants get 404 (avoid leaking existence). Anonymous capability-URL reads remain allowed.
		if u != nil && u.UserID != riderUserID && u.UserID != effectiveDriverUserID {
			c.JSON(http.StatusNotFound, gin.H{"error": "trip not found"})
			return
		}
		pickup := LatLng{pickupLat.Float64, pickupLng.Float64}
		drop := LatLng{0, 0}
		if dropLat.Valid && dropLng.Valid {
			drop = LatLng{dropLat.Float64, dropLng.Float64}
		}

		// Driver fields (best-effort, backward compatible with older DBs).
		var driverPhone, driverCarType, driverColor, driverPlate sql.NullString
		var driverFirstName, driverLastName, driverPlateNumber sql.NullString
		var lastLat, lastLng sql.NullFloat64
		var appLat, appLng sql.NullFloat64
		var appLast sql.NullString
		var appActive sql.NullInt64

		if effectiveDriverUserID != 0 {
			// Newer schema (preferred): includes app_* columns for native driver app GPS.
			qNew := `
				SELECT last_lat, last_lng, phone, car_type, color, plate, first_name, last_name, plate_number,
				       app_lat, app_lng, app_last_seen_at, COALESCE(app_location_active, 0)
				FROM drivers WHERE user_id = ?1`
			qLegacy := `
				SELECT last_lat, last_lng, phone, car_type, color, plate, first_name, last_name, plate_number
				FROM drivers WHERE user_id = ?1`
			rowErr := db.QueryRowContext(ctx, qNew, effectiveDriverUserID).
				Scan(&lastLat, &lastLng, &driverPhone, &driverCarType, &driverColor, &driverPlate, &driverFirstName, &driverLastName, &driverPlateNumber, &appLat, &appLng, &appLast, &appActive)
			if rowErr != nil && strings.Contains(strings.ToLower(rowErr.Error()), "no such column") {
				_ = db.QueryRowContext(ctx, qLegacy, effectiveDriverUserID).
					Scan(&lastLat, &lastLng, &driverPhone, &driverCarType, &driverColor, &driverPlate, &driverFirstName, &driverLastName, &driverPlateNumber)
				appLat, appLng = sql.NullFloat64{}, sql.NullFloat64{}
				appLast = sql.NullString{}
				appActive = sql.NullInt64{Int64: 0, Valid: true}
			}
		}

		driverPos := LatLng{0, 0}
		if lastLat.Valid && lastLng.Valid {
			driverPos = LatLng{lastLat.Float64, lastLng.Float64}
		}

		// Load driver display name from users as a fallback.
		var driverUserName sql.NullString
		_ = db.QueryRowContext(ctx, `SELECT name FROM users WHERE id = ?1`, effectiveDriverUserID).Scan(&driverUserName)

		// Fallback: if drivers.phone is empty, use users.phone (same behavior as Telegram rider assignment notification).
		var driverUserPhone sql.NullString
		_ = db.QueryRowContext(ctx, `SELECT phone FROM users WHERE id = ?1`, effectiveDriverUserID).Scan(&driverUserPhone)

		driverObj := (*DriverObject)(nil)
		if effectiveDriverUserID != 0 && (status == domain.TripStatusWaiting || status == domain.TripStatusArrived || status == domain.TripStatusStarted) {
			name := strings.TrimSpace(driverUserName.String)
			if driverFirstName.Valid || driverLastName.Valid {
				fn := strings.TrimSpace(driverFirstName.String)
				ln := strings.TrimSpace(driverLastName.String)
				full := strings.TrimSpace(strings.TrimSpace(fn + " " + ln))
				if full != "" {
					name = full
				}
			}
			plate := strings.TrimSpace(driverPlateNumber.String)
			if plate == "" {
				plate = strings.TrimSpace(driverPlate.String)
			}
			car := &DriverCar{
				Make:  strings.TrimSpace(driverCarType.String),
				Model: "",
				Color: strings.TrimSpace(driverColor.String),
				Plate: plate,
			}

			phone := strings.TrimSpace(driverPhone.String)
			if phone == "" && driverUserPhone.Valid {
				phone = strings.TrimSpace(driverUserPhone.String)
			}
			// Anonymous capability-URL reads get the map payload, not contact details.
			if u == nil {
				phone = ""
			}

			loc := (*DriverLocationObject)(nil)
			// Prefer effective driver location (native app GPS if active+fresh), else Telegram last_lat/lng.
			eLoc := services.EffectiveDriverLocation{
				AppLat:            appLat,
				AppLng:            appLng,
				AppLastSeenAt:     appLast,
				AppLocationActive: appActive,
				LastLat:           lastLat,
				LastLng:           lastLng,
			}
			eLat, eLng := services.GetEffectiveDriverLocation(eLoc)
			var latPtr, lngPtr *float64
			if eLat != 0 || eLng != 0 {
				lat := eLat
				lng := eLng
				latPtr = &lat
				lngPtr = &lng
				loc = &DriverLocationObject{Lat: lat, Lng: lng}
			}
			driverObj = &DriverObject{
				ID:       fmt.Sprintf("%d", effectiveDriverUserID),
				Name:     name,
				Phone:    phone,
				Car:      car,
				Location: loc,
				Lat:      latPtr,
				Lng:      lngPtr,
			}
		}
		var riderPhone, riderName sql.NullString
		_ = db.QueryRowContext(ctx, `SELECT phone, name FROM users WHERE id = ?1`, riderUserID).Scan(&riderPhone, &riderName)

		distanceKm := float64(distanceM) / 1000
		var computedFare int64
		if fareSvc != nil {
			computedFare, _ = fareSvc.CalculateFare(ctx, distanceKm)
		} else if cfg != nil {
			computedFare = utils.CalculateFareRounded(float64(cfg.StartingFee), float64(cfg.PricePerKm), distanceKm)
		}
		fare, fareAmountPtr := TripFareForResponse(status, fareAmount, computedFare)
		resp := TripInfoResponse{
			ID:           tripID,
			Driver:       driverObj,
			TripID:       tripID,
			Status:       status,
			Pickup:       pickup,
			Drop:         drop,
			DriverLegacy: driverPos,
			DriverPos:    driverPos,
			DistanceKm:   distanceKm,
			Fare:         fare,
			Trip: &TripSummary{
				ID:         tripID,
				Status:     status,
				DistanceM:  distanceM,
				DistanceKm: distanceKm,
				Fare:       fare,
				FareAmount: fareAmountPtr,
			},
		}
		// Top-level driver_id is for driver / anonymous map clients; omit for authenticated riders
		// (nested driver.id remains for display).
		if u == nil || u.Role == domain.RoleDriver {
			resp.DriverID = effectiveDriverUserID
		}
		// Contact details are for participants only. An anonymous caller holding
		// just the trip UUID gets the map payload (status, coordinates, fare) but
		// never phone numbers or names: the UUID travels in tracking links, query
		// strings and logs, so it is a weak secret to hang PII on — and it never
		// expires, so a leaked id would otherwise expose both parties forever.
		if u != nil {
			if riderPhone.Valid {
				resp.RiderPhone = riderPhone.String
			}
			if riderName.Valid {
				resp.RiderName = riderName.String
			}
		}
		if u != nil && (riderPhone.Valid && riderPhone.String != "" || riderName.Valid && riderName.String != "") {
			resp.RiderInfo = &struct {
				Phone string `json:"phone,omitempty"`
				Name  string `json:"name,omitempty"`
			}{
				Phone: riderPhone.String,
				Name:  riderName.String,
			}
		}
		// driver_info only when a real assigned driver is present (same as nested driver). Avoids WAITING/no-driver showing junk or rider-like fields.
		if driverObj != nil {
			plate := strings.TrimSpace(driverPlateNumber.String)
			if plate == "" {
				plate = strings.TrimSpace(driverPlate.String)
			}
			phone := strings.TrimSpace(driverPhone.String)
			if phone == "" && driverUserPhone.Valid {
				phone = strings.TrimSpace(driverUserPhone.String)
			}
			// Same rule as the rider side above: an anonymous tracking link gets the
			// map payload (plate, car, colour) but not the driver's phone number.
			if u == nil {
				phone = ""
			}
			if phone != "" || strings.TrimSpace(driverCarType.String) != "" || strings.TrimSpace(driverColor.String) != "" || plate != "" {
				resp.DriverInfo = &struct {
					Phone   string `json:"phone,omitempty"`
					CarType string `json:"car_type,omitempty"`
					Color   string `json:"color,omitempty"`
					Plate   string `json:"plate,omitempty"`
				}{
					Phone:   phone,
					CarType: strings.TrimSpace(driverCarType.String),
					Color:   strings.TrimSpace(driverColor.String),
					Plate:   plate,
				}
			}
		}
		c.JSON(http.StatusOK, resp)
	}
}
