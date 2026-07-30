package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims is the JWT payload carrying the authenticated identity.
type Claims struct {
	UserID  string   `json:"uid"`
	Email   string   `json:"email"`
	Name    string   `json:"name"`
	Groups  []string `json:"groups,omitempty"`
	IsAdmin bool     `json:"adm,omitempty"`
	jwt.RegisteredClaims
}

// TokenManager signs and verifies JWTs.
type TokenManager struct {
	secret []byte
	ttl    time.Duration
}

func NewTokenManager(secret string, ttl time.Duration) *TokenManager {
	return &TokenManager{secret: []byte(secret), ttl: ttl}
}

// Issue creates a signed JWT for the given user.
func (m *TokenManager) Issue(userID uuid.UUID, email, name string, groups []string, isAdmin bool) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:  userID.String(),
		Email:   email,
		Name:    name,
		Groups:  groups,
		IsAdmin: isAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
			Subject:   userID.String(),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(m.secret)
}

// Verify parses and validates a JWT, returning its claims.
func (m *TokenManager) Verify(tokenStr string) (*Claims, error) {
	var claims Claims
	tok, err := jwt.ParseWithClaims(tokenStr, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return m.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid token")
	}
	return &claims, nil
}

// ExtractBearer pulls the token from an "Authorization: Bearer <t>" header value.
func ExtractBearer(header string) string {
	header = strings.TrimSpace(header)
	switch {
	case strings.HasPrefix(strings.ToLower(header), "bearer "):
		return strings.TrimSpace(header[7:])
	case strings.HasPrefix(strings.ToLower(header), "apikey "):
		return strings.TrimSpace(header[7:])
	default:
		return strings.TrimSpace(header)
	}
}
