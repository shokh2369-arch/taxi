package services

import (
	"context"
	"database/sql"

	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
)

// driverReceivingOrdersViaApp is true when the native app is actively sharing location
// (same freshness window as dispatch: app_location_active and recent app_last_seen_at).
func driverReceivingOrdersViaApp(ctx context.Context, db *sql.DB, driverUserID int64) bool {
	if db == nil || driverUserID == 0 {
		return false
	}
	var appLast sql.NullString
	var appActive int
	err := db.QueryRowContext(ctx, `
		SELECT app_last_seen_at, COALESCE(app_location_active, 0)
		FROM drivers WHERE user_id = ?1`, driverUserID).Scan(&appLast, &appActive)
	if err != nil {
		return false
	}
	return appActive == 1 && isFreshWithin(appLast, driverLocationFreshnessSeconds)
}

// ShouldSkipDriverTripTelegramNotify returns true when trip-status Telegram messages to the driver
// should be suppressed because the driver is on the native app (notifications are shown in-app).
func ShouldSkipDriverTripTelegramNotify(ctx context.Context, db *sql.DB, driverUserID int64) bool {
	if auth.SkipDriverTelegramNotify(ctx) {
		u := auth.UserFromContext(ctx)
		// Driver HTTP routes often omit auth.User on context; nil means driver-originated HTTP.
		if u == nil || u.Role == domain.RoleDriver {
			return true
		}
	}
	return driverReceivingOrdersViaApp(ctx, db, driverUserID)
}
