package migrations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
)

func init() {
	goose.AddMigrationContext(upRideReqDropNameEstPrice, downRideReqDropNameEstPrice)
}

// 055 backfills drop_name + estimated_price on ride_requests for any DB that
// applied an older revision of 053 without those columns. On fresh installs
// 053 already creates them, so the original SQL form failed with
// "duplicate column name: drop_name". This Go migration only adds columns
// that are missing.
func upRideReqDropNameEstPrice(ctx context.Context, tx *sql.Tx) error {
	if err := ensureRideRequestsColumn(ctx, tx, "drop_name",
		`ALTER TABLE ride_requests ADD COLUMN drop_name TEXT`); err != nil {
		return err
	}
	if err := ensureRideRequestsColumn(ctx, tx, "estimated_price",
		`ALTER TABLE ride_requests ADD COLUMN estimated_price INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return nil
}

func downRideReqDropNameEstPrice(ctx context.Context, tx *sql.Tx) error {
	if err := dropRideRequestsColumnIfExists(ctx, tx, "drop_name"); err != nil {
		return err
	}
	if err := dropRideRequestsColumnIfExists(ctx, tx, "estimated_price"); err != nil {
		return err
	}
	return nil
}

func ensureRideRequestsColumn(ctx context.Context, tx *sql.Tx, column, alterSQL string) error {
	exists, err := rideRequestsHasColumn(ctx, tx, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.ExecContext(ctx, alterSQL); err != nil {
		return fmt.Errorf("ride_requests add %s: %w", column, err)
	}
	return nil
}

func dropRideRequestsColumnIfExists(ctx context.Context, tx *sql.Tx, column string) error {
	exists, err := rideRequestsHasColumn(ctx, tx, column)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE ride_requests DROP COLUMN %s`, column)); err != nil {
		return fmt.Errorf("ride_requests drop %s: %w", column, err)
	}
	return nil
}

func rideRequestsHasColumn(ctx context.Context, tx *sql.Tx, column string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM pragma_table_info('ride_requests') WHERE name = ?1`,
		column,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("pragma ride_requests %s: %w", column, err)
	}
	return n > 0, nil
}
