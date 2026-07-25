package services

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Retention defaults. Every one of these tables previously grew without bound:
// trip_locations gains a row per GPS ping and is only ever read as
// "most recent point for this trip", request_notifications is pruned on accept
// and cancel but not on expiry, and expired auth sessions and login codes were
// never swept at all. On a metered remote database that is storage and backup
// weight that nothing reads.
const (
	defaultTripLocationRetentionDays = 90
	defaultNotificationRetentionDays = 30
	defaultAbuseEventRetentionDays   = 30
	retentionSweepInterval           = 6 * time.Hour
	retentionInitialDelay            = 5 * time.Minute
	retentionDeleteBatch             = 5000
)

// RunRetentionWorker periodically deletes rows past their retention window.
//
// Deletes are batched and the worker sleeps between batches: SQLite/libSQL is
// single-writer, so one large DELETE would hold the write lock long enough to
// stall dispatch. Financial and legal records (driver_ledger, payments, trips,
// legal_acceptances) are deliberately never touched.
func RunRetentionWorker(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	// Let the app finish booting before competing for the write lock.
	select {
	case <-ctx.Done():
		return
	case <-time.After(retentionInitialDelay):
	}

	tick := time.NewTicker(retentionSweepInterval)
	defer tick.Stop()
	for {
		runRetentionSweep(ctx, db)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func runRetentionSweep(ctx context.Context, db *sql.DB) {
	tripDays := retentionEnvDays("RETENTION_TRIP_LOCATIONS_DAYS", defaultTripLocationRetentionDays)
	notifDays := retentionEnvDays("RETENTION_REQUEST_NOTIFICATIONS_DAYS", defaultNotificationRetentionDays)
	abuseDays := retentionEnvDays("RETENTION_ABUSE_EVENTS_DAYS", defaultAbuseEventRetentionDays)

	// GPS traces for trips that ended long ago. Kept long enough to answer a
	// dispute or a police request, then dropped.
	if tripDays > 0 {
		sweep(ctx, db, "trip_locations", `
			DELETE FROM trip_locations WHERE rowid IN (
				SELECT tl.rowid FROM trip_locations tl
				JOIN trips t ON t.id = tl.trip_id
				WHERE t.status IN ('FINISHED','CANCELLED','CANCELLED_BY_DRIVER','CANCELLED_BY_RIDER')
				  AND tl.ts < datetime('now', ?1)
				LIMIT ?2)`,
			daysModifier(tripDays))
	}

	// Offers for requests that are no longer live. Accept and cancel already
	// delete these; expiry only flipped the status, so those rows accumulated.
	if notifDays > 0 {
		sweep(ctx, db, "request_notifications", `
			DELETE FROM request_notifications WHERE rowid IN (
				SELECT n.rowid FROM request_notifications n
				JOIN ride_requests r ON r.id = n.request_id
				WHERE r.status IN ('CANCELLED','EXPIRED','ASSIGNED')
				  AND n.created_at < datetime('now', ?1)
				LIMIT ?2)`,
			daysModifier(notifDays))
	}

	// The cancel guard only ever queries a rolling 24h window.
	if abuseDays > 0 {
		sweep(ctx, db, "rider_abuse_events", `
			DELETE FROM rider_abuse_events WHERE rowid IN (
				SELECT rowid FROM rider_abuse_events
				WHERE created_at < datetime('now', ?1) LIMIT ?2)`,
			daysModifier(abuseDays))
	}

	// Consumed or expired credentials have no value once past their TTL.
	sweepNoArg(ctx, db, "rider_login_codes", `
		DELETE FROM rider_login_codes WHERE rowid IN (
			SELECT rowid FROM rider_login_codes
			WHERE consumed = 1 OR expires_at < strftime('%s','now') LIMIT ?1)`)
	sweepNoArg(ctx, db, "driver_login_codes", `
		DELETE FROM driver_login_codes WHERE rowid IN (
			SELECT rowid FROM driver_login_codes
			WHERE used = 1 OR expires_at < datetime('now') LIMIT ?1)`)
	sweepNoArg(ctx, db, "rider_auth_sessions", `
		DELETE FROM rider_auth_sessions WHERE rowid IN (
			SELECT rowid FROM rider_auth_sessions
			WHERE revoked = 1 OR refresh_expires_at < strftime('%s','now') LIMIT ?1)`)
	sweepNoArg(ctx, db, "driver_auth_sessions", `
		DELETE FROM driver_auth_sessions WHERE rowid IN (
			SELECT rowid FROM driver_auth_sessions
			WHERE revoked = 1 OR expires_at < strftime('%s','now') LIMIT ?1)`)
}

// sweep deletes in bounded batches until a batch comes back short, yielding the
// write lock between batches.
func sweep(ctx context.Context, db *sql.DB, table, query string, modifier string) {
	total := int64(0)
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := db.ExecContext(ctx, query, modifier, retentionDeleteBatch)
		if err != nil {
			// A table missing on an un-migrated database is not worth alarming about.
			if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
				log.Printf("retention: %s: %v", table, err)
			}
			return
		}
		n, _ := res.RowsAffected()
		total += n
		if n < retentionDeleteBatch {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if total > 0 {
		log.Printf("retention: deleted %d row(s) from %s", total, table)
	}
}

func sweepNoArg(ctx context.Context, db *sql.DB, table, query string) {
	total := int64(0)
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := db.ExecContext(ctx, query, retentionDeleteBatch)
		if err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
				log.Printf("retention: %s: %v", table, err)
			}
			return
		}
		n, _ := res.RowsAffected()
		total += n
		if n < retentionDeleteBatch {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if total > 0 {
		log.Printf("retention: deleted %d row(s) from %s", total, table)
	}
}

func daysModifier(days int) string { return "-" + strconv.Itoa(days) + " days" }

// retentionEnvDays returns the configured retention in days. 0 disables a sweep.
func retentionEnvDays(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("retention: %s=%q invalid; using %d", key, raw, def)
		return def
	}
	return n
}
