package ws

import (
	"database/sql"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/logger"
	"taxi-mvp/internal/safe"
)

// RiderAccessTokenVerifier validates native rider access JWTs (same contract as
// services.RiderAuthService.VerifyAccessToken). Passed from server to avoid an
// import cycle (services imports ws for TripService broadcasts).
type RiderAccessTokenVerifier interface {
	VerifyAccessToken(token string) (int64, error)
}

const headerInitData = "X-Telegram-Init-Data"

// RiderWSTicketRedeemer redeems a one-time WS ticket issued by
// POST /v1/rider/ws-ticket (services.RiderWSTicketService).
type RiderWSTicketRedeemer interface {
	Redeem(ticket string) (int64, bool)
}

// ServeWsOpts configures WebSocket auth credential sources.
type ServeWsOpts struct {
	EnableDriverIDHeader bool
	// AllowQueryCreds enables init_data / driver_id query fallbacks
	// (local/dev only). access_token is always accepted because browser and
	// Flutter WebSocket APIs may not support custom upgrade headers.
	AllowQueryCreds bool
	DriverTokens    auth.DriverTokenVerifier
	// RiderTickets redeems ?ticket= one-time credentials (web token hygiene:
	// keeps long-lived JWTs out of URLs). Optional.
	RiderTickets RiderWSTicketRedeemer
}

// riderAccessTokenFromRequest returns the native rider JWT from Authorization: Bearer
// or from query access_token. Query tokens are supported in hardened mode because
// browser and Flutter WebSocket clients cannot reliably set upgrade headers.
// HTTP access logs intentionally omit query strings.
func riderAccessTokenFromRequest(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if token != "" {
		return token
	}
	return strings.TrimSpace(r.URL.Query().Get("access_token"))
}

func initDataFromRequest(r *http.Request, allowQuery bool) string {
	initData := strings.TrimSpace(r.Header.Get(headerInitData))
	if initData == "" && allowQuery {
		initData = strings.TrimSpace(r.URL.Query().Get("init_data"))
	}
	return initData
}

// ServeWs handles GET /ws?trip_id=xxx and upgrades to WebSocket (no auth). Use ServeWsWithAuth for protected WS.
func ServeWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	tripID := strings.TrimSpace(r.URL.Query().Get("trip_id"))
	if tripID == "" {
		http.Error(w, "trip_id required", http.StatusBadRequest)
		return
	}
	conn, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade failed path=%s reason=%v", r.URL.Path, err)
		return
	}
	client := &client{
		hub:     hub,
		conn:    conn,
		send:    make(chan []byte, 256),
		tripIDs: map[string]struct{}{tripID: {}},
	}
	client.hub.register <- client
	safe.Go("ws_write_pump", client.writePump)
	client.readPump()
}

