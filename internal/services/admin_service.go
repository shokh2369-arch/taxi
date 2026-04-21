package services

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"taxi-mvp/internal/accounting"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/models"
	"taxi-mvp/internal/repositories"
)

// DashboardSummary is returned by the admin dashboard endpoint.
type DashboardSummary struct {
	TotalDrivers        int64 `json:"total_drivers"`
	ActiveDrivers       int64 `json:"active_drivers"`
	InactiveDrivers     int64 `json:"inactive_drivers"`
	TotalDriverBalances int64 `json:"total_driver_balances"` // promo + cash (compat)
	TotalPromoBalances  int64 `json:"total_promo_balances"`  // platform promotional credit only
	TotalCashBalances   int64 `json:"total_cash_balances"`   // real-wallet leg
	TodaysTrips         int64 `json:"todays_trips"`
}

// AdminService coordinates admin-facing driver, payment, and dashboard operations.
type AdminService struct {
	db       *sql.DB
	drivers  repositories.AdminDriverRepository
	payments repositories.PaymentRepository
	trips    repositories.TripStatsRepository
	ledger   *repositories.DriverLedgerRepository
}

func adminIsMissingColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column")
}

// NewAdminService constructs an AdminService.
func NewAdminService(
	db *sql.DB,
	drivers repositories.AdminDriverRepository,
	payments repositories.PaymentRepository,
	trips repositories.TripStatsRepository,
) *AdminService {
	return &AdminService{
		db:       db,
		drivers:  drivers,
		payments: payments,
		trips:    trips,
		ledger:   repositories.NewDriverLedgerRepository(db),
	}
}

// ListDrivers returns admin DTOs with computed ACTIVE/INACTIVE status.
func (s *AdminService) ListDrivers(ctx context.Context) ([]models.AdminDriverDTO, error) {
	ds, err := s.drivers.ListDriversWithBalance(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]models.AdminDriverDTO, 0, len(ds))
	for _, d := range ds {
		status := "INACTIVE"
		if d.Balance > 0 {
			status = "ACTIVE"
		}
		out = append(out, models.AdminDriverDTO{
			DriverID:                     d.ID,
			Name:                         d.Name,
			Phone:                        d.Phone,
			CarModel:                     d.CarModel,
			PlateNumber:                  d.PlateNumber,
			PromoBalance:                 d.PromoBalance,
			CashBalance:                  d.CashBalance,
			Balance:                      d.Balance,
			TotalPaid:                    d.TotalPaid,
			Status:                       status,
			VerificationStatus:           d.VerificationStatus,
			DriverTermsOK:                d.HasDriverTerms != 0,
			UserTermsOK:                  d.HasUserTerms != 0,
			PrivacyOK:                    d.HasPrivacy != 0,
			DriverTermsAcceptedVersion:   d.AcceptedDriverTermsVersion,
			UserTermsAcceptedVersion:     d.AcceptedUserTermsVersion,
			PrivacyPolicyAcceptedVersion: d.AcceptedPrivacyVersion,
			LegacyUserTermsFlag:          d.UserTermsAcceptedLegacy,
			LegacyDriverTermsFlag:        d.DriverTermsLegacy,
		})
	}
	return out, nil
}

// ListRiders returns riders for the admin dashboard (Foydalanuvchilar).
func (s *AdminService) ListRiders(ctx context.Context) ([]models.AdminRiderDTO, error) {
	return s.drivers.ListRidersForAdmin(ctx)
}

// ListDriverLedger returns recent driver_ledger rows (audit: promo vs cash).
func (s *AdminService) ListDriverLedger(ctx context.Context, driverID int64, limit int) ([]models.DriverLedgerEntry, error) {
	if s.ledger == nil {
		return nil, nil
	}
	return s.ledger.ListByDriver(ctx, driverID, limit)
}

// SetDriverVerification sets verification_status to "approved" or "rejected". Returns the driver's Telegram ID for notification.
func (s *AdminService) SetDriverVerification(ctx context.Context, driverUserID int64, status string) (telegramID int64, err error) {
	if status != "approved" && status != "rejected" {
		return 0, nil
	}
	if err := s.drivers.UpdateVerificationStatus(ctx, driverUserID, status); err != nil {
		return 0, err
	}
	if status == "approved" {
		if err := accounting.TryGrantSignupPromoOnce(ctx, s.db, driverUserID); err != nil {
			log.Printf("admin_service: signup promo on approve user_id=%d: %v", driverUserID, err)
		}
	}
	telegramID, err = s.drivers.GetDriverTelegramID(ctx, driverUserID)
	return telegramID, err
}

