package abuse

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Driver cancellation thresholds over a rolling 24h window.
//
// Deliberately more lenient than the rider guard: a driver has legitimate
// reasons to cancel (breakdown, the rider is not at the pickup point), and a
// driver who is cut off entirely earns nothing, which costs the platform supply
// as well as punishing the driver. So this is a dispatch cooldown, not a block,
// and it starts later than the rider's first-cancellation warning.
const (
	driverCooldownMinCount = 3
	driverCooldownStep     = 15 * time.Minute
	driverCooldownMax      = 4 * time.Hour
)

// DriverPenaltyState is the current cancellation standing for a driver.
type DriverPenaltyState struct {
	Count24h        int
	CooldownUntil   *time.Time
	EscalationLevel int
}

// RecordDriverCancelEvent records a driver cancellation and applies a dispatch
// cooldown once the driver passes the threshold. Callers should treat errors as
// non-fatal: accountability must never block the cancellation itself.
func RecordDriverCancelEvent(ctx context.Context, db *sql.DB, driverUserID int64, tripID string, now time.Time) error {
	if db == nil || driverUserID == 0 || tripID == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO driver_cancel_events (driver_user_id, trip_id, created_at) VALUES (?1, ?2, ?3)`,
		driverUserID, tripID, formatAbuseTime(now)); err != nil {
		return ignoreMissingTable(err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM driver_cancel_events WHERE driver_user_id = ?1 AND created_at >= ?2`,
		driverUserID, formatAbuseTime(now.Add(-24*time.Hour))).Scan(&count); err != nil {
		return ignoreMissingTable(err)
	}
	if count < driverCooldownMinCount {
		return nil
	}

	// Each cancellation past the threshold lengthens the cooldown.
	level := count - driverCooldownMinCount + 1
	cooldown := time.Duration(level) * driverCooldownStep
	if cooldown > driverCooldownMax {
		cooldown = driverCooldownMax
	}
	until := now.Add(cooldown)

	_, err := db.ExecContext(ctx, `
		INSERT INTO driver_cancel_state (driver_user_id, cooldown_until, escalation_level, updated_at)
		VALUES (?1, ?2, ?3, ?4)
		ON CONFLICT(driver_user_id) DO UPDATE SET
			cooldown_until = CASE
				WHEN driver_cancel_state.cooldown_until IS NULL OR driver_cancel_state.cooldown_until < excluded.cooldown_until
				THEN excluded.cooldown_until ELSE driver_cancel_state.cooldown_until END,
			escalation_level = excluded.escalation_level,
			updated_at = excluded.updated_at`,
		driverUserID, formatAbuseTime(until), level, formatAbuseTime(now))
	return ignoreMissingTable(err)
}

// CheckDriverCooldown returns the active cooldown for a driver, or nil when they
// are eligible for dispatch.
func CheckDriverCooldown(ctx context.Context, db *sql.DB, driverUserID int64, now time.Time) (*DriverPenaltyState, error) {
	if db == nil || driverUserID == 0 {
		return nil, nil
	}
	var until sql.NullString
	var level int
	err := db.QueryRowContext(ctx,
		`SELECT cooldown_until, COALESCE(escalation_level, 0) FROM driver_cancel_state WHERE driver_user_id = ?1`,
		driverUserID).Scan(&until, &level)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, ignoreMissingTable(err)
	}
	if !until.Valid || strings.TrimSpace(until.String) == "" {
		return nil, nil
	}
	t, perr := parseTime(until.String)
	if perr != nil || !t.After(now) {
		return nil, nil
	}
	return &DriverPenaltyState{CooldownUntil: &t, EscalationLevel: level}, nil
}

func formatAbuseTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

// ignoreMissingTable swallows "no such table" so a database that has not run the
// guard migration yet degrades to no enforcement instead of failing cancels.
func ignoreMissingTable(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return nil
	}
	return err
}
