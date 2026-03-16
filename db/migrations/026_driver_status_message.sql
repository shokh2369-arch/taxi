-- +goose Up
-- Add status_message_id for pinned driver status panel.
ALTER TABLE drivers ADD COLUMN status_message_id INTEGER;

-- +goose Down
SELECT 1;