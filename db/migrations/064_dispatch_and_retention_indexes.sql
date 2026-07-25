-- +goose Up
-- Indexes for the two hottest scans on request_notifications.
--
-- Both existing indexes lead with request_id (migration 004), but the dispatch
-- long-poll filters on driver_user_id and the expiry worker filters on status.
-- Neither could use an index, so both full-scanned a table that is never pruned:
-- the long-poll once per second per online driver, the expiry worker every 5s.
CREATE INDEX IF NOT EXISTS idx_request_notifications_driver_status_created
  ON request_notifications(driver_user_id, status, created_at);

CREATE INDEX IF NOT EXISTS idx_request_notifications_status_created
  ON request_notifications(status, created_at);

-- Retention sweeps delete by age; without this they would scan the whole table.
CREATE INDEX IF NOT EXISTS idx_trip_locations_ts
  ON trip_locations(ts);

-- +goose Down
DROP INDEX IF EXISTS idx_request_notifications_driver_status_created;
DROP INDEX IF EXISTS idx_request_notifications_status_created;
DROP INDEX IF EXISTS idx_trip_locations_ts;
