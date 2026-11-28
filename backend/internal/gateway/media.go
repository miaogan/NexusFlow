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

const maxMediaBytes = 16 << 20 // 16 MiB per parsed image

var mediaExts = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".webp": {}, ".gif": {},
}

// getMedia streams a parsed image from the object store. Keys are produced by
// the processing worker as <tenant>/<kb>/parsed/<task>/images/<name>; the
// tenant prefix is enforced so a caller cannot read another tenant's media.
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
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "private, max-age=3600")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, io.LimitReader(reader, maxMediaBytes))
}

// validMediaKey accepts only parser-produced image keys below the caller's
// tenant, rejecting traversal, nested names and non-image extensions.
func validMediaKey(key, tenantID string) bool {
	if key == "" || strings.ContainsAny(key, "\\\x00") || strings.Contains(key, "..") {
		return false
	}
	segments := strings.Split(key, "/")
	if len(segments) != 6 {
		return false
	}
	if segments[0] != tenantID || segments[2] != "parsed" || segments[4] != "images" {
		return false
	}
	for _, segment := range []string{segments[1], segments[3], segments[5]} {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") {
			return false
		}
	}
	_, ok := mediaExts[strings.ToLower(path.Ext(segments[5]))]
	return ok
}
