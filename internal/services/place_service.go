package services

import (
	"context"
	"sort"
	"strings"

	"taxi-mvp/internal/models"
	"taxi-mvp/internal/repositories"
	"taxi-mvp/internal/utils"
)

type PlaceDistance struct {
	Place      models.Place
	DistanceKm float64
}

// PlaceService provides nearest-place lookup on top of PlaceRepo.
type PlaceService struct {
	repo *repositories.PlaceRepo
}

func NewPlaceService(repo *repositories.PlaceRepo) *PlaceService {
	return &PlaceService{repo: repo}
}

// NearestWithin returns places within radiusKm (haversine), sorted by distance ascending.
func (s *PlaceService) NearestWithin(ctx context.Context, pickupLat, pickupLng, radiusKm float64) ([]PlaceDistance, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	ps, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PlaceDistance, 0, len(ps))
	for _, p := range ps {
		if strings.TrimSpace(p.Name) == "" {
			continue
		}
		km := utils.HaversineMeters(pickupLat, pickupLng, p.Lat, p.Lng) / 1000
		if radiusKm > 0 && km > radiusKm {
			continue
		}
		out = append(out, PlaceDistance{Place: p, DistanceKm: km})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DistanceKm < out[j].DistanceKm })
	return out, nil
}

