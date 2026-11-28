package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/grpc/metadata"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

// QueryStageRequest carries the real retrieval inputs of the query node.
type QueryStageRequest struct {
	Query     string
	KBID      string
	TopK      int32
	FinalTopN int32
	Rerank    string
	Meta      Meta
}

// QueryStageNode executes retrieval through the query service instead of
// returning a placeholder. Without a client it degrades to a declarative stage
// so DAGs remain valid on minimal deployments.
type QueryStageNode struct {
	Search func(ctx context.Context, req QueryStageRequest) (map[string]any, error)
}

func (QueryStageNode) Type() string { return "query" }

func (n QueryStageNode) Run(ctx context.Context, ec *ExecutionContext, config map[string]any) (map[string]any, error) {
	if n.Search == nil {
		return declarativeStage("query"), nil
	}
	query := strings.TrimSpace(configString(config, "query"))
	if query == "" {
		if key := configString(config, "query_from"); key != "" {
			query = contextString(ec, key)
		}
	}
	if query == "" {
		query = contextString(ec, "query")
	}
	if query == "" {
		return nil, fmt.Errorf("query node requires config.query, config.query_from or input.query")
	}
	kbID := configString(config, "kb_id")
	if kbID == "" {
		kbID = contextString(ec, "kb_id")
	}
	if kbID == "" {
		kbID = "default"
	}
	topK := configInt(config, "top_k", 20)
	finalTopN := configInt(config, "final_top_n", 5)
	out, err := n.Search(ctx, QueryStageRequest{
		Query: query, KBID: kbID, TopK: int32(topK), FinalTopN: int32(finalTopN),
		Rerank: configString(config, "rerank_model"), Meta: ec.Meta(),
	})
	if err != nil {
		return nil, err
	}
	out["query"] = query
	out["kb_id"] = kbID
	return out, nil
}

// NewQueryStageNode adapts a query-service client into a DAG node.
func NewQueryStageNode(client pb.QueryServiceClient) QueryStageNode {
	if client == nil {
		return QueryStageNode{}
	}
	return QueryStageNode{Search: func(ctx context.Context, req QueryStageRequest) (map[string]any, error) {
		ctx = withRunMetadata(ctx, req.Meta)
		res, err := client.Search(ctx, &pb.SearchRequest{
			Query: req.Query,
			Routes: []*pb.RecallRoute{
				{Type: "vector", TopK: req.TopK},
				{Type: "bm25", TopK: req.TopK},
			},
			RerankModel: req.Rerank,
			FinalTopN:   req.FinalTopN,
			KbId:        req.KBID,
		})
		if err != nil {
			return nil, fmt.Errorf("query node search: %w", err)
		}
		chunks := make([]map[string]any, 0, len(res.GetChunks()))
		for _, chunk := range res.GetChunks() {
			chunks = append(chunks, map[string]any{
				"chunk_id": chunk.GetChunkId(),
				"content":  chunk.GetContent(),
				"score":    chunk.GetScore(),
				"meta":     chunk.GetMeta(),
			})
		}
		return map[string]any{
			"chunks":         chunks,
			"count":          len(chunks),
			"recall_count":   res.GetRecallCount(),
			"rerank_applied": res.GetRerankApplied() == 1,
			"trace_id":       res.GetTraceId(),
		}, nil
	}}
}

// SandboxStageRequest carries the real execution inputs of the sandbox node.
type SandboxStageRequest struct {
	Code           string
	Runtime        string
	InputJSON      string
	Packages       []string
	EnvVars        map[string]string
	TimeoutSeconds int32
	Meta           Meta
	RunID          string
}

// SandboxStageNode executes custom code through the sandbox service.
type SandboxStageNode struct {
	Execute func(ctx context.Context, req SandboxStageRequest) (map[string]any, error)
}

func (SandboxStageNode) Type() string { return "sandbox" }

