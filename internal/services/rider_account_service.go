package services

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"

	"taxi-mvp/internal/domain"
)

// ErrRiderAccountActiveTrip is returned when deletion is requested while a trip
// is WAITING/ARRIVED/STARTED. The rider must finish or cancel it first — pulling
// the account out from under a live trip would strand the assigned driver.
var ErrRiderAccountActiveTrip = errors.New("rider_account: active trip in progress")

// DeleteAccount implements DELETE /v1/rider/account (Google Play requires an
// in-app account deletion path for apps with login).
//
// PII is erased/anonymized rather than the row deleted: trips and ride_requests
// reference users.id, and fare/commission records must stay consistent. What
// happens, in one transaction:
//
//   - users: phone and name are cleared; telegram_id is replaced with -id (the
//     column is UNIQUE NOT NULL, and the negative sentinel cannot collide with a
//     real Telegram id, so the Telegram link is severed and the bots stop
//     recognizing the person).
//   - rider_login_codes for the phone are deleted (they key on the phone).
//   - rider_app_notifications for the user are deleted.
//   - every rider auth session is revoked — all devices are logged out and the
//     presented bearer stops working the moment this returns.
//   - any still-PENDING ride request is cancelled so it cannot dispatch to a
//     driver after the account is gone.
//
// If the same person later returns to the Telegram bot, a fresh users row is
// created from their real telegram_id — deletion does not blocklist them.
func (s *RiderAuthService) DeleteAccount(ctx context.Context, userID int64) error {
	if s == nil || s.db == nil {
		return ErrRiderAuthInternal
	}
	if userID <= 0 {
		return ErrRiderAuthInternal
	}

	// Precondition outside the tx: no live trip.
	var activeTripID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM trips
		WHERE rider_user_id = ?1 AND status IN (?2, ?3, ?4)
		LIMIT 1`,
		userID, domain.TripStatusWaiting, domain.TripStatusArrived, domain.TripStatusStarted).Scan(&activeTripID)
	if err == nil && strings.TrimSpace(activeTripID) != "" {
		return ErrRiderAccountActiveTrip
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ErrRiderAuthInternal
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrRiderAuthInternal
	}
	defer tx.Rollback()

	var phone sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT phone FROM users WHERE id = ?1 AND role = 'rider'`, userID).Scan(&phone); err != nil {
		// Unknown id or a non-rider account: nothing to delete via this flow.
		return ErrRiderAuthInternal
	}

	// Sever identity. -id is unique per row, so repeated deletions cannot collide.
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET phone = NULL, name = NULL, telegram_id = -id
		WHERE id = ?1 AND role = 'rider'`, userID); err != nil {
		return ErrRiderAuthInternal
	}
	if phone.Valid && strings.TrimSpace(phone.String) != "" {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM rider_login_codes WHERE phone = ?1`, strings.TrimSpace(phone.String)); err != nil {
			return ErrRiderAuthInternal
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM rider_app_notifications WHERE rider_user_id = ?1`, userID); err != nil {
		return ErrRiderAuthInternal
	}
	// A PENDING request must not dispatch for an account that no longer exists.
	if _, err := tx.ExecContext(ctx, `
		UPDATE ride_requests SET status = ?2
		WHERE rider_user_id = ?1 AND status = ?3`,
		userID, domain.RequestStatusCancelled, domain.RequestStatusPending); err != nil {
		return ErrRiderAuthInternal
	}
	// Log out every device.
	if _, err := tx.ExecContext(ctx, `
		UPDATE rider_auth_sessions SET revoked = 1 WHERE user_id = ?1 AND revoked = 0`, userID); err != nil {
		return ErrRiderAuthInternal
	}

	if err := tx.Commit(); err != nil {
		return ErrRiderAuthInternal
	}
	// user id only — the whole point is that nothing identifying remains.
	log.Printf("rider_account: deleted user_id=%d", userID)
	return nil
}
