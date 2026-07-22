package services

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"
)

// RunDriverAppAutoOfflineWorker marks drivers offline when native app location goes stale.
// Telegram live drivers are not affected. Drivers on an active trip are not affected.
func RunDriverAppAutoOfflineWorker(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		cutoff := time.Now().UTC().Add(-90 * time.Second).Format("2006-01-02 15:04:05")

		// If app schema isn't present yet, do nothing (startup repair should add it).
		// App pings also refresh live_location_active/last_live_location_at, so when an app-only
		// driver goes offline we must clear the stale live flag too; otherwise it stays 1 forever.
		_, err := db.ExecContext(ctx, `
			UPDATE drivers
			SET is_active = 0, app_location_active = 0,
			    live_location_active = CASE
					WHEN last_live_location_at IS NULL OR last_live_location_at < ?1 THEN 0
					ELSE live_location_active
				END
			WHERE COALESCE(is_active, 0) = 1
			  AND COALESCE(app_location_active, 0) = 1
			  AND (app_last_seen_at IS NULL OR app_last_seen_at < ?1)
			  AND NOT EXISTS (
					SELECT 1 FROM trips t
					WHERE t.driver_user_id = drivers.user_id
					  AND t.status IN ('WAITING','ARRIVED','STARTED')
			  )
			  AND NOT (
					COALESCE(live_location_active, 0) = 1
					AND last_live_location_at IS NOT NULL
					AND last_live_location_at >= ?1
			  )`,
			cutoff)
		if err != nil {
			// Be quiet on schema drift: no such column until repair/migrations apply.
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column") {
				continue
			}
			log.Printf("driver_app_auto_offline: update failed: %v", err)
		}
	}
}

