package repositories

import (
	"context"
	"database/sql"
	"errors"
)

// ErrDriverSessionNotFound is returned when a driver bearer token row is missing,
// expired, or revoked.
var ErrDriverSessionNotFound = errors.New("driver_auth_sessions: token not found")

// DriverAuthSession represents a hashed bearer-token row.
type DriverAuthSession struct {
	ID        int64
	UserID    int64
	TokenHash string
	ExpiresAt int64
	Revoked   bool
	CreatedAt int64
}

// DriverAuthSessionsRepo persists hashed opaque bearer tokens for native driver login.
type DriverAuthSessionsRepo struct {
	db *sql.DB
}

// NewDriverAuthSessionsRepo returns a repo.
func NewDriverAuthSessionsRepo(db *sql.DB) *DriverAuthSessionsRepo {
	return &DriverAuthSessionsRepo{db: db}
}

// Insert stores a new bearer-token row (hash only).
func (r *DriverAuthSessionsRepo) Insert(ctx context.Context, userID int64, tokenHash string, expiresAt int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO driver_auth_sessions (user_id, token_hash, expires_at)
		VALUES (?1, ?2, ?3)`,
		userID, tokenHash, expiresAt)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// GetByTokenHash returns the active (non-revoked, non-expired) row for the hash.
func (r *DriverAuthSessionsRepo) GetByTokenHash(ctx context.Context, tokenHash string, nowUnix int64) (*DriverAuthSession, error) {
	var s DriverAuthSession
	var rev int
	err := r.db.QueryRowContext(ctx, `
		SELECT id, user_id, token_hash, expires_at, revoked, created_at
		FROM driver_auth_sessions
		WHERE token_hash = ?1 AND revoked = 0 AND expires_at > ?2
		LIMIT 1`,
		tokenHash, nowUnix).Scan(&s.ID, &s.UserID, &s.TokenHash, &s.ExpiresAt, &rev, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrDriverSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	s.Revoked = rev != 0
	return &s, nil
}

// RevokeByTokenHash marks a single row revoked.
func (r *DriverAuthSessionsRepo) RevokeByTokenHash(ctx context.Context, tokenHash string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE driver_auth_sessions SET revoked = 1 WHERE token_hash = ?1 AND revoked = 0`,
		tokenHash)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeAllForUser revokes every active session for the user.
func (r *DriverAuthSessionsRepo) RevokeAllForUser(ctx context.Context, userID int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE driver_auth_sessions SET revoked = 1 WHERE user_id = ?1 AND revoked = 0`,
		userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
