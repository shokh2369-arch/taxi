package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds application configuration loaded from environment.
type Config struct {
	RiderBotToken                string
	DriverBotToken               string
	DatabaseURL                  string
	CloudinaryCloudName          string
	CloudinaryAPIKey             string
	CloudinaryAPISecret          string
	StartingFee                  int // Base fare when trip starts (so'm)
	PricePerKm                   int // Per-km rate (so'm)
	MatchRadiusKm                float64
	ExpandedRadiusKm             float64 // Radius after expansion if no driver (e.g. 4)
	RadiusExpansionMinutes       int     // Minutes before expanding radius
	RequestExpiresSeconds        int
	DriverSeenSeconds            int
	StartReminderSeconds         int
	WebAppURL                    string // Base URL for Telegram Mini App / driver map (e.g. https://example.com/webapp)
	RiderMapURL                  string // Full URL to rider map HTML (e.g. https://example.com/webapp/rider-map.html); if empty, derived as WebAppURL + "/rider-map.html"
	APIAddr                      string // HTTP API address for driver location and trip (e.g. :8080)
	EnableDriverIDHeader         bool   // Default false; set ENABLE_DRIVER_ID_HEADER=true (or 1/yes/on) to allow X-Driver-Id (local/dev only)
	DriverAuthDebug              bool   // If true, log driver header path flags (never log header value or ids); env DRIVER_AUTH_DEBUG
	EnableDriverHTTPLiveLocation bool   // If true, POST /driver/location refreshes last_live_location_at / live_location_active and may mark driver online (same signals dispatch uses for Telegram live). Default on; set ENABLE_DRIVER_HTTP_LIVE_LOCATION=false for Telegram-only HTTP pings (Mini App map without treating HTTP as live).
	AdminID                      int64  // Telegram user ID of the admin (only this user can use admin bot fare menu)
	AdminBotToken                string // Telegram bot token for admin bot (optional; if empty, admin bot is not started)
	AdminAPIToken                string // Required to mount /admin HTTP API; Bearer token for dashboard. If empty, admin routes are not mounted.
	InfiniteDriverBalance        bool   // If true, dispatch ignores balance and no commission is deducted (temporary launch mode). Default false.
	CommissionPercent            int    // Commission percentage on fare when InfiniteDriverBalance is false (e.g. 5 or 10)
	DispatchDebug                bool   // If true, emit verbose dispatch/grid debug logs
	// WSAllowedOrigins: origins allowed for WebSocket CheckOrigin (from WS_ALLOWED_ORIGINS). Empty → derived from WebAppURL.
	WSAllowedOrigins []string
	// Dispatch tuning: priority queue (one driver at a time, then next after timeout)
	DispatchWaitSeconds         int // Seconds to wait for a driver batch to accept before trying next (e.g. 10)
	DispatchDriverCooldownSec   int // Cooldown before sending another request to the same driver (e.g. 5–10)
	DispatchOfferVisibleSeconds int // Per-driver app queue visibility after offer created_at (e.g. 90); offers also end on accept/cancel/expired
	// PickupStartMaxMeters: driver must be within this distance of pickup to start from WAITING (or to mark ARRIVED).
	PickupStartMaxMeters int
}

