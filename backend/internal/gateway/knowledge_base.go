package gateway

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/apikey"
	"github.com/nexusflow/nexusflow/backend/internal/identity"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

type knowledgeBaseReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// SourceKBID creates a reference KB that reuses another KB's vectors.
	SourceKBID string `json:"source_kb_id"`
}

type knowledgeBasePermissionReq struct {
	SubjectType string `json:"subject_type" binding:"required"`
	SubjectID   string `json:"subject_id" binding:"required"`
	Permission  string `json:"permission" binding:"required"`
}

func (g *Gateway) listKnowledgeBases(c *gin.Context) {
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.knowledgeBases.ListKnowledgeBases(ctx, &pb.ListKnowledgeBasesRequest{})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"knowledge_bases": res.GetKnowledgeBases()})
}

func (g *Gateway) createKnowledgeBase(c *gin.Context) {
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return
	}
	var body knowledgeBaseReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.knowledgeBases.CreateKnowledgeBase(ctx, &pb.CreateKnowledgeBaseRequest{
		Name: body.Name, Description: body.Description, SourceKbId: body.SourceKBID,
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"knowledge_base": res.GetKnowledgeBase()})
}

func (g *Gateway) updateKnowledgeBase(c *gin.Context) {
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return
	}
	var body knowledgeBaseReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.knowledgeBases.UpdateKnowledgeBase(ctx, &pb.UpdateKnowledgeBaseRequest{
		Id: c.Param("id"), Name: body.Name, Description: body.Description,
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"knowledge_base": res.GetKnowledgeBase()})
}

func (g *Gateway) deleteKnowledgeBase(c *gin.Context) {
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.knowledgeBases.DeleteKnowledgeBase(ctx, &pb.DeleteKnowledgeBaseRequest{Id: c.Param("id")})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": res.GetSuccess()})
}

// ---------- kb-scoped permissions ----------

func (g *Gateway) grantKnowledgeBasePermission(c *gin.Context) {
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return
	}
	var body knowledgeBasePermissionReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.knowledgeBases.GrantKnowledgeBasePermission(ctx, &pb.GrantKnowledgeBasePermissionRequest{
		KbId: c.Param("id"), SubjectType: body.SubjectType,
		SubjectId: body.SubjectID, Permission: body.Permission,
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"permission": res.GetPermission()})
}

func (g *Gateway) revokeKnowledgeBasePermission(c *gin.Context) {
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.knowledgeBases.RevokeKnowledgeBasePermission(ctx, &pb.RevokeKnowledgeBasePermissionRequest{
		KbId: c.Param("id"), SubjectType: c.Query("subject_type"), SubjectId: c.Query("subject_id"),
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": res.GetSuccess()})
}

func (g *Gateway) listKnowledgeBasePermissions(c *gin.Context) {
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.knowledgeBases.ListKnowledgeBasePermissions(ctx, &pb.ListKnowledgeBasePermissionsRequest{
		KbId: c.Param("id"),
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"permissions": res.GetPermissions()})
}

// Context keys recording how a logical KB maps onto vector storage.
const (
	ctxKeyKBIndexID  = "gateway.kb.index_id"
	ctxKeyKBReadOnly = "gateway.kb.read_only"
)

// resolveKnowledgeBase validates that the requested namespace exists and is
// visible to the caller, returning the caller's effective kb role. It also
// records the KB whose vectors back this namespace: reference KBs reuse their
// source collection instead of owning one.
func (g *Gateway) resolveKnowledgeBase(c *gin.Context, rawID string) (string, string, bool) {
	id := strings.TrimSpace(rawID)
	// API keys are not users in the identity store, so they carry their own
	// allow-list instead of a kb role. An empty list grants the whole tenant.
	if key, ok := apikey.From(c.Request.Context()); ok {
		if id == "" {
			if len(key.AllowedKBIDs) > 0 {
				id = key.AllowedKBIDs[0]
			} else {
				id = identity.DefaultKnowledgeBaseID
			}
		}
		if !key.AllowsKB(id) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "api key is not allowed to access knowledge base " + id,
			})
			return "", "", false
		}
		// API keys are not identity users, so they cannot use the role-checked
		// GetKnowledgeBase path. Resolve routing metadata through the internal
		// helper after the allow-list check above.
		indexID := id
		if g.knowledgeBases != nil {
			ctx, _ := g.callerContext(c)
			res, err := g.knowledgeBases.ResolveKnowledgeBase(
				ctx, &pb.ResolveKnowledgeBaseRequest{Id: id},
			)
			if err != nil {
				writeIdentityError(c, err)
				return "", "", false
			}
			if source := res.GetSourceKbId(); source != "" {
				indexID = source
			}
		}
		c.Set(ctxKeyKBIndexID, indexID)
		c.Set(ctxKeyKBReadOnly, indexID != id)
		return id, identity.KBRoleViewer, true
	}
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return "", "", false
	}
	if id == "" {
		id = identity.DefaultKnowledgeBaseID
	}
	ctx, _ := g.callerContext(c)
	res, err := g.knowledgeBases.GetKnowledgeBase(ctx, &pb.GetKnowledgeBaseRequest{Id: id})
	if err != nil {
		writeIdentityError(c, err)
		return "", "", false
	}
	kb := res.GetKnowledgeBase()
	indexID := kb.GetSourceKbId()
	if indexID == "" {
		indexID = kb.GetId()
	}
	c.Set(ctxKeyKBIndexID, indexID)
	c.Set(ctxKeyKBReadOnly, kb.GetSourceKbId() != "")
	return id, kb.GetRole(), true
}

// kbIndexID returns the KB that owns the vector/graph storage for this request.
func (g *Gateway) kbIndexID(c *gin.Context, fallback string) string {
	if value, ok := c.Get(ctxKeyKBIndexID); ok {
		if indexID, _ := value.(string); indexID != "" {
			return indexID
		}
	}
	return fallback
}

// kbReadOnly reports whether the active namespace is a reference KB.
func (g *Gateway) kbReadOnly(c *gin.Context) bool {
	value, ok := c.Get(ctxKeyKBReadOnly)
	if !ok {
		return false
	}
	readOnly, _ := value.(bool)
	return readOnly
}

// requireWritableKB rejects mutations against reference KBs with a clear error.
func (g *Gateway) requireWritableKB(c *gin.Context) bool {
	if !g.kbReadOnly(c) {
		return true
	}
	c.JSON(http.StatusConflict, gin.H{
		"error": "reference knowledge base is read-only; mutate the source knowledge base instead",
	})
	return false
}

// requireKBRole rejects callers below the minimum kb role with 403.
func requireKBRole(c *gin.Context, role, minimum string) bool {
	if !identity.RoleAtLeast(role, minimum) {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "forbidden: requires " + minimum, "role": role,
		})
		return false
	}
	return true
}