// ServeWsWithAuth requires Telegram Mini App auth; only rider or assigned driver of the trip may subscribe.
// When enableDriverIDHeader is true and init data is absent, accepts header X-Driver-Id (internal driver user id) for drivers only.
// When riderAuth is non-nil, also accepts the native rider access JWT via Authorization: Bearer
// or ?access_token=. Query bearer tokens remain available when ID/query debug auth is disabled.
func ServeWsWithAuth(hub *Hub, db *sql.DB, driverBotToken, riderBotToken string, opts ServeWsOpts, riderAuth RiderAccessTokenVerifier, w http.ResponseWriter, r *http.Request) {
	tripID := strings.TrimSpace(r.URL.Query().Get("trip_id"))
	if tripID == "" {
		http.Error(w, "trip_id required", http.StatusBadRequest)
		return
	}
	initData := initDataFromRequest(r, opts.AllowQueryCreds)
	ctx := r.Context()
	var userID int64
	var role string

	if initData != "" {
		var telegramUserID int64
		var err error
		telegramUserID, err = auth.VerifyMiniAppInitData(driverBotToken, initData)
		if err != nil {
			telegramUserID, err = auth.VerifyMiniAppInitData(riderBotToken, initData)
		}
		if err != nil {
			logger.AuthFailure("invalid init data")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid init data"}`))
			return
		}
		userID, role, err = auth.ResolveUserFromTelegramID(ctx, db, telegramUserID)
		if err != nil {
			logger.AuthFailure("user not found")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"user not found"}`))
			return
		}
	} else if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" && opts.RiderTickets != nil {
		// One-time ticket (rider web builds). Redeemed exactly once; role is
		// fixed to rider because only RequireRiderBearerAuth can issue one.
		uid, ok := opts.RiderTickets.Redeem(ticket)
		if !ok || uid <= 0 {
			logger.AuthFailure("invalid ws ticket")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid token"}`))
			return
		}
		userID = uid
		role = domain.RoleRider
	} else if token := riderAccessTokenFromRequest(r); token != "" {
		// Prefer opaque driver session, then rider JWT (same Authorization header).
		if opts.DriverTokens != nil {
			if uid, err := opts.DriverTokens.Verify(ctx, token); err == nil && uid > 0 {
				var ver sql.NullString
				err = db.QueryRowContext(ctx, `SELECT verification_status FROM drivers WHERE user_id = ?1`, uid).Scan(&ver)
				if err == nil && strings.EqualFold(strings.TrimSpace(ver.String), "approved") {
					userID = uid
					role = domain.RoleDriver
				}
			}
		}
		if userID == 0 && riderAuth != nil {
			var err error
			userID, err = riderAuth.VerifyAccessToken(token)
			if err != nil || userID <= 0 {
				logger.AuthFailure("invalid rider access token")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"invalid token"}`))
				return
			}
			// Existence only — do not require users.role=rider (dual bot users
			// often have role=driver after the driver bot UPSERT).
			var exists int
			if err := db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?1`, userID).Scan(&exists); err != nil {
				if err == sql.ErrNoRows {
					logger.AuthFailure("user not found")
					w.WriteHeader(http.StatusUnauthorized)
					w.Write([]byte(`{"error":"user not found"}`))
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			role = domain.RoleRider
		}
		if userID == 0 {
			logger.AuthFailure("invalid bearer token")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid token"}`))
			return
		}
	} else if opts.EnableDriverIDHeader {
		driverIDStr := strings.TrimSpace(r.Header.Get(auth.HeaderDriverID))
		if driverIDStr == "" {
			logger.AuthFailure("missing init data")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"missing init data"}`))
			return
		}
		uid, st := auth.ResolveDriverUserForXDriverID(ctx, db, driverIDStr)
		switch st {
		case auth.XDriverIDOK:
			userID = uid
			role = domain.RoleDriver
		case auth.XDriverIDBadFormat:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid X-Driver-Id (digits only: internal user id or Telegram user id)"}`))
			return
		case auth.XDriverIDNotFound:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unknown driver id (use internal user id from admin/users, or Telegram user id)"}`))
			return
		case auth.XDriverIDNotApproved:
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"driver not approved"}`))
			return
		default:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"driver authentication failed"}`))
			return
		}
	} else {
		logger.AuthFailure("missing init data")
		// No credential found at all — log which sources were present (flags only)
		// so a stale client is identifiable from production logs.
		log.Printf("ws: trip ws auth missing path=%s %s", r.URL.Path, authPresenceSummary(r))
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"missing init data"}`))
		return
	}

	allowed, err := auth.AuthorizeTripAccess(ctx, db, userID, tripID, role)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !allowed {
		logger.AuthFailure("not authorized for this trip", slog.String("trip_id", tripID), slog.Int64("user_id", userID))
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"not authorized for this trip"}`))
		return
	}
	conn, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade failed path=%s trip_id=%s reason=%v", r.URL.Path, tripID, err)
		return
	}
	client := &client{
		hub:     hub,
		conn:    conn,
		send:    make(chan []byte, 256),
		tripIDs: map[string]struct{}{tripID: {}},
		userID:  userID,
	}
	client.hub.register <- client
	logger.WebSocketEvent("connect", tripID, userID)
	safe.Go("ws_write_pump", client.writePump)
	client.readPump()
}

func (c *client) readPump() {
	defer func() {
		for tripID := range c.tripIDs {
			logger.WebSocketEvent("disconnect", tripID, c.userID)
			break
		}
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(msg)
			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
