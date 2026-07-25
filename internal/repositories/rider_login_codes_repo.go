package repositories

import (
	"context"
	"database/sql"
	"errors"
)

// ErrRiderLoginCodeNotFound is returned when no active code row matches.
var ErrRiderLoginCodeNotFound = errors.New("rider_login_codes: no active code")

// RiderLoginCode is one row of rider_login_codes.
//
// CodeHash is the hex-encoded sha256 of (Salt + plaintext code). The
// plaintext code itself is NEVER stored or logged.
type RiderLoginCode struct {
	ID        int64
	Phone     string
	CodeHash  string
	Salt      string
	ExpiresAt int64
	Attempts  int
	Consumed  bool
	CreatedAt int64
}

// RiderLoginCodesRepo persists 6-digit OTP codes used by the rider Telegram
// bot login flow. All queries assume phone is already normalized to E.164
// (caller's responsibility — the service layer enforces this).
type RiderLoginCodesRepo struct {
	db *sql.DB
}

// NewRiderLoginCodesRepo returns a new repo.
func NewRiderLoginCodesRepo(db *sql.DB) *RiderLoginCodesRepo {
	return &RiderLoginCodesRepo{db: db}
}

// Insert stores a new code row. expiresAt is unix seconds.
// Returns the row id of the inserted row.
func (r *RiderLoginCodesRepo) Insert(ctx context.Context, phone, codeHash, salt string, expiresAt int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO rider_login_codes (phone, code_hash, salt, expires_at)
		VALUES (?1, ?2, ?3, ?4)`,
		phone, codeHash, salt, expiresAt)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// GetLatestActiveByPhone returns the most recent unconsumed, unexpired row.
// Returns ErrRiderLoginCodeNotFound if none.
func (r *RiderLoginCodesRepo) GetLatestActiveByPhone(ctx context.Context, phone string, nowUnix int64) (*RiderLoginCode, error) {
	var row RiderLoginCode
	var consumedInt int
	err := r.db.QueryRowContext(ctx, `
		SELECT id, phone, code_hash, salt, expires_at, attempts, consumed, created_at
		FROM rider_login_codes
		WHERE phone = ?1 AND consumed = 0 AND expires_at > ?2
		ORDER BY id DESC
		LIMIT 1`,
		phone, nowUnix).Scan(&row.ID, &row.Phone, &row.CodeHash, &row.Salt, &row.ExpiresAt, &row.Attempts, &consumedInt, &row.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrRiderLoginCodeNotFound
	}
	if err != nil {
		return nil, err
	}
	row.Consumed = consumedInt != 0
	return &row, nil
}

// IncAttempts atomically increments the attempts counter and returns the new value.
func (r *RiderLoginCodesRepo) IncAttempts(ctx context.Context, id int64) (int, error) {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE rider_login_codes SET attempts = attempts + 1 WHERE id = ?1`, id); err != nil {
		return 0, err
	}
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT attempts FROM rider_login_codes WHERE id = ?1`, id).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MarkConsumed sets consumed = 1 so the code cannot be reused.
func (r *RiderLoginCodesRepo) MarkConsumed(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE rider_login_codes SET consumed = 1 WHERE id = ?1`, id)
	return err
}

// PurgeExpired deletes rows older than cutoffUnix. Safe to call periodically.
func (r *RiderLoginCodesRepo) PurgeExpired(ctx context.Context, cutoffUnix int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM rider_login_codes WHERE expires_at < ?1`, cutoffUnix)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// LastCreatedAtForPhone returns the created_at unix seconds of the most recent
// code row (consumed or not). Returns 0 if none.
func (r *RiderLoginCodesRepo) LastCreatedAtForPhone(ctx context.Context, phone string) (int64, error) {
	var t sql.NullInt64
	err := r.db.QueryRowContext(ctx, `
		SELECT MAX(created_at) FROM rider_login_codes WHERE phone = ?1`, phone).Scan(&t)
	if err != nil {
		return 0, err
	}
	if !t.Valid {
		return 0, nil
	}
	return t.Int64, nil
}

// CountSinceForPhone returns how many code rows for phone were created at or
// after sinceUnix. Used for the per-hour rate limit.
func (r *RiderLoginCodesRepo) CountSinceForPhone(ctx context.Context, phone string, sinceUnix int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rider_login_codes
		WHERE phone = ?1 AND created_at >= ?2`,
		phone, sinceUnix).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// InvalidateActiveForPhone marks all currently-active rows for phone as consumed.
// Used when a new code is issued so the previous code can no longer be used.
func (r *RiderLoginCodesRepo) InvalidateActiveForPhone(ctx context.Context, phone string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE rider_login_codes SET consumed = 1 WHERE phone = ?1 AND consumed = 0`, phone)
	return err
}
