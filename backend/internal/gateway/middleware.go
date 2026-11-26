package gateway

import (
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

// Auth validates the JWT and injects the tenant principal into the context.
func Auth(secret string, allowAnonymous bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			if !allowAnonymous {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
				return
			}
			p := tenant.Principal{TenantID: "default", UserID: "anonymous", Roles: []string{"reader"}}
			c.Request = c.Request.WithContext(tenant.With(c.Request.Context(), p))
			c.Next()
			return
		}

		raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		p, err := tenant.ParseToken(secret, raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		c.Request = c.Request.WithContext(tenant.With(c.Request.Context(), p))
		c.Next()
	}
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
