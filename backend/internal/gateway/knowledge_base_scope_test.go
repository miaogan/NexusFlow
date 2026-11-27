package gateway

import (
	"context"
	"net/http"
	"testing"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recordingIngestionClient struct{ upload *pb.UploadFileRequest }

func (r *recordingIngestionClient) UploadFile(_ context.Context, req *pb.UploadFileRequest, _ ...grpc.CallOption) (*pb.UploadFileResponse, error) {
	r.upload = req
	return &pb.UploadFileResponse{TaskId: "task-1", Status: "PENDING", Created: true}, nil
}
func (*recordingIngestionClient) GetTaskStatus(context.Context, *pb.GetTaskStatusRequest, ...grpc.CallOption) (*pb.GetTaskStatusResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}
func (*recordingIngestionClient) ListTasks(context.Context, *pb.ListTasksRequest, ...grpc.CallOption) (*pb.ListTasksResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}
func (*recordingIngestionClient) DeleteTask(context.Context, *pb.DeleteTaskRequest, ...grpc.CallOption) (*pb.DeleteTaskResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}
func (*recordingIngestionClient) RetryTask(context.Context, *pb.RetryTaskRequest, ...grpc.CallOption) (*pb.RetryTaskResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}

type recordingQueryClient struct{ search *pb.SearchRequest }

func (r *recordingQueryClient) Search(_ context.Context, req *pb.SearchRequest, _ ...grpc.CallOption) (*pb.SearchResponse, error) {
	r.search = req
	return &pb.SearchResponse{}, nil
}
func (*recordingQueryClient) Answer(context.Context, *pb.AnswerRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.AnswerChunk], error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}

func TestGatewayNormalizesAndOwnsKnowledgeBaseScope(t *testing.T) {
	g := newIdentityGateway(t)
	ingestion := &recordingIngestionClient{}
	query := &recordingQueryClient{}
	g.ingestion = ingestion
	g.query = query
	h := g.Router()
	admin := loginToken(t, h, "admin", "admin123")

	w := call(t, h, http.MethodPost, "/v1/files", admin, map[string]any{
		"file_name": "a.txt", "file_type": "txt", "metadata": map[string]string{"kb_id": "spoofed"},
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("upload default KB: %d %s", w.Code, w.Body.String())
	}
	if ingestion.upload.GetKbId() != "default" || ingestion.upload.GetMetadata()["kb_id"] != "default" {
		t.Fatalf("upload scope not normalized: %+v metadata=%v", ingestion.upload, ingestion.upload.GetMetadata())
	}

	w = call(t, h, http.MethodPost, "/v1/search", admin, map[string]any{
		"query": "test", "filters": map[string]string{
			"kb_id": "spoofed-kb", "tenant_id": "spoofed-tenant", "file_name": "a.txt",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("search default KB: %d %s", w.Code, w.Body.String())
	}
	if query.search.GetKbId() != "default" || query.search.GetFilters()["kb_id"] != "default" || query.search.GetFilters()["tenant_id"] != "default" {
		t.Fatalf("search scope not enforced: %+v filters=%v", query.search, query.search.GetFilters())
	}
	if query.search.GetFilters()["file_name"] != "a.txt" {
		t.Fatalf("ordinary search filter lost: %v", query.search.GetFilters())
	}
}
