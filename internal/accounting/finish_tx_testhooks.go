package accounting

// Test-only: set to force failure inside finish transaction (same package _test.go clears after use).
var (
	testSimulatePromoGrantError    error
	testSimulateReferralGrantError error
	testSimulateCommissionError    error
)
