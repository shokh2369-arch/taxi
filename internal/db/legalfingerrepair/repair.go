// Package legalfingerrepair adds drivers.legal_terms_prompt_fingerprint when the column is missing
// (e.g. goose version advanced without applying migration 036 on this database).
package legalfingerrepair

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// Ensure adds legal_terms_prompt_fingerprint to drivers if absent.
func Ensure(ctx context.Context, db *sql.DB) error {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('drivers') WHERE name = 'legal_terms_prompt_fingerprint'`).Scan(&n)
	if err != nil {
		return fmt.Errorf("legalfingerrepair: pragma drivers: %w", err)
	}
	if n > 0 {
		return nil
	}
	log.Printf("legalfingerrepair: adding drivers.legal_terms_prompt_fingerprint (migration 036 alignment)")
	if _, err := db.ExecContext(ctx, `ALTER TABLE drivers ADD COLUMN legal_terms_prompt_fingerprint TEXT`); err != nil {
		return fmt.Errorf("legalfingerrepair: add column: %w", err)
	}
	return nil
}
