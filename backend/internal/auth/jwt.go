// Package auth implements panel sessions: password hashing, JWT issuing and
// verification, refresh-token revocation, and login rate limiting.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token types distinguish access from refresh JWTs.
const (
	ClaimTypeAccess  = "access"
	ClaimTypeRefresh = "refresh"
)

// Manager issues and verifies session tokens.
type Manager struct {
	key        []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	store      RefreshStore
	now        func() time.Time
}

// RefreshStore persists refresh-token jti state for revocation.
type RefreshStore interface {
	InsertRefreshToken(jti string, userID int64, expiresAt int64) error
	GetRefreshToken(jti string) (revoked bool, expiresAt int64, err error)
	RevokeRefreshToken(jti string) error
	ReplaceRefreshToken(oldJTI, newJTI string, userID, expiresAt int64) (bool, error)
}

// NewManager builds a token manager.
func NewManager(key []byte, accessTTL, refreshTTL time.Duration, store RefreshStore) *Manager {
	return &Manager{key: key, accessTTL: accessTTL, refreshTTL: refreshTTL, store: store, now: time.Now}
}

// Errors returned by Verify / Refresh.
var (
	ErrInvalidToken = errors.New("invalid token")
	ErrRevoked      = errors.New("token revoked")
)

// Claims is the JWT payload of the panel.
type Claims struct {
	Type     string `json:"typ"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// UserID parses the subject back into the numeric user id.
func (c *Claims) UserID() int64 {
	n, _ := strconv.ParseInt(c.Subject, 10, 64)
	return n
}

// Pair is an issued access/refresh token couple.
type Pair struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

// Issue creates a fresh token pair for the user, registering the refresh jti.
func (m *Manager) Issue(userID int64, username, role string) (*Pair, error) {
	pair, jti, err := m.newPair(userID, username, role)
	if err != nil {
		return nil, err
	}
	if err := m.store.InsertRefreshToken(jti, userID, pair.RefreshExpiresAt.Unix()); err != nil {
		return nil, fmt.Errorf("register refresh token: %w", err)
	}
	return pair, nil
}

func (m *Manager) newPair(userID int64, username, role string) (*Pair, string, error) {
	now := m.now()
	accessExp := now.Add(m.accessTTL)
	refreshExp := now.Add(m.refreshTTL)
	jti, err := newJTI()
	if err != nil {
		return nil, "", err
	}
	// Both tokens refer to one revocable session, independent of reusable user IDs.
	access, err := m.signKeyed(ClaimTypeAccess, userID, username, role, accessExp, jti)
	if err != nil {
		return nil, "", err
	}
	refresh, err := m.signKeyed(ClaimTypeRefresh, userID, username, role, refreshExp, jti)
	if err != nil {
		return nil, "", err
	}
	return &Pair{AccessToken: access, RefreshToken: refresh, AccessExpiresAt: accessExp, RefreshExpiresAt: refreshExp}, jti, nil
}

// VerifyAccess validates an access JWT and returns its claims.
func (m *Manager) VerifyAccess(tokenStr string) (*Claims, error) {
	claims, err := m.verify(tokenStr, ClaimTypeAccess)
	if err != nil {
		return nil, err
	}
	revoked, expiresAt, err := m.store.GetRefreshToken(claims.ID)
	if err != nil || revoked || expiresAt <= m.now().Unix() {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// VerifyRefresh validates a refresh JWT without consuming it. Callers use it
// to inspect the jti before Rotate (which consumes the token).
func (m *Manager) VerifyRefresh(tokenStr string) (*Claims, error) {
	return m.verify(tokenStr, ClaimTypeRefresh)
}

// Rotate consumes a refresh token (revoking it) and issues a new pair.
func (m *Manager) Rotate(tokenStr string) (*Pair, error) {
	claims, err := m.verify(tokenStr, ClaimTypeRefresh)
	if err != nil {
		return nil, err
	}
	pair, jti, err := m.newPair(claims.UserID(), claims.Username, claims.Role)
	if err != nil {
		return nil, err
	}
	replaced, err := m.store.ReplaceRefreshToken(claims.ID, jti, claims.UserID(), pair.RefreshExpiresAt.Unix())
	if err != nil {
		return nil, err
	}
	if !replaced {
		return nil, ErrRevoked
	}
	return pair, nil
}

// Revoke marks the refresh token of the given jti as used.
func (m *Manager) Revoke(jti string) error { return m.store.RevokeRefreshToken(jti) }

func (m *Manager) signKeyed(typ string, userID int64, username, role string, exp time.Time, jti string) (string, error) {
	claims := Claims{
		Type:     typ,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", userID),
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(m.now()),
			Issuer:    "sing-hub",
			ID:        jti,
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.key)
}

func (m *Manager) verify(tokenStr, wantType string) (*Claims, error) {
	parser := jwt.NewParser(jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer("sing-hub"), jwt.WithExpirationRequired())
	var claims Claims
	_, err := parser.ParseWithClaims(tokenStr, &claims, func(t *jwt.Token) (any, error) {
		return m.key, nil
	})
	if err != nil {
		return nil, ErrInvalidToken
	}
	if claims.Type != wantType || claims.ID == "" || claims.UserID() <= 0 {
		return nil, ErrInvalidToken
	}
	return &claims, nil
}

func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