// AddDriverBalance records a cash-wallet top-up only (not promotional credit). Creates driver_ledger CASH_TOPUP + payments deposit.
func (s *AdminService) AddDriverBalance(ctx context.Context, driverID int64, amountCents int64, note string) error {
	if amountCents <= 0 {
		return nil
	}
	if s.db == nil {
		return nil
	}
	return accounting.GrantCashTopUp(ctx, s.db, s.payments, driverID, amountCents, note)
}

// AdjustDriverBalance applies a signed manual delta to the driver's cash wallet and total balance, and records an audit ledger entry.
// Does not create a payments row (admin-side correction only).
// Returns final balances and is_active flag after adjustment.
func (s *AdminService) AdjustDriverBalance(ctx context.Context, driverID int64, delta int64, reason string, adminID int64) (promoBalance, cashBalance, totalBalance int64, isActive int, err error) {
	if delta == 0 || s.db == nil {
		return 0, 0, 0, 0, fmt.Errorf("amount must be non-zero")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var promo, cash, total int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(promo_balance, 0), COALESCE(cash_balance, 0), COALESCE(balance, 0)
		FROM drivers WHERE user_id = ?1`, driverID).Scan(&promo, &cash, &total); err != nil {
		return 0, 0, 0, 0, err
	}

	// Business rule: any decrease must come entirely from cash_balance; promo_balance is never reduced.
	if delta < 0 && -delta > cash {
		return 0, 0, 0, 0, fmt.Errorf("Promo balansni kamaytirib bo‘lmaydi")
	}

	newTotal := total + delta
	if newTotal < promo {
		return 0, 0, 0, 0, fmt.Errorf("new total balance (%d) cannot be less than promo_balance (%d)", newTotal, promo)
	}
	newCash := newTotal - promo
	if _, err := tx.ExecContext(ctx, `
		UPDATE drivers SET cash_balance = ?1, balance = ?2,
		  is_active = CASE WHEN ?2 <= 0 THEN 0 ELSE is_active END
		WHERE user_id = ?3`,
		newCash, newTotal, driverID); err != nil {
		return 0, 0, 0, 0, err
	}

	ledger := repositories.NewDriverLedgerRepository(s.db)
	refType := "admin_adjust"
	refID := "admin:" + strconv.FormatInt(adminID, 10)
	note := reason
	if note == "" {
		note = "Admin manual balance adjustment"
	}
	e := &models.DriverLedgerEntry{
		DriverID:      driverID,
		Bucket:        models.LedgerBucketCash,
		EntryType:     models.LedgerEntryManualAdjustment,
		Amount:        delta,
		ReferenceType: &refType,
		ReferenceID:   &refID,
		Note:          &note,
	}
	if err := ledger.InsertTx(ctx, tx, e); err != nil {
		return 0, 0, 0, 0, err
	}

	// Read back final balances and is_active for response.
	var promoFinal, cashFinal, totalFinal int64
	var isActiveFinal int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(promo_balance,0), COALESCE(cash_balance,0), COALESCE(balance,0), COALESCE(is_active,0)
		FROM drivers WHERE user_id = ?1`, driverID).Scan(&promoFinal, &cashFinal, &totalFinal, &isActiveFinal); err != nil {
		return 0, 0, 0, 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, 0, 0, err
	}
	return promoFinal, cashFinal, totalFinal, isActiveFinal, nil
}

