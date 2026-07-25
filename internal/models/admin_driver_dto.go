package models

// AdminDriverDTO is the admin-facing view of a driver with balance.
type AdminDriverDTO struct {
	DriverID     int64  `json:"driver_id"`
	Name         string `json:"name"`
	Phone        string `json:"phone"`
	CarModel     string `json:"car_model"`
	PlateNumber  string `json:"plate_number"`
	PromoBalance int64  `json:"promo_balance"` // platform promotional credit only (not withdrawable)
	CashBalance  int64  `json:"cash_balance"`  // real-wallet leg (admin top-ups; future settlement)
	// Balance is promo_balance + cash_balance for dashboards that expect one total field.
	Balance            int64  `json:"balance"`
	TotalPaid          int64  `json:"total_paid"`
	Status             string `json:"status"`              // "ACTIVE" or "INACTIVE"
	VerificationStatus string `json:"verification_status"` // pending, approved, rejected
	// Compliance from legal_acceptances matching active legal_documents (source of truth).
	DriverTermsOK bool `json:"driver_terms_ok"`
	UserTermsOK   bool `json:"user_terms_ok"`
	PrivacyOK     bool `json:"privacy_ok"`
	// Accepted document versions stored for this user (0 = no row). Display as v{N} in UI when > 0.
	DriverTermsAcceptedVersion   int `json:"driver_terms_accepted_version"`
	UserTermsAcceptedVersion     int `json:"user_terms_accepted_version"`
	PrivacyPolicyAcceptedVersion int `json:"privacy_policy_accepted_version"`
	// Legacy users.terms_accepted / drivers.terms_accepted (informational; do not use for compliance UI).
	LegacyUserTermsFlag   int `json:"legacy_terms_accepted"`
	LegacyDriverTermsFlag int `json:"legacy_driver_terms_accepted"`
}

// AdminRiderDTO is the admin-facing view of a rider with legal flags.
type AdminRiderDTO struct {
	ID          int64  `json:"id"`
	TelegramID  int64  `json:"telegram_id"`
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	UserTermsOK bool   `json:"user_terms_ok"`
	PrivacyOK   bool   `json:"privacy_ok"`
	// Stored versions in legal_acceptances (0 if none).
	UserTermsAcceptedVersion     int `json:"user_terms_accepted_version"`
	PrivacyPolicyAcceptedVersion int `json:"privacy_policy_accepted_version"`
	// Legacy users.terms_accepted (informational only).
	LegacyTermsAccepted int `json:"legacy_terms_accepted"`
	// Alias some dashboards expect alongside legacy_terms_accepted.
	TermsAccepted int `json:"terms_accepted"`
}
