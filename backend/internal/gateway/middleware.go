package gateway

import (
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusflow/nexusflow/backend/internal/revocation"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	"golang.org/x/time/rate"
)

// Trace attaches a trace ID to every request and echoes it back.
func Trace() gin.HandlerFunc {
	return func(c *gin.Context) {
		traceID := c.GetHeader("X-Trace-ID")
		if traceID == "" {
			traceID = uuid.NewString()
		}
		c.Set("trace_id", traceID)
		c.Header("X-Trace-ID", traceID)
		c.Next()
	}
}

// Auth validates the JWT, rejects revoked tokens and injects the tenant
// principal into the context. A nil blacklist disables the revocation check.
func Auth(secret string, allowAnonymous bool, blacklist revocation.Blacklist) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			if !allowAnonymous {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
				return
			}
			// Anonymous callers keep read-only access: they may search and use
			// retrieval-backed chat, but never manage files, users or roles.
			p := tenant.Principal{
				TenantID:    "default",
				UserID:      "anonymous",
				Roles:       []string{"reader"},
				Permissions: []string{"search:use"},
			}
			c.Request = c.Request.WithContext(tenant.With(c.Request.Context(), p))
			c.Next()
			return
		}

		info, err := tenant.ParseTokenInfo(secret, bearerToken(header), tenant.TokenTypeAccess)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		if revocation.Check(c.Request.Context(), blacklist, info.ID) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token revoked"})
			return
		}
		c.Request = c.Request.WithContext(tenant.With(c.Request.Context(), info.Principal))
		c.Next()
	}
}

// bearerToken extracts the credentials from an Authorization header.
func bearerToken(header string) string {
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

// RequirePermission gates a route on a permission key carried by the caller's
// JWT. The wildcard "*" satisfies every requirement.
func RequirePermission(perm string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, _ := tenant.From(c.Request.Context())
		if !tenant.HasPermission(p, perm) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden: requires " + perm})
			return
		}
		c.Next()
	}
}

// RateLimit applies a per-tenant token bucket at the gateway edge.
func RateLimit(rps float64, burst int) gin.HandlerFunc {
	var mu sync.Mutex
	limiters := make(map[string]*rate.Limiter)

	return func(c *gin.Context) {
		p, _ := tenant.From(c.Request.Context())
		key := p.TenantID
		if key == "" {
			key = "default"
		}

		mu.Lock()
		lim, ok := limiters[key]
		if !ok {
			lim = rate.NewLimiter(rate.Limit(rps), burst)
			limiters[key] = lim
		}
		mu.Unlock()

		if !lim.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}
