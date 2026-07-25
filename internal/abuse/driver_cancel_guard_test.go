package abuse

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupDriverGuardDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:driver_guard_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`CREATE TABLE driver_cancel_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			driver_user_id INTEGER NOT NULL,
			trip_id TEXT NOT NULL,
			created_at TEXT NOT NULL)`,
		`CREATE TABLE driver_cancel_state (
			driver_user_id INTEGER PRIMARY KEY,
			cooldown_until TEXT,
			escalation_level INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Occasional cancellations are legitimate (breakdown, rider no-show) and must not
// cost a driver their income.
func TestDriverCancelGuard_BelowThresholdNoCooldown(t *testing.T) {
	db := setupDriverGuardDB(t)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < driverCooldownMinCount-1; i++ {
		if err := RecordDriverCancelEvent(ctx, db, 7, "trip", now); err != nil {
			t.Fatal(err)
		}
	}
	state, err := CheckDriverCooldown(ctx, db, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("driver should not be on cooldown below the threshold, got %+v", state)
	}
}

func TestDriverCancelGuard_CooldownAppliesAndEscalates(t *testing.T) {
	db := setupDriverGuardDB(t)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < driverCooldownMinCount; i++ {
		if err := RecordDriverCancelEvent(ctx, db, 7, "trip", now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := CheckDriverCooldown(ctx, db, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.CooldownUntil == nil {
		t.Fatal("expected a cooldown once the threshold is reached")
	}
	firstUntil := *first.CooldownUntil

	if err := RecordDriverCancelEvent(ctx, db, 7, "trip", now); err != nil {
		t.Fatal(err)
	}
	second, err := CheckDriverCooldown(ctx, db, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.CooldownUntil == nil {
		t.Fatal("expected a cooldown to remain")
	}
	if !second.CooldownUntil.After(firstUntil) {
		t.Errorf("cooldown should lengthen with each further cancellation: %s then %s", firstUntil, *second.CooldownUntil)
	}
	if second.CooldownUntil.Sub(now) > driverCooldownMax {
		t.Errorf("cooldown %s exceeds the cap %s", second.CooldownUntil.Sub(now), driverCooldownMax)
	}
}

// Old cancellations fall out of the 24h window.
func TestDriverCancelGuard_WindowIsRolling(t *testing.T) {
	db := setupDriverGuardDB(t)
	ctx := context.Background()
	now := time.Now()

	old := now.Add(-48 * time.Hour)
	for i := 0; i < driverCooldownMinCount; i++ {
		if err := RecordDriverCancelEvent(ctx, db, 7, "trip", old); err != nil {
			t.Fatal(err)
		}
	}
	// One fresh cancellation: the stale ones must not count toward the threshold.
	if err := RecordDriverCancelEvent(ctx, db, 7, "trip", now); err != nil {
		t.Fatal(err)
	}
	state, err := CheckDriverCooldown(ctx, db, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("cancellations older than 24h must not trigger a cooldown, got %+v", state)
	}
}

func TestDriverCancelGuard_ExpiredCooldownIsClear(t *testing.T) {
	db := setupDriverGuardDB(t)
	ctx := context.Background()
	past := time.Now().Add(-2 * time.Hour)

	for i := 0; i < driverCooldownMinCount; i++ {
		if err := RecordDriverCancelEvent(ctx, db, 7, "trip", past); err != nil {
			t.Fatal(err)
		}
	}
	state, err := CheckDriverCooldown(ctx, db, 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("an elapsed cooldown must not keep a driver out of dispatch, got %+v", state)
	}
}

// A database that has not run migration 066 must degrade to no enforcement
// rather than failing the cancellation itself.
func TestDriverCancelGuard_ToleratesMissingTables(t *testing.T) {
	db, err := sql.Open("sqlite", "file:driver_guard_missing?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if err := RecordDriverCancelEvent(ctx, db, 7, "trip", time.Now()); err != nil {
		t.Errorf("recording must not fail when the guard tables are absent: %v", err)
	}
	state, err := CheckDriverCooldown(ctx, db, 7, time.Now())
	if err != nil {
		t.Errorf("checking must not fail when the guard tables are absent: %v", err)
	}
	if state != nil {
		t.Errorf("no enforcement is possible without the tables, got %+v", state)
	}
}
