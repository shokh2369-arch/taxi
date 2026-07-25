package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
)

// RiderRequestDeps wires native rider ride-request HTTP handlers (Bearer auth).
type RiderRequestDeps struct {
	DB           *sql.DB
	Cfg          *config.Config
	RiderAuthSvc *services.RiderAuthService
	RiderReqSvc  *services.RiderRequestAppService
	TripSvc      *services.TripService
}

type riderCreateRequestBody struct {
	PickupLat       float64 `json:"pickup_lat"`
	PickupLng       float64 `json:"pickup_lng"`
	ClientRequestID string  `json:"client_request_id"`
}

type riderAppDestinationBody struct {
	DropLat  float64 `json:"drop_lat"`
	DropLng  float64 `json:"drop_lng"`
	DropName string  `json:"drop_name"`
}

// floatFromRaw unmarshals the first present key in raw JSON object m.
func floatFromRaw(m map[string]json.RawMessage, keys ...string) (v float64, ok bool, err error) {
	for _, k := range keys {
		if raw, found := m[k]; found {
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, err
			}
			return v, true, nil
		}
	}
	return 0, false, nil
}

func stringFromRaw(m map[string]json.RawMessage, keys ...string) (s string, ok bool, err error) {
	for _, k := range keys {
		if raw, found := m[k]; found {
			if err := json.Unmarshal(raw, &s); err != nil {
				return "", false, err
			}
			return strings.TrimSpace(s), true, nil
		}
	}
	return "", false, nil
}

func bindRiderCreateBody(c *gin.Context) (riderCreateRequestBody, error) {
	var m map[string]json.RawMessage
	if err := c.ShouldBindJSON(&m); err != nil {
		return riderCreateRequestBody{}, err
	}
	lat, latOK, err := floatFromRaw(m, "pickup_lat", "pickupLat")
	if err != nil {
		return riderCreateRequestBody{}, err
	}
	lng, lngOK, err := floatFromRaw(m, "pickup_lng", "pickupLng")
	if err != nil {
		return riderCreateRequestBody{}, err
	}
	if !latOK || !lngOK {
		return riderCreateRequestBody{}, errors.New("missing pickup coordinates")
	}
	var clientID string
	if s, ok, err := stringFromRaw(m, "client_request_id", "clientRequestId"); err != nil {
		return riderCreateRequestBody{}, err
	} else if ok {
		clientID = s
	}
	return riderCreateRequestBody{PickupLat: lat, PickupLng: lng, ClientRequestID: clientID}, nil
}

func bindRiderDestinationBody(c *gin.Context) (riderAppDestinationBody, error) {
	var m map[string]json.RawMessage
	if err := c.ShouldBindJSON(&m); err != nil {
		return riderAppDestinationBody{}, err
	}
	lat, latOK, err := floatFromRaw(m, "drop_lat", "dropLat")
	if err != nil {
		return riderAppDestinationBody{}, err
	}
	lng, lngOK, err := floatFromRaw(m, "drop_lng", "dropLng")
	if err != nil {
		return riderAppDestinationBody{}, err
	}
	if !latOK || !lngOK {
		return riderAppDestinationBody{}, errors.New("missing drop coordinates")
	}
	var name string
	if s, ok, err := stringFromRaw(m, "drop_name", "dropName"); err != nil {
		return riderAppDestinationBody{}, err
	} else if ok {
		name = s
	}
	return riderAppDestinationBody{DropLat: lat, DropLng: lng, DropName: name}, nil
}