// DeductDriverCashBalance deducts a positive amount from the driver's cash balance only (promo is never reduced).
// Caps deduction to available cash (effectiveDeduction = min(amount, cash_balance)).
// Returns final balances, is_active, actual deducted amount, and whether the request was capped.
// Records a MANUAL_DEDUCTION ledger entry when effectiveDeduction > 0; does not create any payments row.
func (s *AdminService) DeductDriverCashBalance(ctx context.Context, driverID int64, amount int64, reason string) (promoBalance, cashBalance, totalBalance int64, isActive int, deducted int64, wasCapped bool, err error) {
	if amount <= 0 || s.db == nil {
		return 0, 0, 0, 0, 0, false, fmt.Errorf("amount must be greater than zero")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var promo, cash, total int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(promo_balance, 0), COALESCE(cash_balance, 0), COALESCE(balance, 0)
		FROM drivers WHERE user_id = ?1`, driverID).Scan(&promo, &cash, &total); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}

	// effectiveDeduction = min(requested amount, cash_balance)
	effective := amount
	if effective > cash {
		effective = cash
	}
	wasCapped = effective < amount

	// Nothing to deduct (cash already zero) – treat as no-op success.
	if effective == 0 {
		return promo, cash, total, 0, 0, wasCapped, nil
	}

	newCash := cash - effective
	newTotal := promo + newCash

	if _, err := tx.ExecContext(ctx, `
		UPDATE drivers SET cash_balance = ?1, balance = ?2,
		  is_active = CASE WHEN ?2 <= 0 THEN 0 ELSE is_active END
		WHERE user_id = ?3`,
		newCash, newTotal, driverID); err != nil {
		return 0, 0, 0, 0, 0, wasCapped, err
	}

	ledger := repositories.NewDriverLedgerRepository(s.db)
	refType := "admin_deduct"
	// Ensure reference_id is unique per deduction to avoid UNIQUE constraint conflicts
	// on (driver_id, reference_type, reference_id).
	refID := fmt.Sprintf("admin:dashboard:%d", time.Now().UnixNano())
	note := reason
	if note == "" {
		note = "Admin manual cash deduction"
	}
	e := &models.DriverLedgerEntry{
		DriverID:      driverID,
		Bucket:        models.LedgerBucketCash,
		EntryType:     models.LedgerEntryManualDeduction,
		Amount:        -effective,
		ReferenceType: &refType,
		ReferenceID:   &refID,
		Note:          &note,
	}
	if err := ledger.InsertTx(ctx, tx, e); err != nil {
		return 0, 0, 0, 0, 0, wasCapped, err
	}

	var promoFinal, cashFinal, totalFinal int64
	var isActiveFinal int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(promo_balance,0), COALESCE(cash_balance,0), COALESCE(balance,0), COALESCE(is_active,0)
		FROM drivers WHERE user_id = ?1`, driverID).Scan(&promoFinal, &cashFinal, &totalFinal, &isActiveFinal); err != nil {
		return 0, 0, 0, 0, 0, wasCapped, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, 0, 0, 0, wasCapped, err
	}
	return promoFinal, cashFinal, totalFinal, isActiveFinal, effective, wasCapped, nil
}

// ListPayments returns payment history, optionally filtered by driver.
func (s *AdminService) ListPayments(ctx context.Context, driverID *int64) ([]models.Payment, error) {
	return s.payments.ListPayments(ctx, driverID, nil, nil)
}

