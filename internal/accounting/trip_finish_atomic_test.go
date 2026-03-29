package accounting

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/models"

	_ "modernc.org/sqlite"
)

func setupFinishAtomicDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:finish_atomic?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		telegram_id INTEGER NOT NULL DEFAULT 0,
		referral_code TEXT,
		referred_by TEXT,
		referral_stage2_reward_paid INTEGER NOT NULL DEFAULT 0
	);`)
	exec(`CREATE TABLE drivers (
		user_id INTEGER PRIMARY KEY,
		promo_balance INTEGER NOT NULL DEFAULT 0,
		cash_balance INTEGER NOT NULL DEFAULT 0,
		balance INTEGER NOT NULL DEFAULT 0,
		verification_status TEXT NOT NULL DEFAULT 'approved',
		signup_bonus_paid INTEGER NOT NULL DEFAULT 0,
		is_active INTEGER NOT NULL DEFAULT 1
	);`)
	exec(`CREATE TABLE trips (
		id TEXT PRIMARY KEY,
		driver_user_id INTEGER NOT NULL,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		finished_at TEXT,
		fare_amount INTEGER,
		rider_bonus_used INTEGER NOT NULL DEFAULT 0
	);`)
	exec(`CREATE TABLE driver_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		driver_id INTEGER NOT NULL,
		bucket TEXT NOT NULL,
		entry_type TEXT NOT NULL,
		amount INTEGER NOT NULL,
		reference_type TEXT,
		reference_id TEXT,
		note TEXT,
		metadata_json TEXT,
		expires_at TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);`)
	exec(`CREATE UNIQUE INDEX idx_driver_ledger_driver_ref_type_id ON driver_ledger(driver_id, reference_type, reference_id);`)
	exec(`CREATE TABLE payments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		driver_id INTEGER NOT NULL,
		amount INTEGER NOT NULL,
		type TEXT NOT NULL,
		note TEXT,
		trip_id TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);`)
	return db
}

func TestFinishTripAtomic_PromoFailureRollsBackTrip(t *testing.T) {
	db := setupFinishAtomicDB(t)
	defer db.Close()
	ctx := context.Background()
	_, _ = db.Exec(`INSERT INTO users (id) VALUES (1)`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, promo_balance, cash_balance, balance) VALUES (1, 5000, 50000, 55000)`)
	_, _ = db.Exec(`INSERT INTO trips (id, driver_user_id, rider_user_id, status) VALUES ('t1', 1, 2, ?1)`, domain.TripStatusStarted)

	testSimulatePromoGrantError = errors.New("simulated promo failure")
	defer func() { testSimulatePromoGrantError = nil }()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE trips SET status = ?1, finished_at = datetime('now'), fare_amount = ?2, rider_bonus_used = ?3
		WHERE id = ?4 AND driver_user_id = ?5 AND status = ?6`,
		domain.TripStatusFinished, int64(10000), int64(0), "t1", int64(1), domain.TripStatusStarted)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Fatalf("update trip want 1 row got %d", n)
	}

	wantPromo := testSimulatePromoGrantError
	_, _, _, err = ExecuteTripFinishEffectsInTx(ctx, tx, db, nil, 1, "t1", 10000, 500, 5, false)
	if err == nil || !errors.Is(err, wantPromo) {
		t.Fatalf("want promo error, got %v", err)
	}
	_ = tx.Rollback()

	var st string
	_ = db.QueryRow(`SELECT status FROM trips WHERE id='t1'`).Scan(&st)
	if st != domain.TripStatusStarted {
		t.Fatalf("trip should stay STARTED after rollback, got %q", st)
	}
}

func TestFinishTripAtomic_ReferralFailureRollsBackTrip(t *testing.T) {
	db := setupFinishAtomicDB(t)
	defer db.Close()
	ctx := context.Background()
	_, _ = db.Exec(`INSERT INTO users (id) VALUES (1)`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, promo_balance, cash_balance, balance) VALUES (1, 5000, 50000, 55000)`)
	_, _ = db.Exec(`INSERT INTO trips (id, driver_user_id, rider_user_id, status) VALUES ('t1', 1, 2, ?1)`, domain.TripStatusStarted)

	testSimulateReferralGrantError = errors.New("simulated referral failure")
	defer func() { testSimulateReferralGrantError = nil }()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		UPDATE trips SET status = ?1, finished_at = datetime('now'), fare_amount = ?2, rider_bonus_used = ?3
		WHERE id = ?4 AND driver_user_id = ?5 AND status = ?6`,
		domain.TripStatusFinished, int64(10000), int64(0), "t1", int64(1), domain.TripStatusStarted)
	if err != nil {
		t.Fatal(err)
	}

	wantRef := testSimulateReferralGrantError
	_, _, _, err = ExecuteTripFinishEffectsInTx(ctx, tx, db, nil, 1, "t1", 10000, 500, 5, false)
	if err == nil || !errors.Is(err, wantRef) {
		t.Fatalf("want referral error, got %v", err)
	}
	_ = tx.Rollback()

	var st string
	_ = db.QueryRow(`SELECT status FROM trips WHERE id='t1'`).Scan(&st)
	if st != domain.TripStatusStarted {
		t.Fatalf("trip should stay STARTED, got %q", st)
	}
}

func TestFinishTripAtomic_CommissionFailureRollsBackTrip(t *testing.T) {
	db := setupFinishAtomicDB(t)
	defer db.Close()
	ctx := context.Background()
	_, _ = db.Exec(`INSERT INTO users (id) VALUES (1)`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, promo_balance, cash_balance, balance) VALUES (1, 5000, 50000, 55000)`)
	_, _ = db.Exec(`INSERT INTO trips (id, driver_user_id, rider_user_id, status) VALUES ('t1', 1, 2, ?1)`, domain.TripStatusStarted)

	testSimulateCommissionError = errors.New("simulated commission failure")
	defer func() { testSimulateCommissionError = nil }()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		UPDATE trips SET status = ?1, finished_at = datetime('now'), fare_amount = ?2, rider_bonus_used = ?3
		WHERE id = ?4 AND driver_user_id = ?5 AND status = ?6`,
		domain.TripStatusFinished, int64(10000), int64(0), "t1", int64(1), domain.TripStatusStarted)
	if err != nil {
		t.Fatal(err)
	}

	wantComm := testSimulateCommissionError
	_, _, _, err = ExecuteTripFinishEffectsInTx(ctx, tx, db, nil, 1, "t1", 10000, 500, 5, false)
	if err == nil || !errors.Is(err, wantComm) {
		t.Fatalf("want commission error, got %v", err)
	}
	_ = tx.Rollback()

	var st string
	_ = db.QueryRow(`SELECT status FROM trips WHERE id='t1'`).Scan(&st)
	if st != domain.TripStatusStarted {
		t.Fatalf("trip should stay STARTED, got %q", st)
	}
}

func TestFinishTripAtomic_SuccessCommitsAll(t *testing.T) {
	db := setupFinishAtomicDB(t)
	defer db.Close()
	ctx := context.Background()
	_, _ = db.Exec(`INSERT INTO users (id) VALUES (1)`)
	_, _ = db.Exec(`INSERT INTO drivers (user_id, promo_balance, cash_balance, balance) VALUES (1, 5000, 50000, 55000)`)
	_, _ = db.Exec(`INSERT INTO trips (id, driver_user_id, rider_user_id, status) VALUES ('t1', 1, 2, ?1)`, domain.TripStatusStarted)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE trips SET status = ?1, finished_at = datetime('now'), fare_amount = ?2, rider_bonus_used = ?3
		WHERE id = ?4 AND driver_user_id = ?5 AND status = ?6`,
		domain.TripStatusFinished, int64(10000), int64(0), "t1", int64(1), domain.TripStatusStarted)
	if err != nil {
		t.Fatal(err)
	}
	g, n, ref, err := ExecuteTripFinishEffectsInTx(ctx, tx, db, nil, 1, "t1", 10000, 500, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if !g || n != 1 || ref.Granted {
		t.Fatalf("first trip: g=%v n=%v ref=%+v", g, n, ref)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var st string
	_ = db.QueryRow(`SELECT status FROM trips WHERE id='t1'`).Scan(&st)
	if st != domain.TripStatusFinished {
		t.Fatalf("status want FINISHED got %q", st)
	}
	var cnt int
	_ = db.QueryRow(`SELECT COUNT(*) FROM driver_ledger WHERE reference_type='first_3_trip_bonus'`).Scan(&cnt)
	if cnt != 1 {
		t.Fatalf("first3 ledger want 1 got %d", cnt)
	}
	_ = db.QueryRow(`SELECT COUNT(*) FROM driver_ledger WHERE entry_type = ?1`, models.LedgerEntryCommissionAccrued).Scan(&cnt)
	if cnt < 1 {
		t.Fatalf("expected commission ledger rows, got %d", cnt)
	}
	var promo int
	_ = db.QueryRow(`SELECT promo_balance FROM drivers WHERE user_id=1`).Scan(&promo)
	// Started 5000 + 10000 first3 - 500 commission offset from promo = 14500
	if promo != 14500 {
		t.Fatalf("promo balance want 14500 got %d", promo)
	}
}