// RegisterRiderRequestRoutes mounts Bearer-authenticated ride request routes
// under /v1/rider/requests* (same DB + dispatch path as Telegram rider bot).
func RegisterRiderRequestRoutes(r *gin.Engine, deps RiderRequestDeps) {
	if r == nil || deps.DB == nil || deps.RiderAuthSvc == nil || deps.RiderReqSvc == nil {
		return
	}
	bearer := RequireRiderBearerAuth(deps.RiderAuthSvc, deps.DB)
	g := r.Group("/v1/rider")
	g.Use(bearer)
	g.POST("/requests", riderAppCreateRequest(deps))
	g.POST("/requests/:id/destination", riderAppSetDestination(deps))
	g.POST("/requests/:id/confirm", riderAppConfirmRequest(deps))
	// Cancellation by request_id (native app, Bearer JWT). Used during the "waiting for driver" window.
	g.POST("/requests/:id/cancel", riderAppCancelRequestByPath(deps))
	g.POST("/requests/cancel", riderAppCancelRequestByBody(deps))
}

func riderUserID(c *gin.Context) (int64, bool) {
	u := auth.UserFromContext(c.Request.Context())
	if u == nil || u.UserID <= 0 {
		writeRiderAPIError(c, http.StatusUnauthorized, "invalid_token", "Kirish talab qilinadi.")
		return 0, false
	}
	return u.UserID, true
}

func riderAppCreateRequest(deps RiderRequestDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		body, err := bindRiderCreateBody(c)
		if err != nil {
			writeRiderAPIError(c, http.StatusBadRequest, "invalid_body", "Yuborilgan ma‘lumot noto‘g‘ri.")
			return
		}
		reqID, err := deps.RiderReqSvc.CreatePickupRequest(c.Request.Context(), uid, body.PickupLat, body.PickupLng, strings.TrimSpace(body.ClientRequestID))
		if err != nil {
			// Pass rider context so a duplicate_pending carries the blocking request.
			mapRiderRequestErrorWithContext(c, err, deps.DB, uid)
			return
		}
		c.JSON(http.StatusOK, gin.H{"request_id": reqID, "requestId": reqID})
	}
}

func riderAppSetDestination(deps RiderRequestDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		body, err := bindRiderDestinationBody(c)
		if err != nil {
			writeRiderAPIError(c, http.StatusBadRequest, "invalid_body", "Yuborilgan ma‘lumot noto‘g‘ri.")
			return
		}
		est, err := deps.RiderReqSvc.SetDestination(c.Request.Context(), uid, id, body.DropLat, body.DropLng, body.DropName)
		if err != nil {
			mapRiderRequestError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":              true,
			"estimated_price": est,
			"estimatedPrice":  est,
		})
	}
}

func riderAppConfirmRequest(deps RiderRequestDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		if err := deps.RiderReqSvc.ConfirmRequest(c.Request.Context(), uid, id); err != nil {
			mapRiderRequestError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

type riderCancelRequestBody struct {
	RequestID string `json:"request_id"`
}

func riderAppCancelRequestByPath(deps RiderRequestDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		requestID := strings.TrimSpace(c.Param("id"))
		if requestID == "" {
			writeRiderAPIError(c, http.StatusBadRequest, "invalid_body", "Yuborilgan ma‘lumot noto‘g‘ri.")
			return
		}
		riderAppCancelRequest(c, deps, uid, requestID)
	}
}

func riderAppCancelRequestByBody(deps RiderRequestDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		var m map[string]json.RawMessage
		if err := c.ShouldBindJSON(&m); err != nil {
			writeRiderAPIError(c, http.StatusBadRequest, "invalid_body", "Yuborilgan ma‘lumot noto‘g‘ri.")
			return
		}
		id, okID, err := stringFromRaw(m, "request_id", "requestId")
		if err != nil || !okID || strings.TrimSpace(id) == "" {
			writeRiderAPIError(c, http.StatusBadRequest, "invalid_body", "Yuborilgan ma‘lumot noto‘g‘ri.")
			return
		}
		riderAppCancelRequest(c, deps, uid, strings.TrimSpace(id))
	}
}

