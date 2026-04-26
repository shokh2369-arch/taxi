-- +goose Up
-- Admin-managed destination places + approximate fare estimate storage (SQLite/Turso compatible)

CREATE TABLE IF NOT EXISTS places (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  lat REAL NOT NULL,
  lng REAL NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ride_requests: store selected drop point name (optional) and estimated price (approximate; not settlement).
ALTER TABLE ride_requests ADD COLUMN drop_name TEXT;
ALTER TABLE ride_requests ADD COLUMN estimated_price INTEGER NOT NULL DEFAULT 0;

-- +goose Down
DROP TABLE IF EXISTS places;

-- libSQL/SQLite 3.35+ supports DROP COLUMN
ALTER TABLE ride_requests DROP COLUMN drop_name;
ALTER TABLE ride_requests DROP COLUMN estimated_price;