// Load reads .env (if present) and builds Config from env with defaults.
func Load() (*Config, error) {
	_ = godotenv.Load()

	startingFee, _ := strconv.Atoi(getEnv("STARTING_FEE", "4000"))
	pricePerKm, _ := strconv.Atoi(getEnv("PRICE_PER_KM", "1500"))
	matchRadiusKm, _ := strconv.ParseFloat(getEnv("MATCH_RADIUS_KM", "3"), 64)
	expandedRadiusKm, _ := strconv.ParseFloat(getEnv("EXPANDED_RADIUS_KM", "4"), 64)
	radiusExpansionMin, _ := strconv.Atoi(getEnv("RADIUS_EXPANSION_MINUTES", "5"))
	requestExpires, _ := strconv.Atoi(getEnv("REQUEST_EXPIRES_SECONDS", "120")) // 2 min TTL: request no longer sent after this
	driverSeen, _ := strconv.Atoi(getEnv("DRIVER_SEEN_SECONDS", "600"))         // 10 min: orders pushed to drivers seen in last 10 min
	startReminder, _ := strconv.Atoi(getEnv("START_REMINDER_SECONDS", "60"))
	pickupStartMaxM, _ := strconv.Atoi(getEnv("PICKUP_START_MAX_METERS", "100"))

	cfg := &Config{
		RiderBotToken:                getEnv("RIDER_BOT_TOKEN", ""),
		DriverBotToken:               getEnv("DRIVER_BOT_TOKEN", ""),
		DatabaseURL:                  getDatabaseURL(),
		CloudinaryCloudName:          strings.TrimSpace(os.Getenv("CLOUDINARY_CLOUD_NAME")),
		CloudinaryAPIKey:             strings.TrimSpace(os.Getenv("CLOUDINARY_API_KEY")),
		CloudinaryAPISecret:          strings.TrimSpace(os.Getenv("CLOUDINARY_API_SECRET")),
		StartingFee:                  startingFee,
		PricePerKm:                   pricePerKm,
		MatchRadiusKm:                matchRadiusKm,
		ExpandedRadiusKm:             expandedRadiusKm,
		RadiusExpansionMinutes:       radiusExpansionMin,
		RequestExpiresSeconds:        requestExpires,
		DriverSeenSeconds:            driverSeen,
		StartReminderSeconds:         startReminder,
		WebAppURL:                    getEnv("WEBAPP_URL", "https://example.com/webapp"),
		RiderMapURL:                  getRiderMapURL(getEnv("WEBAPP_URL", "https://example.com/webapp"), getEnv("RIDER_MAP_URL", "")),
		APIAddr:                      getAPIAddr(),
		EnableDriverIDHeader:         envEnableDriverIDHeader(),
		EnableDriverHTTPLiveLocation: envEnableDriverHTTPLiveLocation(),
		DriverAuthDebug:              getEnv("DRIVER_AUTH_DEBUG", "") == "true" || getEnv("DRIVER_AUTH_DEBUG", "") == "1",
		AdminID:                      getEnvInt64("ADMIN_ID", 0),
		AdminBotToken:                getEnvFirst("ADMIN_BOT_TOKEN", "ADMIN_BOT", ""),
		AdminAPIToken:                strings.TrimSpace(os.Getenv("ADMIN_API_TOKEN")),
		InfiniteDriverBalance:        envTruthy("INFINITE_DRIVER_BALANCE", false),
		CommissionPercent:            getEnvInt("COMMISSION_PERCENT", 5),
		DispatchDebug:                getEnv("DISPATCH_DEBUG", "") == "true" || getEnv("DISPATCH_DEBUG", "") == "1",
		WSAllowedOrigins:             parseWSAllowedOrigins(getEnv("WEBAPP_URL", "https://example.com/webapp")),
		DispatchWaitSeconds:          getEnvInt("DISPATCH_WAIT_SECONDS", 10),
		DispatchDriverCooldownSec:    getEnvInt("DISPATCH_DRIVER_COOLDOWN_SECONDS", 5),
		DispatchOfferVisibleSeconds:  getEnvInt("DISPATCH_OFFER_VISIBLE_SECONDS", 90),
		PickupStartMaxMeters:         pickupStartMaxM,
	}

	if cfg.RiderBotToken == "" {
		return nil, fmt.Errorf("RIDER_BOT_TOKEN is required")
	}
	if cfg.DriverBotToken == "" {
		return nil, fmt.Errorf("DRIVER_BOT_TOKEN is required")
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL or TURSO_DATABASE_URL + TURSO_AUTH_TOKEN required")
	}

	return cfg, nil
}

