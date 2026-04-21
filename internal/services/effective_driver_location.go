package services

import (
	"database/sql"
	"time"
)

// EffectiveDriverLocation is the minimal field set required to pick which driver coordinates to use.
// It is intentionally additive: Telegram fields remain the fallback and are not modified.
type EffectiveDriverLocation struct {
	AppLat            sql.NullFloat64
	AppLng            sql.NullFloat64
	AppLastSeenAt     sql.NullString
	AppLocationActive sql.NullInt64

	LastLat sql.NullFloat64
	LastLng sql.NullFloat64
}

const effectiveAppFreshnessSeconds = 90

func parseUTCDateTime(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
}

func appLocationFreshNow(d EffectiveDriverLocation, now time.Time) bool {
	if !d.AppLocationActive.Valid || d.AppLocationActive.Int64 != 1 {
		return false
	}
	if !d.AppLat.Valid || !d.AppLng.Valid {
		return false
	}
	if !d.AppLastSeenAt.Valid || d.AppLastSeenAt.String == "" {
		return false
	}
	t, err := parseUTCDateTime(d.AppLastSeenAt.String)
	if err != nil {
		return false
	}
	return t.After(now.UTC().Add(-effectiveAppFreshnessSeconds * time.Second))
}

// GetEffectiveDriverLocation returns the coordinates that should be used for read-only calculations.
// If the native app location is active and fresh, it wins; otherwise Telegram/legacy last_lat/last_lng are used.
func GetEffectiveDriverLocation(driver EffectiveDriverLocation) (lat, lng float64) {
	if appLocationFreshNow(driver, time.Now()) {
		return driver.AppLat.Float64, driver.AppLng.Float64
	}
	if driver.LastLat.Valid && driver.LastLng.Valid {
		return driver.LastLat.Float64, driver.LastLng.Float64
	}
	return 0, 0
}

// GetEffectiveDriverLocationSource returns "APP" when native app location is used, otherwise "TELEGRAM".
func GetEffectiveDriverLocationSource(driver EffectiveDriverLocation) string {
	if appLocationFreshNow(driver, time.Now()) {
		return "APP"
	}
	return "TELEGRAM"
}

