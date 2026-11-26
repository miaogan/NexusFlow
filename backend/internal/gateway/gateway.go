package gateway

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

type Gateway struct {
	ingestion pb.IngestionServiceClient
	query     pb.QueryServiceClient
	logger    *slog.Logger
	secret    string
	allowAnon bool
}

func New(ingestion pb.IngestionServiceClient, query pb.QueryServiceClient, logger *slog.Logger, secret string, allowAnon bool) *Gateway {
	return &Gateway{ingestion: ingestion, query: query, logger: logger, secret: secret, allowAnon: allowAnon}
}

func (g *Gateway) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), Trace())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/metrics", gin.WrapH(metrics.Handler()))

	v1 := r.Group("/v1", Auth(g.secret, g.allowAnon), RateLimit(50, 100))
	v1.POST("/files", g.uploadFile)
	v1.GET("/tasks/:id", g.getTask)
	v1.POST("/search", g.search)
	v1.GET("/me", g.me)

	return r
}

func (g *Gateway) callerContext(c *gin.Context) (context.Context, tenant.Principal) {
	p, _ := tenant.From(c.Request.Context())
	return tenant.ToOutgoing(c.Request.Context(), p), p
}

type uploadReq struct {
	FileName     string            `json:"file_name" binding:"required"`
	FileType     string            `json:"file_type" binding:"required"`
	FileSize     int64             `json:"file_size"`
	PresignedURL string            `json:"presigned_url"`
	Metadata     map[string]string `json:"metadata"`
}

func (g *Gateway) uploadFile(c *gin.Context) {
	var body uploadReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, p := g.callerContext(c)
	resp, err := g.ingestion.UploadFile(ctx, &pb.UploadFileRequest{
		FileName:     body.FileName,
		FileType:     body.FileType,
		FileSize:     body.FileSize,
		PresignedUrl: body.PresignedURL,
		Metadata:     body.Metadata,
	})
	if err != nil {
		g.logger.Error("upload failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"task_id": resp.GetTaskId(),
		"status":  resp.GetStatus(),
		"message": resp.GetMessage(),
	})
}

func (g *Gateway) getTask(c *gin.Context) {
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.GetTaskStatus(ctx, &pb.GetTaskStatusRequest{TaskId: c.Param("id")})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"task_id":   resp.GetTaskId(),
		"status":    resp.GetStatus(),
		"progress":  resp.GetProgress(),
		"error_msg": resp.GetErrorMsg(),
	})
}

type routeReq struct {
	Type    string `json:"type"`
	TopK    int32  `json:"top_k"`
	Filters string `json:"filter_expr"`
}

type searchReq struct {
	Query     string            `json:"query" binding:"required"`
	Routes    []routeReq        `json:"routes"`
	Rerank    string            `json:"rerank_model"`
	FinalTopN int32             `json:"final_top_n"`
	KBID      string            `json:"kb_id"`
	Filters   map[string]string `json:"filters"`
}

func (g *Gateway) search(c *gin.Context) {
	var body searchReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	routes := make([]*pb.RecallRoute, 0, len(body.Routes))
	for _, r := range body.Routes {
		routes = append(routes, &pb.RecallRoute{Type: r.Type, TopK: r.TopK, FilterExpr: r.Filters})
	}

	ctx, _ := g.callerContext(c)
	resp, err := g.query.Search(ctx, &pb.SearchRequest{
		Query:       body.Query,
		Routes:      routes,
		RerankModel: body.Rerank,
		FinalTopN:   body.FinalTopN,
		KbId:        body.KBID,
		Filters:     body.Filters,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	chunks := make([]gin.H, 0, len(resp.GetChunks()))
	for _, ch := range resp.GetChunks() {
		chunks = append(chunks, gin.H{
			"chunk_id": ch.GetChunkId(),
			"content":  ch.GetContent(),
			"score":    ch.GetScore(),
			"meta":     ch.GetMeta(),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"chunks":         chunks,
		"trace_id":       resp.GetTraceId(),
		"recall_count":   resp.GetRecallCount(),
		"rerank_applied": resp.GetRerankApplied() == 1,
	})
}

func (g *Gateway) me(c *gin.Context) {
	_, p := g.callerContext(c)
	c.JSON(http.StatusOK, gin.H{
		"tenant_id":   p.TenantID,
		"user_id":     p.UserID,
		"roles":       p.Roles,
		"departments": p.Departments,
	})
}
