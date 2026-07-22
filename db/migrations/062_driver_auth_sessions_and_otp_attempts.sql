-- +goose Up
-- Opaque bearer sessions for native driver OTP login (hash only; plaintext never stored).
CREATE TABLE IF NOT EXISTS driver_auth_sessions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at INTEGER NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

CREATE INDEX IF NOT EXISTS idx_driver_auth_sessions_user
    ON driver_auth_sessions(user_id, revoked, expires_at);

-- Brute-force tracking for driver OTP verify.
ALTER TABLE driver_login_codes ADD COLUMN failed_attempts INTEGER NOT NULL DEFAULT 0;

-- +goose Down
DROP INDEX IF EXISTS idx_driver_auth_sessions_user;
DROP TABLE IF EXISTS driver_auth_sessions;
-- SQLite cannot DROP COLUMN portably across all deployments; leave failed_attempts on down.
