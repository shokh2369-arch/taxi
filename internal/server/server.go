package server

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/handlers"
	"taxi-mvp/internal/repositories"
	"taxi-mvp/internal/safe"
	"taxi-mvp/internal/services"
	"taxi-mvp/internal/ws"
)

// mirrorDriverWSCredentialsIntoHeaders copies query-only credentials onto the canonical HTTP headers
// before tryDriverID + RequireDriverAuth run. Only when allowQuery is true (debug/dev flags).
// If headers are already set, query is ignored.
func mirrorDriverWSCredentialsIntoHeaders(allowQuery bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowQuery {
			c.Next()
			return
		}
		if strings.TrimSpace(c.GetHeader(auth.HeaderDriverID)) == "" {
			for _, key := range []string{"driver_id", "x_driver_id"} {
				if q := strings.TrimSpace(c.Query(key)); q != "" {
					c.Request.Header.Set(auth.HeaderDriverID, q)
					break
				}
			}
		}
		if strings.TrimSpace(c.GetHeader(auth.HeaderInitData)) == "" {
			if q := strings.TrimSpace(c.Query("init_data")); q != "" {
				c.Request.Header.Set(auth.HeaderInitData, q)
			}
		}
		c.Next()
	}
}

// New creates a Gin engine with API routes and optional webapp static files.
// hub can be nil; if set, GET /ws is registered. fareSvc can be nil (then fare uses config only).
// matchSvc and driverBot are used for driver auto-availability and notifications (e.g. after trip finish + Mini App location).
// assignSvc is used for POST /driver/accept-request (same TryAssign as the driver bot); may be nil (then accept returns 503).
// riderBot is optional; used for rider referral link (bot username).
// riderAuthSvc is optional; if non-nil, registers /v1/rider/auth/* (request-code, verify-code, refresh, logout)
// and Bearer-authenticated /v1/rider/legal/* (active documents + accept) for the native rider app.
// riderReqSvc is optional; if non-nil together with riderAuthSvc, registers Bearer-authenticated /v1/rider/requests* (native app ride flow).
func New(db *sql.DB, cfg *config.Config, tripSvc *services.TripService, matchSvc *services.MatchService, assignSvc *services.AssignmentService, driverBot *tgbotapi.BotAPI, riderBot *tgbotapi.BotAPI, hub *ws.Hub, fareSvc *services.FareService, riderAuthSvc *services.RiderAuthService, riderReqSvc *services.RiderRequestAppService) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// Avoid gin's default access logger which may include full query strings
	// (e.g. Telegram init_data on websocket requests), causing large stdout/stderr.
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())
	r.Use(func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		c.Next()
		// Do not log health checks (cron/uptime monitors may hit frequently; keep stdout minimal).
		if path == "/" || path == "/health" {
			return
		}
		status := c.Writer.Status()
		// Keep logs small: do not log query strings or request bodies.
		log.Printf("http_request method=%s path=%s status=%d dur_ms=%d", c.Request.Method, path, status, time.Since(start).Milliseconds())
	})

	ws.ConfigureCheckOrigin(cfg.WSAllowedOrigins)

	// "/" stays a constant so a bare root check is free.
	rootHandler := func(c *gin.Context) {
		c.Data(200, "text/plain; charset=utf-8", []byte("OK"))
	}
	r.GET("/", rootHandler)
	r.HEAD("/", rootHandler)

	// /health verifies the dependency the service cannot run without.
	//
	// It used to return a constant, which meant it could not fail: Render gates
	// deploys and detects wedged instances with it, and the keepalive cron asserts
	// it returns 200 — so the instance reported healthy with Turso unreachable.
	// The DB check is cached briefly so a monitor polling every few seconds does
	// not add load, and HEAD stays body-free for cheap probes.
	r.GET("/health", healthHandler(db))
	r.HEAD("/health", healthHandler(db))

	driverSessions := repositories.NewDriverAuthSessionsRepo(db)
	driverTokens := services.NewDriverAuthTokenService(driverSessions)

	// Native driver login: phone + OTP via existing Telegram driver bot (no Telegram init_data).
	r.POST("/auth/request-code", handlers.DriverAuthRequestCode(db, driverBot))
	r.POST("/auth/verify-code", handlers.DriverAuthVerifyCode(db, driverTokens))

	// Native rider login (Flutter rider app): phone + OTP via existing Telegram
	// rider bot. Issues access_token + refresh_token + expires_in. The four
	// endpoints under /v1/rider/auth/* are wired only when riderAuthSvc is set
	// (so test harnesses that don't need login can pass nil).
	if riderAuthSvc != nil {
		handlers.RegisterRiderAuthRoutes(r, handlers.RiderAuthDeps{Service: riderAuthSvc})
		handlers.RegisterRiderAppLegalRoutes(r, db, riderAuthSvc)
		handlers.RegisterRiderNotificationRoutes(r, handlers.RiderNotificationDeps{
			DB: db, RiderAuthSvc: riderAuthSvc,
		})
		handlers.RegisterRiderAccountRoutes(r, db, riderAuthSvc)
	}
	// One-time WS tickets (web token hygiene). Issued only to authenticated
	// riders; redeemed by GET /ws below.
	var riderWSTickets *services.RiderWSTicketService
	if riderAuthSvc != nil {
		riderWSTickets = services.NewRiderWSTicketService()
		handlers.RegisterRiderWSTicketRoutes(r, db, riderAuthSvc, riderWSTickets)
	}
	if riderAuthSvc != nil && riderReqSvc != nil {
		handlers.RegisterRiderRequestRoutes(r, handlers.RiderRequestDeps{
			DB: db, Cfg: cfg, RiderAuthSvc: riderAuthSvc, RiderReqSvc: riderReqSvc, TripSvc: tripSvc,
		})
	}
	if riderAuthSvc != nil && tripSvc != nil {
		handlers.RegisterRiderTripRoutes(r, handlers.RiderTripDeps{
			DB: db, RiderAuthSvc: riderAuthSvc, TripSvc: tripSvc,
		})
	}

	driverHdr := auth.DriverIDHeaderMiddlewareOpts{Enable: cfg.EnableDriverIDHeader, Debug: cfg.DriverAuthDebug}
	tryDriverID := auth.TryDriverIDHeader(db, driverHdr)
	tryDriverBearer := auth.TryDriverBearerAuth(db, driverTokens)
	tryRiderBearer := func(c *gin.Context) { c.Next() }
	if riderAuthSvc != nil {
		tryRiderBearer = auth.TryRiderBearerAuth(db, riderAuthSvc)
	}
	driverAuth := auth.RequireDriverAuth(db, cfg.DriverBotToken, cfg.EnableDriverIDHeader)
	driverActionSrc := auth.InjectDriverActionSource()
	riderAuth := auth.RequireRiderAuth(db, cfg.RiderBotToken)
	appUserAuth := auth.RequireMiniAppAuthDriverOrRider(db, cfg.DriverBotToken, cfg.RiderBotToken)
	allowWSQueryCreds := cfg.DriverAuthDebug || cfg.EnableDriverIDHeader

	// Driver dispatch websocket (best-effort poke). Optional global singleton used by publisher hooks.
	// Supervised, not just recovered: Run is the only reader of register/unregister/broadcast, so if it
	// stops, dispatch pokes are dropped silently, unregister never closes send channels, and the handler
	// blocks forever once the register channel backs up. Its state is owned by this one goroutine and
	// guarded by no mutex, so restarting it cannot leave a lock held.
	dispatchHub := ws.NewDispatchHub()
	ws.DispatchHubDefault = dispatchHub
	safe.GoSupervised(context.Background(), "dispatch_hub", dispatchHub.Run)

	// Single-session driver login: when a new login revokes the old sessions,
	// push session_revoked to the old device's live sockets and close them.
	driverTokens.OnSessionsRevoked = func(userID int64) {
		dispatchHub.NotifySessionRevoked(userID)
		hub.NotifySessionRevoked(userID) // nil-safe
	}

	if hub != nil {
		r.GET("/ws", func(c *gin.Context) {
			ws.ServeWsWithAuth(hub, db, cfg.DriverBotToken, cfg.RiderBotToken, ws.ServeWsOpts{
				EnableDriverIDHeader: cfg.EnableDriverIDHeader,
				AllowQueryCreds:      allowWSQueryCreds,
				DriverTokens:         driverTokens,
				RiderTickets:         riderWSTickets,
			}, riderAuthSvc, c.Writer, c.Request)
		})
	}
	// Always register dispatch websocket; it is independent of the trip hub.
	r.GET("/ws/driver-dispatch", mirrorDriverWSCredentialsIntoHeaders(allowWSQueryCreds), tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, func(c *gin.Context) {
		ws.ServeDriverDispatchWs(dispatchHub, c.Writer, c.Request)
	})

	// GET /trip/:id: auth optional (legacy Mini App capability URL). Prefer Bearer / initData / trip-scoped driver_id when present.
	tryTripDriverID := auth.TryTripScopedDriverID(db)
	optionalTripAuth := auth.TryOptionalMiniAppAuthDriverOrRider(db, cfg.DriverBotToken, cfg.RiderBotToken)
	r.GET("/trip/:id", tryDriverID, tryTripDriverID, tryDriverBearer, tryRiderBearer, optionalTripAuth, handlers.TripInfo(db, cfg, fareSvc))
	// Mini App: try X-Driver-Id first so Start/Cancel/Finish work without initData when header is present
	r.POST("/driver/location", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverLocation(db, tripSvc, matchSvc, driverBot, hub, cfg, fareSvc))
	// Native driver app location (additive). Does not touch Telegram location fields.
	r.POST("/driver/location/app", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverAppLocation(db, matchSvc))
	r.POST("/driver/offline", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverManualOffline(db))
	r.POST("/driver/online", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverGoOnline(db))
	r.POST("/trip/start", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.TripStart(db, tripSvc, cfg, fareSvc))
	r.POST("/trip/arrived", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.TripArrived(db, tripSvc, cfg, fareSvc))
	r.POST("/trip/finish", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.TripFinish(db, tripSvc, cfg, fareSvc))
	r.POST("/trip/cancel/driver", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.TripCancelDriver(db, tripSvc))
	r.GET("/driver/referral-link", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverReferralLink(db, driverBot))
	r.GET("/driver/promo-program", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverPromoProgram(db))
	r.GET("/driver/tariff", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverTariff(db, cfg, fareSvc))
	r.GET("/driver/referral-status", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverReferralStatus(db))
	r.GET("/driver/available-requests", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverAvailableRequests(db, cfg, fareSvc))
	r.GET("/driver/trips", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverTrips(db))
	r.POST("/driver/accept-request", tryDriverID, tryDriverBearer, driverAuth, driverActionSrc, handlers.DriverAcceptRequest(db, assignSvc, tripSvc, cfg, fareSvc))
	r.POST("/trip/cancel/rider", riderAuth, handlers.TripCancelRider(db, tripSvc))
	r.GET("/rider/referral-link", riderAuth, handlers.RiderReferralLink(db, riderBot))

	// Mini App (Custom Location) reliable confirm: server-side destination set + estimated price + dispatch.
	// Route-scoped CORS for Vercel-hosted mini app (keeps global CORS behavior unchanged for existing clients).
	r.POST("/rider/request/destination", corsForMiniApp("https://custom-location.vercel.app"), handlers.RiderSetDestination(db, cfg, riderBot, fareSvc))

	// Legal: active documents + accept (active versions only; X-Driver-Id allowed when enabled).
	r.GET("/legal/active", tryDriverID, tryDriverBearer, appUserAuth, handlers.LegalActiveDocuments(db))
	r.POST("/legal/accept", tryDriverID, tryDriverBearer, appUserAuth, handlers.LegalAccept(db))

	r.Static("/webapp", "./webapp")

	// Admin HTTP API (dashboard, drivers, payments, driver verification). Additive; does not change trip/dispatch/location logic.
	// Mounted only when ADMIN_API_TOKEN is set (never open).
	adminDriverRepo := repositories.NewAdminDriverRepository(db)
	paymentRepo := repositories.NewPaymentRepository(db)
	tripStatsRepo := repositories.NewTripStatsRepository(db)
	adminSvc := services.NewAdminService(db, adminDriverRepo, paymentRepo, tripStatsRepo)
	adminHandlers := handlers.NewAdminHandlers(adminSvc, matchSvc, driverBot, db)
	adminHandlers.Register(r, cfg.AdminAPIToken)
	handlers.RegisterAdminLegalRoutes(r, db, cfg.AdminAPIToken)
	return r
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.Request.Header.Get("Origin"))
		// Echo a concrete Origin when the browser sent one. Flutter web (and any
		// fetch with credentials) rejects Access-Control-Allow-Origin: * — that
		// shows up in DevTools as failed preflight/fetch with "Server javob bermadi".
		// Local debug origins (127.0.0.1 / localhost) are the common case.
		if origin != "" {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Vary", "Origin")
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		}
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		// Reflect requested headers when present so Flutter/BrowserClient preflights
		// are not rejected for an unexpected header (e.g. X-Requested-With). Fall
		// back to the known app header set otherwise.
		allowHeaders := strings.TrimSpace(c.Request.Header.Get("Access-Control-Request-Headers"))
		if allowHeaders == "" {
			allowHeaders = "Accept, Content-Type, Authorization, X-Telegram-Init-Data, X-Driver-Id, X-Driver-Session, X-Requested-With"
		}
		c.Writer.Header().Set("Access-Control-Allow-Headers", allowHeaders)
		// Without this a browser client cannot read the 429 rate-limit countdown.
		c.Writer.Header().Set("Access-Control-Expose-Headers", "Retry-After")
		c.Writer.Header().Set("Access-Control-Max-Age", "86400")
		// Chrome Private Network Access preflight (localhost page → public API is
		// fine; public → private needs this). Harmless to advertise when asked.
		if strings.EqualFold(c.Request.Header.Get("Access-Control-Request-Private-Network"), "true") {
			c.Writer.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

// corsForMiniApp sets strict CORS only when the Origin matches allowedOrigin.
// It does not interfere with global CORS for other clients.
func corsForMiniApp(allowedOrigin string) gin.HandlerFunc {
	allowedOrigin = strings.TrimSpace(allowedOrigin)
	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.Request.Header.Get("Origin"))
		if allowedOrigin != "" && origin == allowedOrigin {
			c.Header("Access-Control-Allow-Origin", allowedOrigin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Methods", "POST, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type")
			if c.Request.Method == "OPTIONS" {
				c.AbortWithStatus(204)
				return
			}
		}
		c.Next()
	}
}

