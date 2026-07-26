package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
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
	// OnSessionsRevoked, when set, is called after a new login revokes the
	// driver's previous session(s), so live WebSockets of the old device can be
	// told {"type":"session_revoked"} and closed. Must not block.
	OnSessionsRevoked func(userID int64)
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
	// One active session per driver. Without this, logging in on a second device
	// left the first session valid, so two phones posted location for the same
	// driver and fought over the same trip — and neither knew the other existed.
	// The old session's next request now fails auth, which is the signal the
	// client uses to log itself out.
	revoked := int64(0)
	if n, err := s.sessions.RevokeAllForUser(ctx, userID); err != nil {
		// Not fatal: a login that cannot revoke is still better than no login,
		// but it means two devices may briefly coexist.
		log.Printf("driver_auth: revoke previous sessions for user %d: %v", userID, err)
	} else if n > 0 {
		revoked = n
		log.Printf("driver_auth: revoked %d previous session(s) for user %d on new login", n, userID)
	}
	if _, err := s.sessions.Insert(ctx, userID, hash, now.Add(s.ttl).Unix()); err != nil {
		return nil, err
	}
	// Only after the new session is durably stored: tell the old device's live
	// sockets they were signed out (best-effort push; HTTP 401 remains the
	// authoritative signal).
	if revoked > 0 && s.OnSessionsRevoked != nil {
		s.OnSessionsRevoked(userID)
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