func (n SandboxStageNode) Run(ctx context.Context, ec *ExecutionContext, config map[string]any) (map[string]any, error) {
	if n.Execute == nil {
		return declarativeStage("sandbox"), nil
	}
	code := configString(config, "code")
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("sandbox node requires config.code")
	}
	runtimeName := strings.ToLower(strings.TrimSpace(configString(config, "runtime")))
	if runtimeName == "" {
		runtimeName = "python"
	}
	timeout := configInt(config, "timeout_seconds", 10)
	if timeout <= 0 {
		timeout = 10
	}
	if timeout > 300 {
		timeout = 300
	}
	inputJSON := configString(config, "input_json")
	if inputJSON == "" {
		if value, ok := config["input"]; ok && value != nil {
			if raw, err := json.Marshal(value); err == nil {
				inputJSON = string(raw)
			}
		}
	}
	out, err := n.Execute(ctx, SandboxStageRequest{
		Code: code, Runtime: runtimeName, InputJSON: inputJSON,
		Packages: configStrings(config, "packages"), EnvVars: configStringMap(config, "env_vars"),
		TimeoutSeconds: int32(timeout), Meta: ec.Meta(), RunID: ec.RunID(),
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// NewSandboxStageNode adapts a sandbox-service client into a DAG node.
func NewSandboxStageNode(client pb.SandboxExecutionServiceClient) SandboxStageNode {
	if client == nil {
		return SandboxStageNode{}
	}
	return SandboxStageNode{Execute: func(ctx context.Context, req SandboxStageRequest) (map[string]any, error) {
		runtime := pb.Runtime_PYTHON_311
		switch req.Runtime {
		case "node", "nodejs", "node18":
			runtime = pb.Runtime_NODE_18
		case "python", "python311", "python3", "":
			runtime = pb.Runtime_PYTHON_311
		default:
			return nil, fmt.Errorf("sandbox node: unsupported runtime %q", req.Runtime)
		}
		ctx = withRunMetadata(ctx, req.Meta)
		res, err := client.Execute(ctx, &pb.CodeExecuteRequest{
			TaskId: req.RunID, Runtime: runtime, Code: req.Code,
			Packages: req.Packages, EnvVars: req.EnvVars,
			InputJson: req.InputJSON, TimeoutSeconds: req.TimeoutSeconds,
		})
		if err != nil {
			return nil, fmt.Errorf("sandbox node execute: %w", err)
		}
		return map[string]any{
			"success":     res.GetSuccess(),
			"output_json": res.GetOutputJson(),
			"stdout":      res.GetStdout(),
			"stderr":      res.GetStderr(),
			"sandbox_id":  res.GetSandboxId(),
			"error_msg":   res.GetErrorMsg(),
		}, nil
	}}
}

// declarativeStage is the explicit result of a stage that only marks its place
// in the DAG (ingest/parse/index are asynchronous pipelines driven by uploads).
func declarativeStage(name string) map[string]any {
	return map[string]any{
		"stage": name, "mode": "declarative", "status": "ok",
	}
}

// withRunMetadata forwards the run identity to downstream gRPC services.
func withRunMetadata(ctx context.Context, meta Meta) context.Context {
	pairs := make([]string, 0, 6)
	if meta.TenantID != "" {
		pairs = append(pairs, "x-tenant-id", meta.TenantID)
	}
	if meta.UserID != "" {
		pairs = append(pairs, "x-user-id", meta.UserID)
	}
	if meta.TraceID != "" {
		pairs = append(pairs, "x-trace-id", meta.TraceID)
	}
	if meta.RequestID != "" {
		pairs = append(pairs, "x-request-id", meta.RequestID)
	}
	if len(pairs) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}

func configInt(config map[string]any, key string, fallback int) int {
	raw := configString(config, key)
	if raw == "" {
		return fallback
	}
	if parsed, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return parsed
	}
	if parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
		return int(parsed)
	}
	return fallback
}

func configStrings(config map[string]any, key string) []string {
	value, ok := config[key]
	if !ok || value == nil {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		parts := strings.Split(typed, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	default:
		return nil
	}
}

func configStringMap(config map[string]any, key string) map[string]string {
	value, ok := config[key]
	if !ok || value == nil {
		return nil
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprint(v)
	}
	return out
}
