package sandbox

import (
	"context"
	"time"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

// GRPCService exposes the sandbox executor over the protobuf contract.
type GRPCService struct {
	pb.UnimplementedSandboxExecutionServiceServer
	service *Service
}

func NewGRPCService(service *Service) *GRPCService {
	return &GRPCService{service: service}
}

func (g *GRPCService) Execute(ctx context.Context, req *pb.CodeExecuteRequest) (*pb.CodeExecuteResponse, error) {
	runtime := "python:3.11"
	switch req.GetRuntime() {
	case pb.Runtime_PYTHON_311:
		runtime = "python:3.11"
	case pb.Runtime_NODE_18:
		runtime = "node:18"
	}

	resp := g.service.Execute(ctx, ExecuteRequest{
		TaskID:    req.GetTaskId(),
		NodeID:    req.GetNodeId(),
		Runtime:   runtime,
		Code:      req.GetCode(),
		Packages:  req.GetPackages(),
		Env:       req.GetEnvVars(),
		InputJSON: req.GetInputJson(),
		Timeout:   time.Duration(req.GetTimeoutSeconds()) * time.Second,
	})

	return &pb.CodeExecuteResponse{
		Success:    resp.Success,
		OutputJson: resp.OutputJSON,
		Stdout:     resp.Stdout,
		Stderr:     resp.Stderr,
		SandboxId:  resp.SandboxID,
		ErrorMsg:   resp.ErrorMsg,
	}, nil
}
