-- +goose Up
-- OTP for native driver login via phone; codes sent by existing Telegram driver bot (HTTP only).

CREATE TABLE driver_login_codes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used INTEGER NOT NULL DEFAULT 0 CHECK (used IN (0, 1)),
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_driver_login_codes_user_created ON driver_login_codes(user_id, created_at);

-- +goose Down
DROP INDEX IF EXISTS idx_driver_login_codes_user_created;
DROP TABLE IF EXISTS driver_login_codes;
