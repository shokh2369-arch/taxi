package utils

import (
	"math"
	"strconv"
	"strings"
)

// FormatSoM groups a so'm amount in thousands for display: 25000 -> "25 000".
// Fares here run to five and six digits, which are hard to read unseparated —
// "25000" and "250000" look alike at a glance in a chat message.
// Display only; never use for parsing or arithmetic.
func FormatSoM(amount int64) string {
	digits := strconv.FormatInt(amount, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	if len(digits) <= 3 {
		return sign + digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(digits[i : i+3])
	}
	return sign + b.String()
}

// FareFromMeters computes the distance-based part of the fare (no starting fee).
// Formula: ceil(distance_m / 1000) * price_per_km (legacy; prefer CalculateFareTiered).
func FareFromMeters(distanceM int64, pricePerKm int64) int64 {
	if distanceM <= 0 {
		return 0
	}
	km := int64(math.Ceil(float64(distanceM) / 1000))
	return km * pricePerKm
}

// FareWithStartingFee returns total fare: startingFee + ceil(distance_m/1000)*pricePerKm (legacy).
func FareWithStartingFee(distanceM int64, startingFee, pricePerKm int64) int64 {
	return startingFee + FareFromMeters(distanceM, pricePerKm)
}

// CalculateFareRounded computes fare = BASE_FARE + (distance_km × PER_KM_FARE), rounded to nearest integer (0.5 rounds up).
// Legacy single-tier; prefer CalculateFareTiered for admin-controlled tariff.
func CalculateFareRounded(baseFare, perKmFare float64, distanceKm float64) int64 {
	if distanceKm < 0 {
		distanceKm = 0
	}
	return int64(math.Round(baseFare + distanceKm*perKmFare))
}

// CalculateFareTiered computes fare using tiered pricing (admin-controlled):
//   - base_fare
//   - 0..1 km: distance * tier_0_1_km
//   - 1..2 km: distance in range * tier_1_2_km
//   - 2+ km: distance above 2 km * tier_2_plus_km
//
// Returns rounded integer (0.5 rounds up).
func CalculateFareTiered(baseFare, tier0_1, tier1_2, tier2Plus float64, distanceKm float64) int64 {
	if distanceKm < 0 {
		distanceKm = 0
	}
	sum := baseFare
	if distanceKm <= 1 {
		sum += distanceKm * tier0_1
	} else if distanceKm <= 2 {
		sum += 1*tier0_1 + (distanceKm-1)*tier1_2
	} else {
		sum += 1*tier0_1 + 1*tier1_2 + (distanceKm-2)*tier2Plus
	}
	return int64(math.Round(sum))
}
