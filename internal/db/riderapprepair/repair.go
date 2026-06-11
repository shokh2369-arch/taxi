// Package riderapprepair adds optional users rider-app activity columns when missing.
package riderapprepair

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

func ensureColumn(ctx context.Context, db *sql.DB, columnName, alterSQL string) error {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = ?1`, columnName).Scan(&n)
	if err != nil {
		return fmt.Errorf("riderapprepair: pragma users %s: %w", columnName, err)
	}
	if n > 0 {
		return nil
	}
	log.Printf("riderapprepair: adding users.%s", columnName)
	if _, err := db.ExecContext(ctx, alterSQL); err != nil {
		return fmt.Errorf("riderapprepair: add %s: %w", columnName, err)
	}
	return nil
}

// Ensure adds rider native-app last-seen column to users if absent.
func Ensure(ctx context.Context, db *sql.DB) error {
	return ensureColumn(ctx, db, "rider_app_last_seen_at", `ALTER TABLE users ADD COLUMN rider_app_last_seen_at TEXT`)
}
