-- +goose Up
-- Rejecting a driver nulls both document file_ids and resets application_step,
-- so the applicant is put straight back in the queue with no trace that they
-- were ever rejected. A second admin reviewing the re-application saw a clean
-- first-time applicant. Keep a minimal trail so repeat applicants are visible.
ALTER TABLE drivers ADD COLUMN rejection_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE drivers ADD COLUMN last_rejected_at TEXT;

-- +goose Down
-- SQLite cannot drop columns on older versions; leaving them is harmless.
SELECT 1;
