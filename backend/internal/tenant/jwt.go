package tenant

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the JWT payload issued by the unified auth center.
type Claims struct {
	TenantID    string   `json:"tenant_id"`
	UserID      string   `json:"user_id"`
	Departments []string `json:"departments"`
	Roles       []string `json:"roles"`
	jwt.RegisteredClaims
}

// ParseToken validates an HS256 token and converts it into a Principal.
func ParseToken(secret, raw string) (Principal, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	}, jwt.WithExpirationRequired())
	if err != nil {
		return Principal{}, err
	}
	if !tok.Valid || claims.TenantID == "" {
		return Principal{}, errors.New("invalid token")
	}
	return Principal{
		TenantID:    claims.TenantID,
		UserID:      claims.UserID,
		Roles:       claims.Roles,
		Departments: claims.Departments,
	}, nil
}

// IssueToken is a helper for local development and tests.
func IssueToken(secret string, p Principal, ttl time.Duration) (string, error) {
	claims := Claims{
		TenantID:    p.TenantID,
		UserID:      p.UserID,
		Roles:       p.Roles,
		Departments: p.Departments,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}
