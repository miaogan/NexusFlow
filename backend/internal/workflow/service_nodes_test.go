package workflow

import (
	"context"
	"testing"
)

func TestQueryStageNodeRunsSearchAndCarriesMeta(t *testing.T) {
	var seen QueryStageRequest
	node := QueryStageNode{Search: func(_ context.Context, req QueryStageRequest) (map[string]any, error) {
		seen = req
		return map[string]any{"chunks": []map[string]any{}}, nil
	}}
	ec := NewExecutionContext(map[string]any{"kb_id": "kb-from-input"})
	ec.SetMeta(Meta{TenantID: "t1", UserID: "u1", TraceID: "trace-1"})
	out, err := node.Run(context.Background(), ec, map[string]any{
		"query": "hello", "top_k": 7, "final_top_n": 3, "rerank_model": "rerank",
	})
	if err != nil {
		t.Fatalf("query node: %v", err)
	}
	if seen.Query != "hello" || seen.KBID != "kb-from-input" {
		t.Fatalf("request = %+v", seen)
	}
	if seen.TopK != 7 || seen.FinalTopN != 3 || seen.Rerank != "rerank" {
		t.Fatalf("search options = %+v", seen)
	}
	if seen.Meta.TenantID != "t1" || seen.Meta.TraceID != "trace-1" {
		t.Fatalf("meta = %+v", seen.Meta)
	}
	if out["query"] != "hello" || out["kb_id"] != "kb-from-input" {
		t.Fatalf("output = %+v", out)
	}
}

func TestQueryStageNodeUsesQueryFromContext(t *testing.T) {
	node := QueryStageNode{Search: func(_ context.Context, req QueryStageRequest) (map[string]any, error) {
		if req.Query != "upstream question" {
			t.Fatalf("query = %q", req.Query)
		}
		return map[string]any{}, nil
	}}
	ec := NewExecutionContext(map[string]any{"question": "upstream question"})
	if _, err := node.Run(context.Background(), ec, map[string]any{"query_from": "question"}); err != nil {
		t.Fatalf("query node: %v", err)
	}
}

func TestQueryStageNodeRequiresQuery(t *testing.T) {
	node := QueryStageNode{Search: func(context.Context, QueryStageRequest) (map[string]any, error) {
		t.Fatal("search should not be called without a query")
		return nil, nil
	}}
	if _, err := node.Run(context.Background(), NewExecutionContext(nil), nil); err == nil {
		t.Fatal("expected an error when the query is missing")
	}
}

func TestQueryStageNodeFallsBackToDeclarative(t *testing.T) {
	out, err := (QueryStageNode{}).Run(context.Background(), NewExecutionContext(nil), nil)
	if err != nil {
		t.Fatalf("declarative fallback: %v", err)
	}
	if out["mode"] != "declarative" {
		t.Fatalf("output = %+v", out)
	}
}

func TestSandboxStageNodeMapsConfig(t *testing.T) {
	var seen SandboxStageRequest
	node := SandboxStageNode{Execute: func(_ context.Context, req SandboxStageRequest) (map[string]any, error) {
		seen = req
		return map[string]any{"success": true}, nil
	}}
	ec := NewExecutionContext(nil)
	ec.SetMeta(Meta{TenantID: "t1"})
	_, err := node.Run(context.Background(), ec, map[string]any{
		"code": "print(1)", "runtime": "node", "timeout_seconds": 90,
		"packages": []any{"numpy", "pandas"},
		"env_vars": map[string]any{"MODE": "test"},
		"input":    map[string]any{"a": 1},
	})
	if err != nil {
		t.Fatalf("sandbox node: %v", err)
	}
	if seen.Code != "print(1)" || seen.Runtime != "node" || seen.TimeoutSeconds != 90 {
		t.Fatalf("request = %+v", seen)
	}
	if len(seen.Packages) != 2 || seen.EnvVars["MODE"] != "test" {
		t.Fatalf("packages/env = %+v / %+v", seen.Packages, seen.EnvVars)
	}
	if seen.InputJSON != `{"a":1}` {
		t.Fatalf("input json = %q", seen.InputJSON)
	}
}

func TestSandboxStageNodeRequiresCode(t *testing.T) {
	node := SandboxStageNode{Execute: func(context.Context, SandboxStageRequest) (map[string]any, error) {
		t.Fatal("execute should not be called without code")
		return nil, nil
	}}
	if _, err := node.Run(context.Background(), NewExecutionContext(nil), map[string]any{}); err == nil {
		t.Fatal("expected an error when code is missing")
	}
}
