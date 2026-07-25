package models

import "time"

// PaymentType is the kind of payment row.
type PaymentType string

const (
	PaymentTypeDeposit    PaymentType = "deposit"
	PaymentTypeCommission PaymentType = "commission"
	PaymentTypeAdjustment PaymentType = "adjustment"
)

// Payment is a legacy admin export row (deposits and internal commission records).
// Authoritative audit trail for promo vs cash is driver_ledger; see README accounting section.
type Payment struct {
	ID         int64       `db:"id" json:"id"`
	DriverID   int64       `db:"driver_id" json:"driver_id"`
	Amount     int64       `db:"amount" json:"amount"`
	Type       PaymentType `db:"type" json:"type"`
	Note       string      `db:"note" json:"note"`
	CreatedAt  time.Time   `db:"created_at" json:"created_at"`
	TripID     *string     `db:"trip_id" json:"-"`               // optional link to trip; not exposed in API
	TotalPrice *int64      `db:"-" json:"total_price,omitempty"` // trip total (fare) for trip-related payments; set from JOIN in ListPayments
	// CommissionDue is the fee the platform earned on this trip and
	// UncollectedAmount is the part of it the driver's wallets could not cover.
	// Amount stays "what was actually taken", so revenue and receivables are
	// separately answerable instead of being inferred from a JSON blob.
	CommissionDue     int64 `db:"commission_due" json:"commission_due,omitempty"`
	UncollectedAmount int64 `db:"uncollected_amount" json:"uncollected_amount,omitempty"`
}
