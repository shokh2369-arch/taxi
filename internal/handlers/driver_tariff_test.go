package handlers

import (
	"context"
	"testing"

	"taxi-mvp/internal/config"
)

// A driver must be able to look up the rate they are charged at. It previously
// existed only in fare_settings and the admin bot, so a driver watched their
// wallet drop with no way to see why.
func TestDriverTariff_FallsBackToConfigAndClamps(t *testing.T) {
	ctx := context.Background()

	got := driverTariffFromSettings(ctx, &config.Config{
		StartingFee: 4000, PricePerKm: 1500, CommissionPercent: 5,
	}, nil)
	if got.BaseFare != 4000 || got.Tier0To1Km != 1500 {
		t.Errorf("fare fell back wrongly: %+v", got)
	}
	if got.CommissionPercent != 5 {
		t.Errorf("commission_percent = %d, want 5", got.CommissionPercent)
	}
	if !got.CommissionCharged {
		t.Error("commission should be charged when INFINITE_DRIVER_BALANCE is off")
	}
	if got.Currency != "UZS" {
		t.Errorf("currency = %q, want UZS", got.Currency)
	}

	// When commission is globally switched off, say so rather than advertising a
	// rate that is not applied.
	off := driverTariffFromSettings(ctx, &config.Config{
		CommissionPercent: 5, InfiniteDriverBalance: true,
	}, nil)
	if off.CommissionCharged {
		t.Error("commission_charged must be false while INFINITE_DRIVER_BALANCE is on")
	}

	// A nonsensical configured value must not reach the app.
	for _, in := range []int{-10, 250} {
		c := driverTariffFromSettings(ctx, &config.Config{CommissionPercent: in}, nil)
		if c.CommissionPercent < 0 || c.CommissionPercent > 100 {
			t.Errorf("commission_percent %d not clamped, got %d", in, c.CommissionPercent)
		}
	}
}
