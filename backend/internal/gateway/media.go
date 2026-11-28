package gateway

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/apikey"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

const maxMediaBytes = 16 << 20 // 16 MiB per parsed image / layout file

var mediaExts = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".webp": {}, ".gif": {},
}

// getMedia streams a parsed image or layout JSON from the object store. Keys
// are produced by the processing worker as
// <tenant>/<kb>/parsed/<task>/images/<name> or
// <tenant>/<kb>/parsed/<task>/layout.json; the tenant prefix is enforced so a
// caller cannot read another tenant's media.
func (g *Gateway) getMedia(c *gin.Context) {
	key := strings.TrimSpace(c.Query("key"))
	principal, _ := tenant.From(c.Request.Context())
	if principal.TenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "tenant required"})
		return
	}
	if !validMediaKey(key, principal.TenantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "media not found"})
		return
	}
	// API keys are additionally limited to their knowledge-base allow-list
	// (the kb id is the second path segment of the media key).
	if keyInfo, ok := apikey.From(c.Request.Context()); ok {
		segments := strings.Split(key, "/")
		if !keyInfo.AllowsKB(segments[1]) {
			c.JSON(http.StatusNotFound, gin.H{"error": "media not found"})
			return
		}
	}
	reader, err := g.objects.Get(c.Request.Context(), key)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "media not found"})
		return
	}
	defer reader.Close()

	contentType := mime.TypeByExtension(strings.ToLower(path.Ext(key)))
	if contentType == "" && strings.EqualFold(path.Ext(key), ".json") {
		contentType = "application/json"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "private, max-age=3600")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, io.LimitReader(reader, maxMediaBytes))
}

// validMediaKey accepts only parser-produced image keys below the caller's
// tenant, plus the single layout.json per parsed task. Traversal, nested names
// and other extensions are rejected.
func validMediaKey(key, tenantID string) bool {
	if key == "" || strings.ContainsAny(key, "\\\x00") || strings.Contains(key, "..") {
		return false
	}
	segments := strings.Split(key, "/")
	if len(segments) == 5 {
		// <tenant>/<kb>/parsed/<task>/layout.json
		if segments[0] != tenantID || segments[2] != "parsed" || segments[4] != "layout.json" {
			return false
		}
		for _, segment := range segments[1:4] {
			if !validPathSegment(segment) {
				return false
			}
		}
		return true
	}
	if len(segments) != 6 {
		return false
	}
	if segments[0] != tenantID || segments[2] != "parsed" || segments[4] != "images" {
		return false
	}
	for _, segment := range []string{segments[1], segments[3], segments[5]} {
		if !validPathSegment(segment) {
			return false
		}
	}
	_, ok := mediaExts[strings.ToLower(path.Ext(segments[5]))]
	return ok
}

func validPathSegment(segment string) bool {
	return segment != "" && segment != "." && segment != ".." && !strings.HasPrefix(segment, ".")
}
