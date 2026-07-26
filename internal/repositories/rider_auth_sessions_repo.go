package repositories

import (
	"context"
	"database/sql"
	"errors"
)

// ErrRiderSessionNotFound is returned when a refresh-token row is missing,
// expired, revoked, or already used by a different client.
var ErrRiderSessionNotFound = errors.New("rider_auth_sessions: refresh token not found")

// RiderAuthSession represents a refresh-token row.
type RiderAuthSession struct {
	ID               int64
	UserID           int64
	RefreshHash      string
	RefreshExpiresAt int64
	Revoked          bool
	CreatedAt        int64
}

// RiderAuthSessionsRepo persists hashed refresh tokens for the rider native
// auth flow. Access tokens are stateless HS256 JWTs and do NOT live here.
type RiderAuthSessionsRepo struct {
	db *sql.DB
}

// NewRiderAuthSessionsRepo returns a repo.
func NewRiderAuthSessionsRepo(db *sql.DB) *RiderAuthSessionsRepo {
	return &RiderAuthSessionsRepo{db: db}
}

// Insert stores a new refresh-token row.
func (r *RiderAuthSessionsRepo) Insert(ctx context.Context, userID int64, refreshHash string, refreshExpiresAt int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO rider_auth_sessions (user_id, refresh_hash, refresh_expires_at)
		VALUES (?1, ?2, ?3)`,
		userID, refreshHash, refreshExpiresAt)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// GetByRefreshHash returns the row whose refresh_hash matches and that is
// not revoked / not expired. Otherwise ErrRiderSessionNotFound.
func (r *RiderAuthSessionsRepo) GetByRefreshHash(ctx context.Context, refreshHash string, nowUnix int64) (*RiderAuthSession, error) {
	var s RiderAuthSession
	var rev int
	err := r.db.QueryRowContext(ctx, `
		SELECT id, user_id, refresh_hash, refresh_expires_at, revoked, created_at
		FROM rider_auth_sessions
		WHERE refresh_hash = ?1 AND revoked = 0 AND refresh_expires_at > ?2
		LIMIT 1`,
		refreshHash, nowUnix).Scan(&s.ID, &s.UserID, &s.RefreshHash, &s.RefreshExpiresAt, &rev, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrRiderSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	s.Revoked = rev != 0
	return &s, nil
}

// RevokeByRefreshHash marks a single row revoked. Returns rows affected.
func (r *RiderAuthSessionsRepo) RevokeByRefreshHash(ctx context.Context, refreshHash string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE rider_auth_sessions SET revoked = 1 WHERE refresh_hash = ?1 AND revoked = 0`,
		refreshHash)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RotateByRefreshHash atomically revokes the presented refresh token and
// inserts its replacement in one transaction. Returns the session's user id.
//
// The conditional UPDATE doubles as winner selection for concurrent refreshes
// with the same token: exactly one caller flips revoked 0→1 and proceeds;
// every other caller affects 0 rows and gets ErrRiderSessionNotFound (→ 401).
// Any failure after the UPDATE rolls the whole transaction back, so the old
// refresh token is only ever revoked once its replacement is durably stored.
func (r *RiderAuthSessionsRepo) RotateByRefreshHash(ctx context.Context, oldHash string, nowUnix int64, newHash string, newExpiresAt int64) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE rider_auth_sessions SET revoked = 1
		WHERE refresh_hash = ?1 AND revoked = 0 AND refresh_expires_at > ?2`,
		oldHash, nowUnix)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrRiderSessionNotFound
	}
	var userID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT user_id FROM rider_auth_sessions WHERE refresh_hash = ?1 LIMIT 1`,
		oldHash).Scan(&userID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rider_auth_sessions (user_id, refresh_hash, refresh_expires_at)
		VALUES (?1, ?2, ?3)`,
		userID, newHash, newExpiresAt); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return userID, nil
}

// RevokeAllForUser marks every active session for the given user as revoked.
// Used by /v1/rider/auth/logout where we only know the user id (from access JWT).
func (r *RiderAuthSessionsRepo) RevokeAllForUser(ctx context.Context, userID int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE rider_auth_sessions SET revoked = 1 WHERE user_id = ?1 AND revoked = 0`,
		userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
