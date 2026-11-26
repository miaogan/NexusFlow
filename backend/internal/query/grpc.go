package query

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

// GRPCService exposes the recall pipeline over the protobuf contract.
type GRPCService struct {
	pb.UnimplementedQueryServiceServer
	engine *Engine
}

func NewGRPCService(engine *Engine) *GRPCService {
	return &GRPCService{engine: engine}
}

func traceID(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-trace-id"); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return uuid.NewString()
}

func (s *GRPCService) Search(ctx context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	routes := make([]Route, 0, len(req.GetRoutes()))
	for _, r := range req.GetRoutes() {
		routes = append(routes, Route{Type: r.GetType(), TopK: int(r.GetTopK()), FilterExpr: r.GetFilterExpr()})
	}

	res, err := s.engine.Search(ctx, SearchOptions{
		Query:     req.GetQuery(),
		Routes:    routes,
		FinalTopN: int(req.GetFinalTopN()),
		Filters:   req.GetFilters(),
	})
	if err != nil {
		return nil, err
	}

	chunks := make([]*pb.RetrievedChunk, 0, len(res.Chunks))
	for _, c := range res.Chunks {
		chunks = append(chunks, &pb.RetrievedChunk{
			ChunkId: c.ID,
			Content: c.Content,
			Score:   float32(c.Score),
			Meta:    c.Meta,
		})
	}
	applied := int32(0)
	if res.RerankApplied {
		applied = 1
	}
	return &pb.SearchResponse{
		Chunks:        chunks,
		TraceId:       traceID(ctx),
		RecallCount:   int32(res.RecallCount),
		RerankApplied: applied,
	}, nil
}

func (s *GRPCService) Answer(req *pb.AnswerRequest, stream pb.QueryService_AnswerServer) error {
	ctx := stream.Context()
	trace := traceID(ctx)

	routes := make([]Route, 0, len(req.GetRoutes()))
	for _, r := range req.GetRoutes() {
		routes = append(routes, Route{Type: r.GetType(), TopK: int(r.GetTopK()), FilterExpr: r.GetFilterExpr()})
	}
	res, err := s.engine.Search(ctx, SearchOptions{
		Query:   req.GetQuery(),
		Routes:  routes,
		Filters: req.GetFilters(),
	})
	if err != nil {
		return err
	}

	citations := make([]*pb.RetrievedChunk, 0, len(res.Chunks))
	for _, c := range res.Chunks {
		citations = append(citations, &pb.RetrievedChunk{
			ChunkId: c.ID,
			Content: c.Content,
			Score:   float32(c.Score),
			Meta:    c.Meta,
		})
	}

	// The generation step is delegated to the LLM layer; this service only
	// assembles grounded context, so the placeholder keeps the stream shape.
	answer := fmt.Sprintf("（占位回答）已检索到 %d 个相关片段，请接入生成模型。", len(citations))
	if err := stream.Send(&pb.AnswerChunk{Delta: answer, TraceId: trace, Citations: citations}); err != nil {
		return err
	}
	return stream.Send(&pb.AnswerChunk{Done: true, TraceId: trace, Citations: citations})
}
