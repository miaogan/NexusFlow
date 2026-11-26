package processing

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexusflow/nexusflow/backend/internal/mineru"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

// ParseService exposes submit-only parsing; the long-running poll happens in
// the queue worker so that the gRPC call stays short.
type ParseService struct {
	pb.UnimplementedProcessingServiceServer
	client  mineru.Client
	machine state.Store
	logger  *slog.Logger
}

func NewParseService(client mineru.Client, machine state.Store, logger *slog.Logger) *ParseService {
	return &ParseService{client: client, machine: machine, logger: logger}
}

func (s *ParseService) SubmitParseTask(ctx context.Context, req *pb.SubmitParseTaskRequest) (*pb.SubmitParseTaskResponse, error) {
	if req.GetTaskId() == "" || req.GetFileUrl() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id and file_url are required")
	}
	jobID, err := s.client.Submit(ctx, req.GetTaskId(), req.GetFileUrl(), req.GetFileType())
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if _, err := s.machine.Transition(ctx, req.GetTaskId(), state.Parsing, ""); err != nil {
		s.logger.Warn("submit parse transition failed", "task_id", req.GetTaskId(), "error", err)
	}
	return &pb.SubmitParseTaskResponse{Success: true, MineruTaskId: jobID}, nil
}
