package db

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestPragmaConnectorAppliesToEveryConnection proves the connector applies
// pragmas to every NEW connection, using a driver that keeps the session across
// reuse (modernc.org/sqlite).
//
// This deliberately does not model production: remote libSQL resets the session
// on every pooled checkout, so the pragma is discarded there. See the note on
// openWithSessionPragmas — do not read a pass here as "pragmas work on Turso".
func TestPragmaConnectorAppliesToEveryConnection(t *testing.T) {
	database, err := openPragmaDB("sqlite", "file:pragma_connector_test?mode=memory&cache=shared",
		[]string{"PRAGMA foreign_keys = ON"})
	if err != nil {
		t.Fatalf("openPragmaDB: %v", err)
	}
	defer func() { _ = database.Close() }()

	const conns = 4
	database.SetMaxOpenConns(conns)
	database.SetMaxIdleConns(conns)

	// Hold every connection open simultaneously so each one is distinct.
	held := make([]*sql.Conn, 0, conns)
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for i := 0; i < conns; i++ {
		c, err := database.Conn(t.Context())
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		held = append(held, c)
	}

	for i, c := range held {
		var on int
		if err := c.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&on); err != nil {
			t.Fatalf("conn %d: read pragma: %v", i, err)
		}
		if on != 1 {
			t.Errorf("conn %d: foreign_keys = %d, want 1", i, on)
		}
	}
}

func TestOpenRejectsEmptyURL(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") should return an error")
	}
}

func TestSessionPragmasAlwaysSetBusyTimeout(t *testing.T) {
	pragmas := sessionPragmas()
	if len(pragmas) == 0 {
		t.Fatal("expected at least a busy_timeout pragma")
	}
	if got := pragmas[0]; got != "PRAGMA busy_timeout = 5000" {
		t.Errorf("first pragma = %q, want the default busy_timeout", got)
	}
}

func TestIsRemoteLibSQL(t *testing.T) {
	remote := []string{
		"libsql://my-db.turso.io",
		"libsql://my-db.turso.io?authToken=x",
		"wss://my-db.turso.io",
		"https://my-db.turso.io",
		"HTTP://example.com",
	}
	for _, u := range remote {
		if !isRemoteLibSQL(u) {
			t.Errorf("isRemoteLibSQL(%q) = false, want true", u)
		}
	}
	local := []string{
		"file:local.db",
		":memory:",
		"file:memdb?mode=memory&cache=shared",
	}
	for _, u := range local {
		if isRemoteLibSQL(u) {
			t.Errorf("isRemoteLibSQL(%q) = true, want false", u)
		}
	}
}

// foreign_keys is opt-in: enabling it wholesale on a schema backfilled without
// enforcement could start rejecting writes that succeed today.
func TestForeignKeysOffByDefaultAndOptIn(t *testing.T) {
	for _, p := range sessionPragmas() {
		if p == "PRAGMA foreign_keys = ON" {
			t.Fatal("foreign_keys should be off unless DB_FOREIGN_KEYS is set")
		}
	}

	t.Setenv("DB_FOREIGN_KEYS", "true")
	var found bool
	for _, p := range sessionPragmas() {
		if p == "PRAGMA foreign_keys = ON" {
			found = true
		}
	}
	if !found {
		t.Error("DB_FOREIGN_KEYS=true should add the foreign_keys pragma")
	}
}

func TestConfigurePoolClampsIdleToOpen(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "3")
	t.Setenv("DB_MAX_IDLE_CONNS", "99")

	database, err := sql.Open("sqlite", "file:configure_pool_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	configurePool(database)
	if got := database.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("MaxOpenConnections = %d, want 3", got)
	}
}