// envEnableDriverIDHeader is false by default (require Bearer session or Telegram initData).
// Set ENABLE_DRIVER_ID_HEADER to true, 1, yes, or on (case-insensitive) to allow X-Driver-Id for local/dev.
func envEnableDriverIDHeader() bool {
	return envTruthy("ENABLE_DRIVER_ID_HEADER", false)
}

// envTruthy reads a boolean env var. When unset/empty, returns defaultVal.
// True values: true, 1, yes, on. False values: false, 0, no, off (and any other non-empty string → false when defaultVal is true isn't used).
func envTruthy(key string, defaultVal bool) bool {
	s := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if s == "" {
		return defaultVal
	}
	switch s {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// parseWSAllowedOrigins returns origins from WS_ALLOWED_ORIGINS (comma-separated),
// or the scheme+host of webAppURL when the env is empty.
func parseWSAllowedOrigins(webAppURL string) []string {
	raw := strings.TrimSpace(os.Getenv("WS_ALLOWED_ORIGINS"))
	if raw != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, strings.TrimRight(p, "/"))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if o := originFromURL(webAppURL); o != "" {
		return []string{o}
	}
	return nil
}

func originFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Simple parse for http(s)://host[:port]/path without pulling net/url.
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" || rest == "" {
		return ""
	}
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host = rest[:i]
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// envEnableDriverHTTPLiveLocation is true by default so native/Mini App clients that only send POST /driver/location
// are eligible for dispatch like Telegram live-location sharers. Set ENABLE_DRIVER_HTTP_LIVE_LOCATION to false, 0, no, or off
// (case-insensitive) to restore legacy behavior: HTTP location updates grid/last_seen only; Telegram live drives live_location_*.
func envEnableDriverHTTPLiveLocation() bool {
	s := strings.TrimSpace(strings.ToLower(os.Getenv("ENABLE_DRIVER_HTTP_LIVE_LOCATION")))
	switch s {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// getEnvFirst returns the first non-empty env value for the given keys; last argument is the default.
// Example: getEnvFirst("ADMIN_BOT_TOKEN", "ADMIN_BOT", "") tries both vars, then returns "".
func getEnvFirst(keys ...string) string {
	if len(keys) < 2 {
		return ""
	}
	defaultVal := keys[len(keys)-1]
	for i := 0; i < len(keys)-1; i++ {
		if v := os.Getenv(keys[i]); v != "" {
			return v
		}
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	s := os.Getenv(key)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return defaultVal
	}
	return v
}

func getEnvInt(key string, defaultVal int) int {
	s := os.Getenv(key)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

// getRiderMapURL returns RIDER_MAP_URL if set, otherwise derives from webAppURL: same base path + "/rider-map.html".
// Example: webAppURL "https://your-domain.com/webapp" -> "https://your-domain.com/webapp/rider-map.html".
func getRiderMapURL(webAppURL, riderMapURL string) string {
	if riderMapURL != "" {
		return strings.TrimSuffix(riderMapURL, "/")
	}
	base := strings.TrimSuffix(webAppURL, "/")
	if base == "" {
		return ""
	}
	return base + "/rider-map.html"
}

// getAPIAddr returns the HTTP listen address. Uses PORT (e.g. from Railway/Render) if set, else API_ADDR.
func getAPIAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return getEnv("API_ADDR", ":8080")
}

// getDatabaseURL returns the Turso libSQL connection URL.
// Use DATABASE_URL (full libsql://...?authToken=...) or TURSO_DATABASE_URL + TURSO_AUTH_TOKEN.
func getDatabaseURL() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	url := os.Getenv("TURSO_DATABASE_URL")
	token := os.Getenv("TURSO_AUTH_TOKEN")
	if url != "" && token != "" {
		sep := "?"
		if len(url) > 0 && url[len(url)-1] == '?' {
			sep = ""
		} else if strings.Contains(url, "?") {
			sep = "&"
		}
		return url + sep + "authToken=" + token
	}
	return ""
}