// GetDashboard computes summary stats for the admin dashboard.
func (s *AdminService) GetDashboard(ctx context.Context) (*DashboardSummary, error) {
	ds, err := s.drivers.ListDriversWithBalance(ctx)
	if err != nil {
		return nil, err
	}
	var summary DashboardSummary
	for _, d := range ds {
		summary.TotalDrivers++
		if d.Balance > 0 {
			summary.ActiveDrivers++
		} else {
			summary.InactiveDrivers++
		}
		summary.TotalDriverBalances += d.Balance
		summary.TotalPromoBalances += d.PromoBalance
		summary.TotalCashBalances += d.CashBalance
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	tripsToday, err := s.trips.CountTripsForDay(ctx, day)
	if err != nil {
		return nil, err
	}
	summary.TodaysTrips = tripsToday
	return &summary, nil
}

// AdminMapDriver is a minimal view for the admin map (location + activity only).
type AdminMapDriver struct {
	ID                 int64   `json:"id"`
	LastLat            float64 `json:"last_lat"`
	LastLng            float64 `json:"last_lng"`
	IsActive           int     `json:"is_active"`
	LiveLocationActive int     `json:"live_location_active"`
	LastLocationAt     string  `json:"last_location_at,omitempty"`
}

// AdminMapRideRequest is the admin map view of a pending ride request (GET .../map/ride-requests).
// Rider join keys and rider_phone are always present at top level; rider_phone is "" when users.phone is null or blank.
type AdminMapRideRequest struct {
	ID         string  `json:"id"`
	UserID     int64   `json:"user_id"`
	TelegramID int64   `json:"telegram_id"`
	RiderPhone string  `json:"rider_phone"`
	RiderName  string  `json:"rider_name"`
	PickupLat  float64 `json:"pickup_lat"`
	PickupLng  float64 `json:"pickup_lng"`
	Status     string  `json:"status"`
}

// ListActiveDriversForMap returns drivers with valid coordinates for the admin map.
// IMPORTANT: it must include offline drivers too (is_active=0, live_location_active=0).
func (s *AdminService) ListActiveDriversForMap(ctx context.Context) ([]AdminMapDriver, error) {
	if s.db == nil {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-90 * time.Second).Format("2006-01-02 15:04:05")

	// Prefer effective location (APP fresh wins) but stay backward compatible when DB isn't migrated.
	queryApp := `
		SELECT user_id,
		       last_lat, last_lng,
		       app_lat, app_lng, app_last_seen_at, COALESCE(app_location_active, 0),
		       COALESCE(is_active, 0),
		       COALESCE(live_location_active, 0), COALESCE(last_live_location_at, ''),
		       COALESCE(last_seen_at, '')
		FROM drivers
		WHERE (last_lat IS NOT NULL AND last_lng IS NOT NULL) OR (app_lat IS NOT NULL AND app_lng IS NOT NULL)`

	queryTelegramOnly := `
		SELECT user_id, last_lat, last_lng, COALESCE(is_active, 0), COALESCE(live_location_active, 0), COALESCE(last_seen_at, '')
		FROM drivers
		WHERE last_lat IS NOT NULL AND last_lng IS NOT NULL`

	rows, err := s.db.QueryContext(ctx, queryApp)
	appColsOK := true
	if err != nil && adminIsMissingColumnErr(err) {
		appColsOK = false
		rows, err = s.db.QueryContext(ctx, queryTelegramOnly)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminMapDriver
	for rows.Next() {
		var d AdminMapDriver
		if appColsOK {
			var (
				lastLat, lastLng sql.NullFloat64
				appLat, appLng   sql.NullFloat64
				appLast          sql.NullString
				appActive        int
				liveActive       int
				lastLiveAt       string
				lastSeenAt       string
			)
			if err := rows.Scan(&d.ID, &lastLat, &lastLng, &appLat, &appLng, &appLast, &appActive, &d.IsActive, &liveActive, &lastLiveAt, &lastSeenAt); err != nil {
				return nil, err
			}

			loc := EffectiveDriverLocation{
				AppLat:            appLat,
				AppLng:            appLng,
				AppLastSeenAt:     appLast,
				AppLocationActive: sql.NullInt64{Int64: int64(appActive), Valid: true},
				LastLat:           lastLat,
				LastLng:           lastLng,
			}
			eLat, eLng := GetEffectiveDriverLocation(loc)
			if eLat != 0 || eLng != 0 {
				d.LastLat, d.LastLng = eLat, eLng
			} else if lastLat.Valid && lastLng.Valid {
				d.LastLat, d.LastLng = lastLat.Float64, lastLng.Float64
			}

			// Mark driver "live" on map if either source is fresh (same 90s window).
			appFresh := appActive == 1 && appLast.Valid && appLast.String != "" && appLast.String >= cutoff
			tgFresh := liveActive == 1 && lastLiveAt != "" && lastLiveAt >= cutoff
			if appFresh || tgFresh {
				d.LiveLocationActive = 1
			} else {
				d.LiveLocationActive = 0
			}

			// For display only.
			if appFresh && appLast.Valid {
				d.LastLocationAt = appLast.String
			} else {
				d.LastLocationAt = lastSeenAt
			}
		} else {
			if err := rows.Scan(&d.ID, &d.LastLat, &d.LastLng, &d.IsActive, &d.LiveLocationActive, &d.LastLocationAt); err != nil {
				return nil, err
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListActiveRideRequestsForMap returns active ride requests with valid pickup coordinates.
func (s *AdminService) ListActiveRideRequestsForMap(ctx context.Context) ([]AdminMapRideRequest, error) {
	if s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.pickup_lat, r.pickup_lng, r.status,
			u.id, u.telegram_id, u.phone, u.name
		FROM ride_requests r
		LEFT JOIN users u ON u.id = r.rider_user_id
		WHERE r.pickup_lat IS NOT NULL AND r.pickup_lng IS NOT NULL
		  AND r.status = ?1`,
		domain.RequestStatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminMapRideRequest
	for rows.Next() {
		var r AdminMapRideRequest
		var uid sql.NullInt64
		var tid sql.NullInt64
		var phone, riderName sql.NullString
		if err := rows.Scan(&r.ID, &r.PickupLat, &r.PickupLng, &r.Status,
			&uid, &tid, &phone, &riderName); err != nil {
			return nil, err
		}
		if uid.Valid {
			r.UserID = uid.Int64
		}
		if tid.Valid {
			r.TelegramID = tid.Int64
		}
		if phone.Valid {
			r.RiderPhone = strings.TrimSpace(phone.String)
		}
		if riderName.Valid {
			r.RiderName = strings.TrimSpace(riderName.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
