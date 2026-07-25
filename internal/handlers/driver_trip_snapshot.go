package handlers

import (
	"context"
	"database/sql"

	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
	"taxi-mvp/internal/utils"
)

// DriverTripSnapshot is the trip view returned to driver clients after any state
// change, and inline on accept.
//
// It exists so a driver app never has to follow a successful action with a second
// GET /trip/:id just to learn the resulting status, fare and coordinates — that
// second round trip is what made the buttons feel slow.
//
// Status is the real trip status, so cancellations arrive as CANCELLED_BY_DRIVER
// or CANCELLED_BY_RIDER rather than a flattened "CANCELLED"; clients that only
// care about terminality should prefix-match on "CANCELLED".
type DriverTripSnapshot struct {
	TripID      string  `json:"trip_id"`
	Status      string  `json:"status"`
	PickupLat   float64 `json:"pickup_lat"`
	PickupLng   float64 `json:"pickup_lng"`
	DropoffLat  float64 `json:"dropoff_lat"`
	DropoffLng  float64 `json:"dropoff_lng"`
	FareSom     int64   `json:"fare_som"`
	DistanceKm  float64 `json:"distance_km"`
	RiderPhone  string  `json:"rider_phone,omitempty"`
	RiderName   string  `json:"rider_name,omitempty"`
	IsFinalFare bool    `json:"is_final_fare"`

	// CommissionPercent is the rate this trip is charged at, and CommissionSom is
	// the amount. Before the trip finishes the amount is an estimate derived from
	// the live fare; once FINISHED it is what was actually taken from the wallet.
	//
	// A driver could previously watch their balance drop with no way to see the
	// rate or the per-trip amount — it existed only in fare_settings and the admin
	// bot. DriverNetSom is what they keep after commission.
	CommissionPercent int   `json:"commission_percent"`
	CommissionSom     int64 `json:"commission_som"`
	DriverNetSom      int64 `json:"driver_net_som"`
	IsFinalCommission bool  `json:"is_final_commission"`
}

// loadDriverTripSnapshot builds the snapshot for one trip.
//
// Returns nil (not an error) when the trip cannot be read, so callers can fall
// back to the minimal response rather than failing an action that already
// succeeded — the state change is committed by this point either way.
func loadDriverTripSnapshot(ctx context.Context, db *sql.DB, cfg *config.Config, fareSvc *services.FareService, tripID string) *DriverTripSnapshot {
	if db == nil || tripID == "" {
		return nil
	}
	var (
		status     string
		distanceM  int64
		fareAmount sql.NullInt64
		pickupLat  sql.NullFloat64
		pickupLng  sql.NullFloat64
		dropLat    sql.NullFloat64
		dropLng    sql.NullFloat64
		riderPhone sql.NullString
		riderName  sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT t.status, COALESCE(t.distance_m, 0), t.fare_amount,
		       r.pickup_lat, r.pickup_lng, r.drop_lat, r.drop_lng,
		       u.phone, u.name
		FROM trips t
		JOIN ride_requests r ON r.id = t.request_id
		LEFT JOIN users u ON u.id = t.rider_user_id
		WHERE t.id = ?1`, tripID).
		Scan(&status, &distanceM, &fareAmount, &pickupLat, &pickupLng, &dropLat, &dropLng, &riderPhone, &riderName)
	if err != nil {
		return nil
	}

	distanceKm := float64(distanceM) / 1000
	computed := computeSnapshotFare(ctx, cfg, fareSvc, distanceKm)
	fare, finalPtr := TripFareForResponse(status, fareAmount, computed)

	pct, commission, finalCommission := tripCommission(ctx, db, cfg, fareSvc, tripID, status, fare)

	return &DriverTripSnapshot{
		TripID:            tripID,
		Status:            status,
		PickupLat:         pickupLat.Float64,
		PickupLng:         pickupLng.Float64,
		DropoffLat:        dropLat.Float64,
		DropoffLng:        dropLng.Float64,
		FareSom:           fare,
		DistanceKm:        distanceKm,
		RiderPhone:        riderPhone.String,
		RiderName:         riderName.String,
		IsFinalFare:       finalPtr != nil,
		CommissionPercent: pct,
		CommissionSom:     commission,
		DriverNetSom:      fare - commission,
		IsFinalCommission: finalCommission,
	}
}

// tripCommission returns the rate, the amount, and whether the amount is settled.
//
// For a finished trip the real charged amount is read from the payments row, so
// what the driver sees matches what left their wallet even if the rate changed
// afterwards. Before finish it is an estimate at the current rate.
func tripCommission(ctx context.Context, db *sql.DB, cfg *config.Config, fareSvc *services.FareService, tripID, status string, fare int64) (percent int, amount int64, final bool) {
	tariff := driverTariffFromSettings(ctx, cfg, fareSvc)
	percent = tariff.CommissionPercent
	if !tariff.CommissionCharged {
		return 0, 0, status == domain.TripStatusFinished
	}

	if status == domain.TripStatusFinished {
		var due sql.NullInt64
		err := db.QueryRowContext(ctx, `
			SELECT commission_due FROM payments
			WHERE trip_id = ?1 AND type = 'commission'
			ORDER BY id DESC LIMIT 1`, tripID).Scan(&due)
		if err == nil && due.Valid {
			return percent, due.Int64, true
		}
		// Older rows, or a database without migration 067: fall back to the rate.
	}
	return percent, fare * int64(percent) / 100, false
}

// computeSnapshotFare mirrors the live fare shown by GET /trip/:id, so an action
// response and a subsequent read never disagree.
func computeSnapshotFare(ctx context.Context, cfg *config.Config, fareSvc *services.FareService, distanceKm float64) int64 {
	if fareSvc != nil {
		if fare, err := fareSvc.CalculateFare(ctx, distanceKm); err == nil {
			return fare
		}
	}
	if cfg == nil {
		return 0
	}
	return utils.CalculateFareRounded(float64(cfg.StartingFee), float64(cfg.PricePerKm), distanceKm)
}

// activeTripStatuses is the set a driver can still act on.
var activeTripStatuses = []string{
	domain.TripStatusWaiting,
	domain.TripStatusArrived,
	domain.TripStatusStarted,
}
