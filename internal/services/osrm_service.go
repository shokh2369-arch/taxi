package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type RouteResult struct {
	DistanceMeters  float64
	DurationSeconds float64
}

type osrmRouteResponse struct {
	Routes []struct {
		Distance float64 `json:"distance"`
		Duration float64 `json:"duration"`
	} `json:"routes"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func osrmBaseURL() string {
	base := strings.TrimSpace(os.Getenv("OSRM_BASE_URL"))
	if base == "" {
		base = "https://router.project-osrm.org"
	}
	return strings.TrimRight(base, "/")
}

func osrmTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("OSRM_TIMEOUT_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 4 * time.Second
}

// osrmClient is shared across calls.
//
// A fresh http.Client with its own Transport was previously built per call, so
// no connection was ever reused — every fare estimate paid a full TCP and TLS
// handshake — and each discarded Transport kept its idle connections and their
// reader/writer goroutines alive for the IdleConnTimeout, so a burst of
// estimates leaked sockets and goroutines steadily.
var osrmClient = &http.Client{
	Timeout: osrmTimeout(),
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       30 * time.Second,
	},
}

// GetRouteDistance returns the road distance between two points.
// Prefer GetRouteDistanceContext so a disconnected caller does not hold the
// request open for the full timeout.
func GetRouteDistance(
	pickupLat, pickupLng float64,
	dropLat, dropLng float64,
) (*RouteResult, error) {
	return GetRouteDistanceContext(context.Background(), pickupLat, pickupLng, dropLat, dropLng)
}

// GetRouteDistanceContext is GetRouteDistance bound to a caller context.
func GetRouteDistanceContext(
	ctx context.Context,
	pickupLat, pickupLng float64,
	dropLat, dropLng float64,
) (*RouteResult, error) {
	// OSRM API expects lng,lat
	url := fmt.Sprintf(
		"%s/route/v1/driving/%f,%f;%f,%f?overview=false",
		osrmBaseURL(),
		pickupLng, pickupLat,
		dropLng, dropLat,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := osrmClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("osrm http status %d", resp.StatusCode)
	}

	var out osrmRouteResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Routes) == 0 {
		if out.Message != "" {
			return nil, errors.New(out.Message)
		}
		if out.Code != "" && out.Code != "Ok" {
			return nil, fmt.Errorf("osrm code %s", out.Code)
		}
		return nil, errors.New("osrm: no routes")
	}

	r := out.Routes[0]
	if r.Distance <= 0 {
		return nil, errors.New("osrm: invalid distance")
	}
	return &RouteResult{
		DistanceMeters:  r.Distance,
		DurationSeconds: r.Duration,
	}, nil
}
