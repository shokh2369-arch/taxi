// Package broadcasts ensures broadcast tables exist (admin broadcasts -> rider app + Telegram fan-out).
// Production Turso DBs may not run goose for every migration.
package broadcasts

import (
	"context"
	"database/sql"
	"fmt"
)

func Ensure(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("broadcasts: nil db")
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS broadcast_posts (
			id                       TEXT PRIMARY KEY,
			title                    TEXT,
			body                     TEXT NOT NULL,
			created_at               TEXT NOT NULL DEFAULT (datetime('now')),
			status                   TEXT NOT NULL DEFAULT 'published' CHECK (status IN ('draft','published')),
			created_by_telegram_id   INTEGER NOT NULL DEFAULT 0,
			audience                 TEXT NOT NULL DEFAULT 'all_riders',
			cloudinary_public_id     TEXT,
			cloudinary_secure_url    TEXT,
			media_type               TEXT,
			width                    INTEGER,
			height                   INTEGER,
			format                   TEXT
		)`); err != nil {
		return fmt.Errorf("broadcasts: create broadcast_posts: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_broadcast_posts_created
			ON broadcast_posts(created_at DESC, id DESC)`); err != nil {
		return fmt.Errorf("broadcasts: create idx_broadcast_posts_created: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS broadcast_telegram_deliveries (
			broadcast_id  TEXT NOT NULL REFERENCES broadcast_posts(id) ON DELETE CASCADE,
			chat_id       INTEGER NOT NULL,
			delivered_at  TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (broadcast_id, chat_id)
		)`); err != nil {
		return fmt.Errorf("broadcasts: create broadcast_telegram_deliveries: %w", err)
	}
	return nil
}
