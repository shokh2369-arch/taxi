package migrations

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"strings"

	"github.com/pressly/goose/v3"
)

// 034 rebuilds the legal_* tables, but only when they are actually incompatible.
//
// This was previously plain SQL that ran `DROP TABLE legal_acceptances` on every
// database it was applied to, drifted or not. legal_acceptances is the record of
// who accepted which terms version, when, and from which IP — the only evidence
// the platform holds that a driver agreed to the commission and promo clauses.
// Replaying migrations against a database that already has that data (a restored
// backup with a reset goose_db_version, a branch seeded from production, a fresh
// staging environment) destroyed it and forced everyone to re-accept.
//
// The guard is the same probe internal/db/legalrepair already uses: rebuild only
// when legal_documents is missing (fresh install) or exists without the
// document_type column (the genuinely incompatible shape this migration was
// written for). A healthy schema is left untouched.
//
//go:embed 034_legal_rebuild.sqlbody
var legalRebuildSQL string

func init() {
	goose.AddMigrationContext(upLegalDocumentsSchemaRebuild, downLegalDocumentsSchemaRebuild)
}

func upLegalDocumentsSchemaRebuild(ctx context.Context, tx *sql.Tx) error {
	needed, err := legalRebuildNeeded(ctx, tx)
	if err != nil {
		return err
	}
	if !needed {
		log.Printf("migration 034: legal schema already compatible; skipping rebuild (acceptances preserved)")
		return nil
	}
	for i, stmt := range splitSQLStatementsQuoteAware(legalRebuildSQL) {
		if stmt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			preview := stmt
			if len(preview) > 180 {
				preview = preview[:180] + "..."
			}
			return fmt.Errorf("migration 034 statement %d: %w\n%s", i, err, preview)
		}
	}
	return nil
}

func downLegalDocumentsSchemaRebuild(ctx context.Context, tx *sql.Tx) error {
	// Deliberately a no-op. The original Down dropped the same tables, so rolling
	// back destroyed consent records a second time.
	return nil
}

// legalRebuildNeeded reports whether the destructive rebuild should run.
func legalRebuildNeeded(ctx context.Context, tx *sql.Tx) (bool, error) {
	var tables int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='legal_documents'`).Scan(&tables); err != nil {
		return false, err
	}
	if tables == 0 {
		return true, nil // fresh install: nothing to lose, create the tables
	}
	var hasDocumentType int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('legal_documents') WHERE name='document_type'`).Scan(&hasDocumentType); err != nil {
		return false, err
	}
	// Missing document_type is the incompatible shape this migration exists for.
	return hasDocumentType == 0, nil
}

// splitSQLStatementsQuoteAware splits on semicolons outside single-quoted strings.
// The legal document bodies are long prose containing semicolons, so a naive
// split would cut statements in half.
func splitSQLStatementsQuoteAware(sqlText string) []string {
	var out []string
	var b strings.Builder
	inStr := false
	runes := []rune(strings.TrimSpace(sqlText))
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if inStr {
			b.WriteRune(c)
			if c == '\'' {
				if i+1 < len(runes) && runes[i+1] == '\'' {
					b.WriteRune(runes[i+1])
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		if c == '\'' {
			inStr = true
			b.WriteRune(c)
			continue
		}
		if c == ';' {
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, s)
			}
			b.Reset()
			continue
		}
		b.WriteRune(c)
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		out = append(out, s)
	}
	return out
}
