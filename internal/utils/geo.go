package utils

import "math"

// PickupCoordsDispatchable reports whether pickup lat/lng are finite, in range, and not the
// common bogus (0,0) sentinel. Used on confirm/dispatch so bad client coords do not silently empty the pool.
func PickupCoordsDispatchable(lat, lng float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lng) || math.IsInf(lat, 0) || math.IsInf(lng, 0) {
		return false
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return false
	}
	if lat == 0 && lng == 0 {
		return false
	}
	return true
}

// HaversineMeters returns the great-circle distance in meters between two
// points on Earth given their latitude and longitude in degrees.
func HaversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusM = 6_371_000 // meters
	dLat := rad(lat2 - lat1)
	dLng := rad(lng2 - lng1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusM * c
}

func rad(deg float64) float64 {
	return deg * math.Pi / 180
}
