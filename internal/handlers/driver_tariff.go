package handlers

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/services"
)

// DriverTariffResponse is the fare structure and commission rate a driver is
// working under. All money values are whole so'm.
//
// Drivers could previously see a commission deducted from their wallet with no
// way to learn the rate it was calculated at — it existed only in fare_settings
// and the admin bot. Charging someone a percentage they cannot look up is a
// support burden at best and a trust problem at worst.
type DriverTariffResponse struct {
	BaseFare          int64 `json:"base_fare"`
	Tier0To1Km        int64 `json:"tier_0_1_km"`
	Tier1To2Km        int64 `json:"tier_1_2_km"`
	Tier2PlusKm       int64 `json:"tier_2_plus_km"`
	CommissionPercent int   `json:"commission_percent"`
	// CommissionCharged is false when the platform is not currently taking
	// commission at all (INFINITE_DRIVER_BALANCE), so the app can say "no
	// commission right now" instead of showing a rate that is not applied.
	CommissionCharged bool   `json:"commission_charged"`
	Currency          string `json:"currency"`
}

// DriverTariff returns the current fare tiers and commission rate.
func DriverTariff(db *sql.DB, cfg *config.Config, fareSvc *services.FareService) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		resp := driverTariffFromSettings(c.Request.Context(), cfg, fareSvc)
		if etag, unchanged := writeETag(c, resp); unchanged {
			c.AbortWithStatus(http.StatusNotModified)
			return
		} else if etag != "" {
			c.Header("ETag", etag)
		}
		c.JSON(http.StatusOK, resp)
	}
}

// driverTariffFromSettings reads the admin-controlled tariff, falling back to
// config when no fare_settings row exists.
func driverTariffFromSettings(ctx context.Context, cfg *config.Config, fareSvc *services.FareService) DriverTariffResponse {
	resp := DriverTariffResponse{Currency: "UZS"}
	if cfg != nil {
		resp.BaseFare = int64(cfg.StartingFee)
		resp.Tier0To1Km = int64(cfg.PricePerKm)
		resp.Tier1To2Km = int64(cfg.PricePerKm)
		resp.Tier2PlusKm = int64(cfg.PricePerKm)
		resp.CommissionPercent = cfg.CommissionPercent
		resp.CommissionCharged = !cfg.InfiniteDriverBalance
	}
	if fareSvc != nil {
		if s, err := fareSvc.GetFareSettings(ctx); err == nil && s != nil {
			resp.BaseFare = s.BaseFare
			resp.Tier0To1Km = s.Tier0_1Km
			resp.Tier1To2Km = s.Tier1_2Km
			resp.Tier2PlusKm = s.Tier2PlusKm
			resp.CommissionPercent = s.CommissionPercent
		}
	}
	if resp.CommissionPercent < 0 {
		resp.CommissionPercent = 0
	}
	if resp.CommissionPercent > 100 {
		resp.CommissionPercent = 100
	}
	return resp
}
