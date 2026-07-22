package db

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// Open connects to Turso (libSQL), pings it, and returns the DB.
// databaseURL must be a libsql URL, e.g. libsql://your-db.turso.io?authToken=...
func Open(databaseURL string) (*sql.DB, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL or TURSO_DATABASE_URL + TURSO_AUTH_TOKEN required")
	}
	db, err := sql.Open("libsql", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("sql open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	// SQLite does not enforce REFERENCES constraints unless foreign_keys is on.
	// Best-effort: some libsql transports may not support the pragma; do not fail startup.
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		log.Printf("db: enable foreign_keys pragma: %v", err)
	}
	if err := ensureTripsDriverStatusIndex(db); err != nil {
		log.Printf("db: ensure trips(driver_user_id, status) index: %v", err)
	}
	return db, nil
}

// ensureTripsDriverStatusIndex creates the index used by promo/referral finished-trip count queries
// (startup repair; migrations may not have run on this database yet, so guard on the table).
func ensureTripsDriverStatusIndex(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'trips'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_trips_driver_status ON trips(driver_user_id, status)`)
	return err
}

// Close closes the database connection.
func Close(db *sql.DB) error {
	if db == nil {
		return nil
	}
	return db.Close()
}
