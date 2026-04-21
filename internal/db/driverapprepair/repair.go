// Package driverapprepair adds optional drivers app-location columns when missing
// (e.g. goose version advanced without applying migrations on this database).
package driverapprepair

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

func ensureColumn(ctx context.Context, db *sql.DB, columnName, alterSQL string) error {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('drivers') WHERE name = ?1`, columnName).Scan(&n)
	if err != nil {
		return fmt.Errorf("driverapprepair: pragma drivers %s: %w", columnName, err)
	}
	if n > 0 {
		return nil
	}
	log.Printf("driverapprepair: adding drivers.%s", columnName)
	if _, err := db.ExecContext(ctx, alterSQL); err != nil {
		return fmt.Errorf("driverapprepair: add %s: %w", columnName, err)
	}
	return nil
}

// Ensure adds native-app location columns to drivers if absent.
func Ensure(ctx context.Context, db *sql.DB) error {
	if err := ensureColumn(ctx, db, "app_lat", `ALTER TABLE drivers ADD COLUMN app_lat REAL`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "app_lng", `ALTER TABLE drivers ADD COLUMN app_lng REAL`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "app_last_seen_at", `ALTER TABLE drivers ADD COLUMN app_last_seen_at TEXT`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "app_location_active", `ALTER TABLE drivers ADD COLUMN app_location_active INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return nil
}

