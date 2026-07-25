package migrations

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// readGooseSection returns the statements between the given goose marker and the
// next one, so the test exercises the migration file itself rather than a copy.
func readGooseSection(t *testing.T, path, marker string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("%s: marker %q not found", path, marker)
	}
	text = text[start+len(marker):]
	if next := strings.Index(text, "-- +goose "); next >= 0 {
		text = text[:next]
	}

	// Strip comment lines before splitting on ';' — a comment may itself contain
	// a semicolon, which would otherwise cut a statement in half.
	var kept []string
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "--") {
			kept = append(kept, l)
		}
	}

	var stmts []string
	for _, raw := range strings.Split(strings.Join(kept, "\n"), ";") {
		if s := strings.TrimSpace(raw); s != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}

func applyMigration(t *testing.T, db *sql.DB, path, marker string) {
	t.Helper()
	for _, stmt := range readGooseSection(t, path, marker) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply %q: %v", stmt, err)
		}
	}
}

func newRideRequestsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE ride_requests (
		id TEXT PRIMARY KEY,
		rider_user_id INTEGER NOT NULL,
		status TEXT NOT NULL CHECK (status IN ('PENDING','ASSIGNED','CANCELLED','EXPIRED')),
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatal(err)
	}
	return db
}

const singlePendingMigration = "063_ride_requests_single_pending_per_rider.sql"

// The migration must survive a database that already contains the duplicates it
// exists to prevent. An aborted migration crash-loops the container, because the
// image runs `migrate -up && exec ./app`.
func TestSinglePendingMigration_DeduplicatesExistingRows(t *testing.T) {
	db := newRideRequestsDB(t)
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status, created_at) VALUES
		('a', 1, 'PENDING',  '2026-07-01 10:00:00'),
		('b', 1, 'PENDING',  '2026-07-01 11:00:00'),
		('c', 1, 'PENDING',  '2026-07-01 12:00:00'),
		('d', 2, 'PENDING',  '2026-07-01 10:00:00'),
		('e', 3, 'ASSIGNED', '2026-07-01 10:00:00'),
		('f', 3, 'EXPIRED',  '2026-07-01 09:00:00')`); err != nil {
		t.Fatal(err)
	}

	applyMigration(t, db, singlePendingMigration, "-- +goose Up")

	// Newest pending per rider survives; older ones are retired, not deleted.
	want := map[string]string{
		"a": "EXPIRED", "b": "EXPIRED", "c": "PENDING",
		"d": "PENDING", "e": "ASSIGNED", "f": "EXPIRED",
	}
	rows, err := db.Query(`SELECT id, status FROM ride_requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatal(err)
		}
		got[id] = status
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for id, wantStatus := range want {
		if got[id] != wantStatus {
			t.Errorf("request %s status = %q, want %q", id, got[id], wantStatus)
		}
	}
}

func TestSinglePendingMigration_IndexRejectsSecondPending(t *testing.T) {
	db := newRideRequestsDB(t)
	applyMigration(t, db, singlePendingMigration, "-- +goose Up")

	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status) VALUES ('a', 1, 'PENDING')`); err != nil {
		t.Fatalf("first pending request should be accepted: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status) VALUES ('b', 1, 'PENDING')`); err == nil {
		t.Fatal("a second PENDING request for the same rider must be rejected")
	}

	// Non-pending rows for the same rider stay unconstrained, and a different
	// rider is unaffected.
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status) VALUES ('c', 1, 'CANCELLED')`); err != nil {
		t.Errorf("non-PENDING insert should be allowed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status) VALUES ('d', 2, 'PENDING')`); err != nil {
		t.Errorf("another rider's PENDING insert should be allowed: %v", err)
	}
	// Once the pending request is closed, the rider can order again.
	if _, err := db.Exec(`UPDATE ride_requests SET status = 'CANCELLED' WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status) VALUES ('e', 1, 'PENDING')`); err != nil {
		t.Errorf("rider should be able to order again after cancelling: %v", err)
	}
}

func TestSinglePendingMigration_DownDropsIndex(t *testing.T) {
	db := newRideRequestsDB(t)
	applyMigration(t, db, singlePendingMigration, "-- +goose Up")
	applyMigration(t, db, singlePendingMigration, "-- +goose Down")

	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status) VALUES ('a', 1, 'PENDING')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ride_requests (id, rider_user_id, status) VALUES ('b', 1, 'PENDING')`); err != nil {
		t.Errorf("after Down the constraint should be gone: %v", err)
	}
}
