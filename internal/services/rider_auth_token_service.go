package services

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"taxi-mvp/internal/repositories"
)

// RiderAuthTokens is the success payload returned by VerifyCode and Refresh.
//
// The shape (access_token / refresh_token / expires_in) matches what the
// Flutter rider app already parses, so do not rename these fields.
type RiderAuthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// RiderAuthTokenService issues and validates rider tokens.
//
//   - Access token: stateless HS256 JWT, validated without a DB hit.
//   - Refresh token: 32-byte opaque random string; only its sha256 hex digest
//     is stored in rider_auth_sessions, so leaking the DB does not let an
//     attacker present a valid refresh token.
//
// The HMAC secret is derived from the rider bot token (RIDER_BOT_TOKEN), which
// is already a high-entropy secret unique to the deployment. We hash it once
// at construction so we never feed the raw bot token into HMAC.
type RiderAuthTokenService struct {
	sessions   *repositories.RiderAuthSessionsRepo
	hmacSecret []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time
}

// NewRiderAuthTokenService constructs a token service. botToken must be the
// rider bot token (RIDER_BOT_TOKEN); it is used as HMAC keying material for
// access JWT signing and is never returned to clients.
func NewRiderAuthTokenService(sessions *repositories.RiderAuthSessionsRepo, botToken string) *RiderAuthTokenService {
	// Hash the bot token so we never sign with the raw secret.
	sum := sha256.Sum256([]byte("yettiqanot-rider-auth\x00" + botToken))
	return &RiderAuthTokenService{
		sessions:   sessions,
		hmacSecret: sum[:],
		accessTTL:  60 * time.Minute,
		refreshTTL: 30 * 24 * time.Hour,
		now:        time.Now,
	}
}

// AccessTTL returns the configured access-token lifetime. Exposed for tests.
func (s *RiderAuthTokenService) AccessTTL() time.Duration { return s.accessTTL }

// Issue generates a fresh access + refresh token pair for the given user.
// The refresh token is persisted hashed; the plaintext returned here is the
// only way the client can later present it.
func (s *RiderAuthTokenService) Issue(ctx context.Context, userID int64) (*RiderAuthTokens, error) {
	if userID <= 0 {
		return nil, errors.New("rider auth: invalid user id")
	}
	now := s.now().UTC()
	access, err := s.signAccess(userID, now)
	if err != nil {
		return nil, err
	}
	refresh, err := generateOpaqueToken(32)
	if err != nil {
		return nil, err
	}
	refreshHash := sha256Hex(refresh)
	if _, err := s.sessions.Insert(ctx, userID, refreshHash, now.Add(s.refreshTTL).Unix()); err != nil {
		return nil, err
	}
	return &RiderAuthTokens{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(s.accessTTL.Seconds()),
		TokenType:    "Bearer",
	}, nil
}

// Refresh validates the refresh token and rotates it: the presented token is
// revoked and a brand-new pair is returned. Returns an error for unknown,
// revoked, or expired tokens.
func (s *RiderAuthTokenService) Refresh(ctx context.Context, refreshToken string) (*RiderAuthTokens, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("rider auth: empty refresh token")
	}
	now := s.now().UTC()
	hash := sha256Hex(refreshToken)
	row, err := s.sessions.GetByRefreshHash(ctx, hash, now.Unix())
	if err != nil {
		return nil, err
	}
	// Rotate: revoke the old hash before issuing a new one. Even if Issue
	// fails, the old refresh token is gone — the client must re-login.
	if _, err := s.sessions.RevokeByRefreshHash(ctx, hash); err != nil {
		return nil, err
	}
	return s.Issue(ctx, row.UserID)
}

// VerifyAccess parses an access token and returns the user id if the
// signature is valid and the token has not expired.
func (s *RiderAuthTokenService) VerifyAccess(token string) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, errors.New("rider auth: empty access token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, errors.New("rider auth: malformed access token")
	}
	headerB, payloadB, sigB := parts[0], parts[1], parts[2]
	signingInput := headerB + "." + payloadB
	expectedSig := base64URLEncode(hmacSHA256([]byte(signingInput), s.hmacSecret))
	if !hmac.Equal([]byte(expectedSig), []byte(sigB)) {
		return 0, errors.New("rider auth: bad signature")
	}
	payloadJSON, err := base64URLDecode(payloadB)
	if err != nil {
		return 0, err
	}
	var p accessPayload
	if err := json.Unmarshal(payloadJSON, &p); err != nil {
		return 0, err
	}
	if p.Type != "rider_access" {
		return 0, errors.New("rider auth: wrong token type")
	}
	if p.Sub <= 0 {
		return 0, errors.New("rider auth: missing subject")
	}
	if s.now().UTC().Unix() >= p.Exp {
		return 0, errors.New("rider auth: token expired")
	}
	return p.Sub, nil
}

// RevokeAllForUser is invoked by /v1/rider/auth/logout. It revokes every
// active refresh-token row for the given user. The access token itself
// stays valid until its short TTL elapses; that is acceptable for a 1h TTL.
func (s *RiderAuthTokenService) RevokeAllForUser(ctx context.Context, userID int64) error {
	_, err := s.sessions.RevokeAllForUser(ctx, userID)
	return err
}

type accessHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type accessPayload struct {
	Sub  int64  `json:"sub"`
	Exp  int64  `json:"exp"`
	Iat  int64  `json:"iat"`
	Jti  string `json:"jti,omitempty"`
	Type string `json:"typ"`
}

func (s *RiderAuthTokenService) signAccess(userID int64, now time.Time) (string, error) {
	header := accessHeader{Alg: "HS256", Typ: "JWT"}
	jti, err := generateOpaqueToken(8)
	if err != nil {
		return "", err
	}
	payload := accessPayload{
		Sub:  userID,
		Iat:  now.Unix(),
		Exp:  now.Add(s.accessTTL).Unix(),
		Jti:  jti,
		Type: "rider_access",
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := base64URLEncode(hb) + "." + base64URLEncode(pb)
	sig := base64URLEncode(hmacSHA256([]byte(signingInput), s.hmacSecret))
	return signingInput + "." + sig, nil
}

func hmacSHA256(message, key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	return mac.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// base64URLEncode is JWT-style base64url without padding.
func base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func generateOpaqueToken(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rider auth: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
