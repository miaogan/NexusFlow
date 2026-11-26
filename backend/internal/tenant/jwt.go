package tenant

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Token types carried in the "token_type" claim.
const (
	TokenTypeAccess  = "access"
	TokenTypeRefresh = "refresh"
)

// Claims is the JWT payload issued by the unified auth center.
type Claims struct {
	TenantID    string   `json:"tenant_id"`
	UserID      string   `json:"user_id"`
	Departments []string `json:"departments"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions,omitempty"`
	TokenType   string   `json:"token_type,omitempty"`
	jwt.RegisteredClaims
}

// TokenInfo is a verified token payload plus the metadata needed for
// revocation: the jti claim (ID) and the expiry.
type TokenInfo struct {
	Principal
	ID        string
	Type      string
	ExpiresAt time.Time
}

// ParseToken validates an HS256 token and converts it into a Principal.
func ParseToken(secret, raw string) (Principal, error) {
	info, err := ParseTokenInfo(secret, raw, "")
	return info.Principal, err
}

// ParseTypedToken validates the token and asserts its "token_type" claim.
// Tokens without a token_type (legacy) are accepted when expected is access.
func ParseTypedToken(secret, raw, expected string) (Principal, error) {
	info, err := ParseTokenInfo(secret, raw, expected)
	return info.Principal, err
}

// ParseTokenInfo verifies the token signature and expiry, then returns the
// principal together with the jti and expiry used by the revocation list.
func ParseTokenInfo(secret, raw, expected string) (TokenInfo, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	}, jwt.WithExpirationRequired())
	if err != nil {
		return TokenInfo{}, err
	}
	if !tok.Valid || claims.TenantID == "" {
		return TokenInfo{}, errors.New("invalid token")
	}
	typ := claims.TokenType
	if typ == "" {
		typ = TokenTypeAccess // legacy tokens are access tokens
	}
	if expected != "" && typ != expected {
		return TokenInfo{}, errors.New("unexpected token type")
	}
	info := TokenInfo{
		Principal: Principal{
			TenantID:    claims.TenantID,
			UserID:      claims.UserID,
			Roles:       claims.Roles,
			Departments: claims.Departments,
			Permissions: claims.Permissions,
		},
		ID:   claims.ID,
		Type: typ,
	}
	if claims.ExpiresAt != nil {
		info.ExpiresAt = claims.ExpiresAt.Time
	}
	return info, nil
}

// IssueToken mints an access token for local development and tests.
func IssueToken(secret string, p Principal, ttl time.Duration) (string, error) {
	return issue(secret, p, ttl, TokenTypeAccess)
}

// IssueRefreshToken mints a refresh token.
func IssueRefreshToken(secret string, p Principal, ttl time.Duration) (string, error) {
	return issue(secret, p, ttl, TokenTypeRefresh)
}

func issue(secret string, p Principal, ttl time.Duration, typ string) (string, error) {
	claims := Claims{
		TenantID:    p.TenantID,
		UserID:      p.UserID,
		Roles:       p.Roles,
		Departments: p.Departments,
		Permissions: p.Permissions,
		TokenType:   typ,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(), // jti: revocation key
			Subject:   p.UserID,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}
