-- +goose Up
-- One-time signup bonus (100k so'm) when driver completes application; avoid double-credit.
ALTER TABLE drivers ADD COLUMN signup_bonus_paid INTEGER NOT NULL DEFAULT 0;
-- +goose Down
SELECT 1;