// healthCacheTTL bounds how often /health actually touches the database.
const healthCacheTTL = 5 * time.Second

// healthDBPingTimeout is the hard cap for the SELECT 1 probe. Kept short so a
// Flutter web reachability check (often ~5s client timeout) still gets a
// response, and so Render's healthCheckPath does not wait on a wedged pool.
const healthDBPingTimeout = 2 * time.Second

// healthHandler reports whether the service can reach its database.
//
// Concurrency notes (Flutter web + Render hit this path together):
//   - The DB ping uses a detached timeout context, not the request context.
//     Otherwise a browser that cancels at 5s would mark the cached probe as
//     failed and poison Render/keepalive with 503 for the whole cache TTL.
//   - The mutex is not held across the DB round-trip, so one slow Turso ping
//     cannot block every concurrent /health waiter behind it.
func healthHandler(db *sql.DB) gin.HandlerFunc {
	var (
		mu        sync.Mutex
		checkedAt time.Time
		lastErr   error
	)
	return func(c *gin.Context) {
		mu.Lock()
		needPing := time.Since(checkedAt) > healthCacheTTL
		err := lastErr
		mu.Unlock()

		if needPing {
			ctx, cancel := context.WithTimeout(context.Background(), healthDBPingTimeout)
			var one int
			pingErr := db.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
			cancel()

			mu.Lock()
			// Another goroutine may have refreshed while we were in flight; only
			// publish if we are still the stale writer, or always publish a
			// successful ping so a good result wins races.
			if pingErr == nil || time.Since(checkedAt) > healthCacheTTL {
				lastErr = pingErr
				checkedAt = time.Now()
			}
			err = lastErr
			mu.Unlock()
		}

		if err != nil {
			log.Printf("health: database unreachable: %v", err)
			c.Data(http.StatusServiceUnavailable, "text/plain; charset=utf-8", []byte("DB UNAVAILABLE"))
			return
		}
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte("OK"))
	}
}
