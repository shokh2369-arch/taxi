-- +goose Up
-- Driver-side cancellation accountability.
--
-- Riders who cancel after a driver accepts are warned, then blocked for 30
-- minutes, then 24 hours (migration 050). The opposite direction — a driver who
-- accepts and then cancels — was never counted at all, even though it is the
-- more damaging one: the rider has been told a specific car is coming and is
-- left waiting, often at night, with nothing to show for it.
--
-- These tables mirror rider_abuse_events / rider_abuse_state. Enforcement is a
-- dispatch cooldown rather than a hard block: a driver who is cut off entirely
-- earns nothing, which punishes the platform as much as the driver.
CREATE TABLE IF NOT EXISTS driver_cancel_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  driver_user_id INTEGER NOT NULL,
  trip_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_driver_cancel_events_driver_created_at
  ON driver_cancel_events (driver_user_id, created_at);

CREATE TABLE IF NOT EXISTS driver_cancel_state (
  driver_user_id INTEGER PRIMARY KEY,
  cooldown_until TEXT,
  escalation_level INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT
);

-- +goose Down
DROP INDEX IF EXISTS idx_driver_cancel_events_driver_created_at;
DROP TABLE IF EXISTS driver_cancel_events;
DROP TABLE IF EXISTS driver_cancel_state;