func riderAppCancelRequest(c *gin.Context, deps RiderRequestDeps, riderUserID int64, requestID string) {
	ctx := c.Request.Context()
	out, err := deps.RiderReqSvc.CancelRequest(ctx, riderUserID, requestID, deps.TripSvc)
	if err != nil {
		// Cancel endpoint has stricter error codes required by the native app.
		switch {
		case errors.Is(err, services.ErrRiderRequestConflictState):
			writeRiderAPIError(c, http.StatusConflict, "invalid_state", "So‘rov holati noto‘g‘ri yoki muddati tugagan.")
		default:
			mapRiderRequestError(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, out)
}

// mapRiderRequestError maps service errors to the rider JSON envelope.
//
// db and riderUserID are optional; when supplied, duplicate_pending carries the
// blocking request so the client can act on it instead of leaving the rider to
// wait out the 30-minute abandoned-request timeout with no way forward.
func mapRiderRequestError(c *gin.Context, err error) {
	mapRiderRequestErrorWithContext(c, err, nil, 0)
}

func mapRiderRequestErrorWithContext(c *gin.Context, err error, db *sql.DB, riderUserID int64) {
	switch {
	case errors.Is(err, services.ErrRiderRequestLegalRequired):
		writeRiderAPIError(c, http.StatusForbidden, "legal_required", "Iltimos, avval foydalanuvchi shartlari va maxfiylik siyosatini qabul qiling.")
	case errors.Is(err, services.ErrRiderRequestPhoneRequired):
		writeRiderAPIError(c, http.StatusForbidden, "phone_required", "Telefon raqamingiz profilda yo‘q. Iltimos, rider botda kontakt yuboring.")
	case errors.Is(err, services.ErrRiderRequestAbuseBlocked):
		writeRiderAPIError(c, http.StatusForbidden, "abuse_blocked", "Buyurtma vaqtincha cheklangan. Keyinroq qayta urinib ko‘ring.")
	case errors.Is(err, services.ErrRiderRequestDuplicatePending):
		// Include the blocking request so the client can offer to cancel it. Without
		// this the only escape from an abandoned request is the 30-minute server-side
		// timeout, which reads to the rider as "the app is broken".
		details := gin.H{}
		if db != nil && riderUserID > 0 {
			var id string
			var confirmed int
			if e := db.QueryRowContext(c.Request.Context(), `
				SELECT id, COALESCE(destination_confirmed, 0)
				FROM ride_requests
				WHERE rider_user_id = ?1 AND status = ?2
				ORDER BY created_at DESC LIMIT 1`,
				riderUserID, domain.RequestStatusPending).Scan(&id, &confirmed); e == nil && id != "" {
				details["pending_request_id"] = id
				// false means the rider never confirmed a destination, so this request
				// was never dispatched and is safe to cancel and start over.
				details["destination_confirmed"] = confirmed == 1
			}
		}
		if len(details) > 0 {
			writeRiderAPIErrorWithDetails(c, http.StatusConflict, "duplicate_pending",
				"Sizda allaqachon faol so‘rov bor. Haydovchi topilguncha yoki bekor qilinguncha kuting.", details)
			return
		}
		writeRiderAPIError(c, http.StatusConflict, "duplicate_pending", "Sizda allaqachon faol so‘rov bor. Haydovchi topilguncha yoki bekor qilinguncha kuting.")
	case errors.Is(err, services.ErrRiderRequestNotFound):
		writeRiderAPIError(c, http.StatusNotFound, "not_found", "So‘rov topilmadi.")
	case errors.Is(err, services.ErrRiderRequestNotYours):
		writeRiderAPIError(c, http.StatusForbidden, "not_your_request", "So‘rov sizga tegishli emas.")
	case errors.Is(err, services.ErrRiderRequestConflictState):
		writeRiderAPIError(c, http.StatusConflict, "conflict", "So‘rov holati noto‘g‘ri yoki muddati tugagan.")
	case errors.Is(err, services.ErrRiderRequestInvalidCoords):
		writeRiderAPIError(c, http.StatusBadRequest, "invalid_coordinates", "Koordinatalar noto‘g‘ri.")
	case errors.Is(err, services.ErrRiderRequestMatchUnavailable):
		writeRiderAPIError(c, http.StatusServiceUnavailable, "service_unavailable", "Xizmat vaqtincha ishlamayapti.")
	default:
		writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
	}
}
