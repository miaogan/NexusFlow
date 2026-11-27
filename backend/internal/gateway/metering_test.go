package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nexusflow/nexusflow/backend/internal/metering"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type usageBody struct {
	Usage struct {
		StorageBytes   int64 `json:"storage_bytes"`
		FileCount      int64 `json:"file_count"`
		QueryCount     int64 `json:"query_count"`
		KnowledgeBases []struct {
			KBID         string `json:"kb_id"`
			StorageBytes int64  `json:"storage_bytes"`
		} `json:"knowledge_bases"`
	} `json:"usage"`
	Quota struct {
		MaxStorageBytes int64 `json:"max_storage_bytes"`
		MaxQueries      int64 `json:"max_queries"`
	} `json:"quota"`
	Remaining struct {
		MaxQueries int64 `json:"max_queries"`
	} `json:"remaining"`
}

func newMeteredGateway(t *testing.T, quota metering.Quota) (*Gateway, *recordingIngestionClient, *recordingQueryClient, http.Handler) {
	t.Helper()
	g := newIdentityGateway(t)
	ingestion := &recordingIngestionClient{}
	query := &recordingQueryClient{}
	g.ingestion = ingestion
	g.query = query
	g.metering = metering.NewMemoryStore()
	g.quota = quota
	return g, ingestion, query, g.Router()
}

func readUsage(t *testing.T, h http.Handler, token string) usageBody {
	t.Helper()
	w := call(t, h, http.MethodGet, "/v1/usage", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("usage: %d %s", w.Code, w.Body.String())
	}
	var body usageBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	return body
}

func TestUsageEndpointReportsQuotaAndPermissions(t *testing.T) {
	_, _, _, h := newMeteredGateway(t, metering.Quota{MaxStorageBytes: 1000, MaxFiles: 10, MaxQueries: 7})
	admin := loginToken(t, h, "admin", "admin123")

	body := readUsage(t, h, admin)
	if body.Quota.MaxStorageBytes != 1000 || body.Quota.MaxQueries != 7 || body.Remaining.MaxQueries != 7 {
		t.Fatalf("usage body = %+v", body)
	}
	if w := call(t, h, http.MethodGet, "/v1/usage", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("anonymous usage = %d, want 403", w.Code)
	}
}

func TestQueryQuotaBlocksAfterLimit(t *testing.T) {
	_, _, query, h := newMeteredGateway(t, metering.Quota{MaxQueries: 1})
	admin := loginToken(t, h, "admin", "admin123")
	payload := map[string]any{"query": "NexusFlow"}

	if w := call(t, h, http.MethodPost, "/v1/search", admin, payload); w.Code != http.StatusOK {
		t.Fatalf("first search = %d %s", w.Code, w.Body.String())
	}
	if query.search == nil {
		t.Fatal("first search did not reach the query service")
	}
	if w := call(t, h, http.MethodPost, "/v1/search", admin, payload); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second search = %d, want 429", w.Code)
	}
	if body := readUsage(t, h, admin); body.Usage.QueryCount != 1 {
		t.Fatalf("query_count = %d, want 1", body.Usage.QueryCount)
	}
}

func TestStorageQuotaBlocksUploadAndDeleteReleases(t *testing.T) {
	_, ingestion, _, h := newMeteredGateway(t, metering.Quota{MaxStorageBytes: 50, MaxFiles: 5})
	admin := loginToken(t, h, "admin", "admin123")

	tooBig := map[string]any{"file_name": "big.txt", "file_type": "txt", "file_size": 100}
	if w := call(t, h, http.MethodPost, "/v1/files", admin, tooBig); w.Code != http.StatusTooManyRequests {
		t.Fatalf("oversized upload = %d %s, want 429", w.Code, w.Body.String())
	}
	if ingestion.upload != nil {
		t.Fatal("rejected upload must not reach the ingestion service")
	}

	if w := call(t, h, http.MethodPost, "/v1/files", admin, map[string]any{
		"file_name": "ok.txt", "file_type": "txt", "file_size": 40,
	}); w.Code != http.StatusAccepted {
		t.Fatalf("accepted upload = %d %s", w.Code, w.Body.String())
	}
	body := readUsage(t, h, admin)
	if body.Usage.StorageBytes != 40 || body.Usage.FileCount != 1 {
		t.Fatalf("usage after upload = %+v", body.Usage)
	}
	if len(body.Usage.KnowledgeBases) != 1 || body.Usage.KnowledgeBases[0].KBID != "default" {
		t.Fatalf("kb breakdown = %+v", body.Usage.KnowledgeBases)
	}

	// The cascade delete subtracts the stored bytes again.
	ingestion.fileSize = 40
	if w := call(t, h, http.MethodDelete, "/v1/files/task-1?kb_id=default", admin, nil); w.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	if body := readUsage(t, h, admin); body.Usage.StorageBytes != 0 || body.Usage.FileCount != 0 {
		t.Fatalf("usage after delete = %+v", body.Usage)
	}
}

// The search path records usage only after the downstream call succeeds.
func TestFailedSearchDoesNotConsumeQuota(t *testing.T) {
	gw := newIdentityGateway(t)
	gw.query = &failingQueryClient{}
	gw.metering = metering.NewMemoryStore()
	gw.quota = metering.Quota{MaxQueries: 1}
	h := gw.Router()
	admin := loginToken(t, h, "admin", "admin123")

	if w := call(t, h, http.MethodPost, "/v1/search", admin, map[string]any{"query": "x"}); w.Code != http.StatusBadGateway {
		t.Fatalf("failed search = %d, want 502", w.Code)
	}
	if body := readUsage(t, h, admin); body.Usage.QueryCount != 0 {
		t.Fatalf("failed search consumed quota: %+v", body.Usage)
	}
}

type failingQueryClient struct{}

func (*failingQueryClient) Search(context.Context, *pb.SearchRequest, ...grpc.CallOption) (*pb.SearchResponse, error) {
	return nil, status.Error(codes.Unavailable, "query service down")
}

func (*failingQueryClient) Answer(context.Context, *pb.AnswerRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.AnswerChunk], error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}
