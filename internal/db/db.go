package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// Pool defaults. Turso is a remote database, so an unbounded pool (database/sql's
// default) lets a traffic spike open arbitrarily many connections, while the
// default of 2 idle connections forces a fresh handshake for nearly every query.
// Keeping idle == open makes connections reusable instead of churning.
const (
	defaultMaxOpenConns    = 20
	defaultConnMaxLifetime = 30 * time.Minute
	defaultConnMaxIdleTime = 5 * time.Minute
	defaultBusyTimeoutMS   = 5000
)

// Open connects to Turso (libSQL), pings it, and returns the DB.
// databaseURL must be a libsql URL, e.g. libsql://your-db.turso.io?authToken=...
func Open(databaseURL string) (*sql.DB, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL or TURSO_DATABASE_URL + TURSO_AUTH_TOKEN required")
	}
	db, err := openWithSessionPragmas(databaseURL)
	if err != nil {
		return nil, err
	}
	configurePool(db)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if err := ensureTripsDriverStatusIndex(db); err != nil {
		log.Printf("db: ensure trips(driver_user_id, status) index: %v", err)
	}
	return db, nil
}

// openWithSessionPragmas opens the pool behind a connector that applies session
// pragmas to every new connection. SQLite pragmas are per-connection: issuing
// them once against a *sql.DB only configures whichever connection the pool
// happened to hand out, leaving every other connection at the driver default.
// Falls back to a plain open if the driver cannot be wrapped.
//
// IMPORTANT — remote Turso/libSQL (hrana) rejects PRAGMA busy_timeout /
// foreign_keys ("SQL not allowed statement"). A failed Exec also closes the
// stream, so "best effort" pragma application leaves a bad connection in the
// pool and breaks the next statement (startup repairs → fatal). Session
// pragmas are therefore skipped entirely for remote URLs; see isRemoteLibSQL.
func openWithSessionPragmas(databaseURL string) (*sql.DB, error) {
	if isRemoteLibSQL(databaseURL) {
		if envBool("DB_FOREIGN_KEYS", false) {
			log.Printf("db: WARNING DB_FOREIGN_KEYS=true has no effect on remote libSQL — " +
				"session pragmas are not applied (Turso rejects them). Treat foreign keys as OFF.")
		}
		// Plain open: no pragmaConnector. Turso rejects busy_timeout and a failed
		// Exec leaves the hrana stream closed ("driver: bad connection").
		return sql.Open("libsql", databaseURL)
	}
	return openPragmaDB("libsql", databaseURL, sessionPragmas())
}

// isRemoteLibSQL reports whether the URL points at a remote hrana endpoint
// (as opposed to an embedded/file database), where session pragmas do not stick.
func isRemoteLibSQL(dsn string) bool {
	d := strings.ToLower(strings.TrimSpace(dsn))
	return strings.HasPrefix(d, "libsql://") ||
		strings.HasPrefix(d, "wss://") ||
		strings.HasPrefix(d, "ws://") ||
		strings.HasPrefix(d, "https://") ||
		strings.HasPrefix(d, "http://")
}

func openPragmaDB(driverName, dsn string, pragmas []string) (*sql.DB, error) {
	probe, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sql open: %w", err)
	}
	drv := probe.Driver()
	_ = probe.Close()
	if drv == nil || len(pragmas) == 0 {
		return sql.Open(driverName, dsn)
	}
	return sql.OpenDB(pragmaConnector{drv: drv, dsn: dsn, pragmas: pragmas}), nil
}

// sessionPragmas returns the statements applied to each new connection.
// See openWithSessionPragmas: these do not persist on remote libSQL.
//
// foreign_keys is opt-in via DB_FOREIGN_KEYS=true. It stays off by default
// because this schema has been written and backfilled without enforcement, so
// switching it on wholesale could start rejecting writes that succeed today —
// audit for orphaned rows first, then enable it deliberately.
func sessionPragmas() []string {
	pragmas := []string{fmt.Sprintf("PRAGMA busy_timeout = %d", envInt("DB_BUSY_TIMEOUT_MS", defaultBusyTimeoutMS))}
	if envBool("DB_FOREIGN_KEYS", false) {
		pragmas = append(pragmas, "PRAGMA foreign_keys = ON")
	}
	return pragmas
}

func configurePool(db *sql.DB) {
	maxOpen := envInt("DB_MAX_OPEN_CONNS", defaultMaxOpenConns)
	if maxOpen < 1 {
		maxOpen = 1
	}
	maxIdle := envInt("DB_MAX_IDLE_CONNS", maxOpen)
	if maxIdle < 1 {
		maxIdle = 1
	}
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(time.Duration(envInt("DB_CONN_MAX_LIFETIME_SEC", int(defaultConnMaxLifetime.Seconds()))) * time.Second)
	db.SetConnMaxIdleTime(time.Duration(envInt("DB_CONN_MAX_IDLE_SEC", int(defaultConnMaxIdleTime.Seconds()))) * time.Second)
}

// pragmaConnector opens connections through the underlying driver and applies
// session pragmas before the connection enters the pool.
type pragmaConnector struct {
	drv     driver.Driver
	dsn     string
	pragmas []string
}

func (c pragmaConnector) Driver() driver.Driver { return c.drv }

func (c pragmaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	for _, p := range c.pragmas {
		// Best effort: some libsql transports reject pragmas. Keep the connection
		// usable rather than failing the request, matching prior behaviour.
		if err := execOnConn(ctx, conn, p); err != nil {
			log.Printf("db: %q on new connection: %v", p, err)
		}
	}
	return conn, nil
}

func execOnConn(ctx context.Context, conn driver.Conn, stmt string) error {
	if ec, ok := conn.(driver.ExecerContext); ok {
		_, err := ec.ExecContext(ctx, stmt, nil)
		return err
	}
	st, err := conn.Prepare(stmt)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	_, err = st.Exec(nil)
	return err
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("db: %s=%q is not an integer; using %d", key, raw, def)
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return def
	}
	switch raw {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
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
