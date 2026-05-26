// Package migrations holds Go-based goose migrations alongside the SQL ones.
//
// 054_places_add_updated_at exists for legacy databases where an older revision
// of 053 created `places` without `updated_at`. On fresh installs 053 already
// adds the column, so this migration must be a no-op there. The SQL form of
// this migration unconditionally ran `ALTER TABLE places ADD COLUMN updated_at`
// and broke fresh deploys with `duplicate column name: updated_at`.
package migrations

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

func init() {
	goose.AddMigrationContext(upPlacesAddUpdatedAt, downPlacesAddUpdatedAt)
}

func upPlacesAddUpdatedAt(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM pragma_table_info('places') WHERE name = 'updated_at'`,
	).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE places ADD COLUMN updated_at TEXT`); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(
		ctx,
		`UPDATE places SET updated_at = COALESCE(updated_at, created_at, datetime('now')) WHERE updated_at IS NULL`,
	)
	return err
}

func downPlacesAddUpdatedAt(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM pragma_table_info('places') WHERE name = 'updated_at'`,
	).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `ALTER TABLE places DROP COLUMN updated_at`)
	return err
}
