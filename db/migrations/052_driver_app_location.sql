-- +goose Up
-- Native driver app location (additive; Telegram live location fields unchanged).

ALTER TABLE drivers ADD COLUMN app_lat REAL;
ALTER TABLE drivers ADD COLUMN app_lng REAL;
ALTER TABLE drivers ADD COLUMN app_last_seen_at TEXT;
ALTER TABLE drivers ADD COLUMN app_location_active INTEGER NOT NULL DEFAULT 0;

-- +goose Down
SELECT 1;

