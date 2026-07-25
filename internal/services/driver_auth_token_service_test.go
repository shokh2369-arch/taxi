package services

import (
	"context"
	"database/sql"
	"testing"

	"taxi-mvp/internal/repositories"

	_ "modernc.org/sqlite"
)

func setupDriverSessionsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:driver_sessions_"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE driver_auth_sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		expires_at INTEGER NOT NULL,
		revoked INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
	)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// Logging in on a second device must invalidate the first.
//
// Without this, two phones held valid sessions for the same driver and both
// posted location and competed for the same trip, with neither aware of the
// other. The old session failing auth is how the first device learns to log out.
func TestDriverAuthTokens_SecondLoginRevokesFirst(t *testing.T) {
	db := setupDriverSessionsDB(t)
	svc := NewDriverAuthTokenService(repositories.NewDriverAuthSessionsRepo(db))
	ctx := context.Background()

	first, err := svc.Issue(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(ctx, first.AccessToken); err != nil {
		t.Fatalf("first token should be valid immediately after issue: %v", err)
	}

	second, err := svc.Issue(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccessToken == first.AccessToken {
		t.Fatal("second login must mint a distinct token")
	}

	if _, err := svc.Verify(ctx, first.AccessToken); err == nil {
		t.Error("the first device's token must stop working after a second login")
	}
	if uid, err := svc.Verify(ctx, second.AccessToken); err != nil || uid != 7 {
		t.Errorf("the newest token must remain valid: uid=%d err=%v", uid, err)
	}
}

// Revoking one driver must not disturb another.
func TestDriverAuthTokens_RevocationIsPerDriver(t *testing.T) {
	db := setupDriverSessionsDB(t)
	svc := NewDriverAuthTokenService(repositories.NewDriverAuthSessionsRepo(db))
	ctx := context.Background()

	other, err := svc.Issue(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if uid, err := svc.Verify(ctx, other.AccessToken); err != nil || uid != 8 {
		t.Errorf("driver 8's session must survive driver 7 logging in: uid=%d err=%v", uid, err)
	}
}
