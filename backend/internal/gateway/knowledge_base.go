package gateway

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/identity"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

type knowledgeBaseReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
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
		Name: body.Name, Description: body.Description,
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

// resolveKnowledgeBase validates that the requested namespace belongs to the
// authenticated tenant. Empty IDs consistently mean the tenant's default KB.
func (g *Gateway) resolveKnowledgeBase(c *gin.Context, rawID string) (string, bool) {
	if g.knowledgeBases == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "knowledge base service not configured"})
		return "", false
	}
	id := strings.TrimSpace(rawID)
	if id == "" {
		id = identity.DefaultKnowledgeBaseID
	}
	ctx, _ := g.callerContext(c)
	if _, err := g.knowledgeBases.GetKnowledgeBase(ctx, &pb.GetKnowledgeBaseRequest{Id: id}); err != nil {
		writeIdentityError(c, err)
		return "", false
	}
	return id, true
}
