-- +goose Up
-- Rider must confirm estimated price before dispatch; allow destination edits until confirmed.
ALTER TABLE ride_requests ADD COLUMN destination_confirmed INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- libSQL/SQLite 3.35+ supports DROP COLUMN
ALTER TABLE ride_requests DROP COLUMN destination_confirmed;

