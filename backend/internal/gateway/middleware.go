package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusflow/nexusflow/backend/internal/apikey"
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

// Auth validates the bearer credential and injects the tenant principal into
// the context. A token starting with the API-key prefix is resolved against the
// key store; everything else is verified as a JWT. A nil key store or blacklist
// disables that check.
func Auth(secret string, allowAnonymous bool, blacklist revocation.Blacklist, keys apikey.Store) gin.HandlerFunc {
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
				Permissions: []string{"search:use", "kb:read"},
			}
			c.Request = c.Request.WithContext(tenant.With(c.Request.Context(), p))
			c.Next()
			return
		}

		raw := bearerToken(header)
		if keys != nil && apikey.LooksLike(raw) {
			key, err := keys.FindByHash(c.Request.Context(), apikey.Hash(raw))
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
				return
			}
			if key.Status != apikey.StatusActive {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "api key is not active"})
				return
			}
			if key.Expired(time.Now()) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "api key expired"})
				return
			}
			ctx := tenant.With(c.Request.Context(), key.Principal())
			ctx = apikey.With(ctx, key)
			c.Request = c.Request.WithContext(ctx)
			// Usage bookkeeping must not delay the request or be cancelled with it.
			go func(id, tenantID string) {
				touchCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = keys.Touch(touchCtx, tenantID, id, time.Now().UTC())
			}(key.ID, key.TenantID)
			c.Next()
			return
		}

		info, err := tenant.ParseTokenInfo(secret, raw, tenant.TokenTypeAccess)
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

// RequireAnyPermission admits callers holding at least one of the keys. It
// keeps the OpenAI-compatible endpoint usable by both API keys
// (chat:completions) and console users (search:use).
func RequireAnyPermission(perms ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, _ := tenant.From(c.Request.Context())
		for _, perm := range perms {
			if tenant.HasPermission(p, perm) {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "forbidden: requires one of " + strings.Join(perms, ", "),
		})
	}
}

// APIKeyRateLimit enforces the per-key requests-per-minute budget recorded on
// api_keys.rate_limit_rpm. JWT callers are handled by RateLimit instead.
func APIKeyRateLimit() gin.HandlerFunc {
	var mu sync.Mutex
	limiters := make(map[string]*rate.Limiter)

	return func(c *gin.Context) {
		key, ok := apikey.From(c.Request.Context())
		if !ok || key.RateLimitRPM <= 0 {
			c.Next()
			return
		}

		mu.Lock()
		lim, exists := limiters[key.ID]
		if !exists {
			burst := key.RateLimitRPM / 6
			if burst < 1 {
				burst = 1
			}
			lim = rate.NewLimiter(rate.Limit(float64(key.RateLimitRPM)/60.0), burst)
			limiters[key.ID] = lim
		}
		mu.Unlock()

		if !lim.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"message": "api key rate limit exceeded",
					"type":    "rate_limit_error",
					"code":    "rate_limit_exceeded",
				},
			})
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
