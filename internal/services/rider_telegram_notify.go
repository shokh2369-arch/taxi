package services

import (
	"context"
	"database/sql"

	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
)

// riderUsingNativeApp is true when the rider recently used the native app API
// (users.rider_app_last_seen_at within the same freshness window as driver dispatch).
func riderUsingNativeApp(ctx context.Context, db *sql.DB, riderUserID int64) bool {
	if db == nil || riderUserID == 0 {
		return false
	}
	var lastSeen sql.NullString
	err := db.QueryRowContext(ctx, `SELECT rider_app_last_seen_at FROM users WHERE id = ?1`, riderUserID).Scan(&lastSeen)
	if err != nil {
		return false
	}
	return isFreshWithin(lastSeen, driverLocationFreshnessSeconds)
}

// ShouldSkipRiderTripTelegramNotify returns true when trip-status Telegram messages to the rider
// should be suppressed because the rider is on the native app (notifications are shown in-app).
func ShouldSkipRiderTripTelegramNotify(ctx context.Context, db *sql.DB, riderUserID int64) bool {
	if auth.SkipRiderTelegramNotify(ctx) {
		u := auth.UserFromContext(ctx)
		if u != nil && u.Role == domain.RoleRider {
			return true
		}
	}
	return riderUsingNativeApp(ctx, db, riderUserID)
}
