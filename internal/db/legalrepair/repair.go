package legalrepair

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"strings"
)

//go:embed rebuild.sql
var rebuildSQL string

// NeedsRebuild reports whether legal_documents exists but lacks document_type
// (incompatible schema). If the table is missing, returns false so migrations can create it.
func NeedsRebuild(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='legal_documents'`).Scan(&n)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	var c int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('legal_documents') WHERE name='document_type'`).Scan(&c)
	if err != nil {
		return false, err
	}
	return c == 0, nil
}

// Ensure runs the embedded rebuild when NeedsRebuild is true. It drops legal_* data and
// recreates tables plus seeds; clears users/drivers terms_accepted flags.
func Ensure(ctx context.Context, db *sql.DB) error {
	need, err := NeedsRebuild(ctx, db)
	if err != nil {
		return err
	}
	if !need {
		return nil
	}
	log.Printf("legalrepair: legal_documents schema missing document_type; rebuilding legal_* tables (acceptances cleared)")
	return applyStatements(ctx, db, rebuildSQL)
}

func applyStatements(ctx context.Context, db *sql.DB, sqlText string) error {
	stmts := splitSQLStatements(sqlText)
	for i, q := range stmts {
		if q == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, q); err != nil {
			preview := q
			if len(preview) > 180 {
				preview = preview[:180] + "..."
			}
			return fmt.Errorf("statement %d: %w\n%s", i, err, preview)
		}
	}
	return nil
}

// splitSQLStatements splits on ';' outside single-quoted SQL strings ('' is escape).
func splitSQLStatements(sql string) []string {
	sql = strings.TrimSpace(sql)
	var out []string
	var b strings.Builder
	inStr := false
	runes := []rune(sql)
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
			s := strings.TrimSpace(b.String())
			if s != "" {
				out = append(out, s)
			}
			b.Reset()
			continue
		}
		b.WriteRune(c)
	}
	s := strings.TrimSpace(b.String())
	if s != "" {
		out = append(out, s)
	}
	return out
}
