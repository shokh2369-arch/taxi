package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"taxi-mvp/internal/repositories"
)

// DriverAuthTokens is returned by OTP verify for the native driver app.
type DriverAuthTokens struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	DriverID    int64  `json:"driver_id"`
}

// DriverAuthTokenService issues opaque bearer tokens for drivers.
// Only the sha256 hex digest is stored (same idea as rider refresh tokens).
type DriverAuthTokenService struct {
	sessions *repositories.DriverAuthSessionsRepo
	ttl      time.Duration
	now      func() time.Time
}

// NewDriverAuthTokenService constructs a driver token service.
func NewDriverAuthTokenService(sessions *repositories.DriverAuthSessionsRepo) *DriverAuthTokenService {
	return &DriverAuthTokenService{
		sessions: sessions,
		ttl:      30 * 24 * time.Hour,
		now:      time.Now,
	}
}

// Issue creates a fresh opaque bearer token for the driver user id.
func (s *DriverAuthTokenService) Issue(ctx context.Context, userID int64) (*DriverAuthTokens, error) {
	if s == nil || s.sessions == nil {
		return nil, errors.New("driver auth: token service unavailable")
	}
	if userID <= 0 {
		return nil, errors.New("driver auth: invalid user id")
	}
	now := s.now().UTC()
	raw, err := generateDriverOpaqueToken(32)
	if err != nil {
		return nil, err
	}
	hash := driverTokenSHA256Hex(raw)
	if _, err := s.sessions.Insert(ctx, userID, hash, now.Add(s.ttl).Unix()); err != nil {
		return nil, err
	}
	return &DriverAuthTokens{
		AccessToken: raw,
		ExpiresIn:   int(s.ttl.Seconds()),
		TokenType:   "Bearer",
		DriverID:    userID,
	}, nil
}

// Verify returns the driver user id for a valid non-expired bearer token.
func (s *DriverAuthTokenService) Verify(ctx context.Context, token string) (int64, error) {
	if s == nil || s.sessions == nil {
		return 0, errors.New("driver auth: token service unavailable")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, errors.New("driver auth: empty token")
	}
	row, err := s.sessions.GetByTokenHash(ctx, driverTokenSHA256Hex(token), s.now().UTC().Unix())
	if err != nil {
		return 0, err
	}
	return row.UserID, nil
}

func driverTokenSHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func generateDriverOpaqueToken(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("driver auth: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
