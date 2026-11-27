package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAgentNodeCallsHubWithIdentity(t *testing.T) {
	var captured map[string]any
	var headers http.Header
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if r.URL.Path != "/v1/call" {
			t.Errorf("path = %s, want /v1/call", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"agent":"local.intent","data":{"intent":"chat"}}`))
	}))
	defer hub.Close()

	node := NewAgentNode(hub.URL, "svc-token", 0)
	ec := NewExecutionContext(nil)
	ec.SetMeta(Meta{TenantID: "t1", UserID: "u1", TraceID: "trace-1"})
	ec.Set("query.text", "from-dag")

	out, err := node.Run(context.Background(), ec, map[string]any{
		"capability": "intent.classify",
		"input":      map[string]any{"message": "${query.text}"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out["agent"] != "local.intent" {
		t.Fatalf("agent = %v", out["agent"])
	}
	if headers.Get("x-service-token") != "svc-token" || headers.Get("x-tenant-id") != "t1" {
		t.Fatalf("headers = %v", headers)
	}
	envelope, _ := captured["envelope"].(map[string]any)
	if envelope["trace_id"] != "trace-1" || envelope["tenant_id"] != "t1" {
		t.Fatalf("envelope = %v", envelope)
	}
	agent, _ := captured["agent"].(map[string]any)
	if agent["capability"] != "intent.classify" {
		t.Fatalf("agent target = %v", agent)
	}
	input, _ := agent["input"].(map[string]any)
	if input["message"] != "from-dag" {
		t.Fatalf("input interpolation failed: %v", input)
	}
}

func TestAgentNodeRequiresTarget(t *testing.T) {
	node := NewAgentNode("http://127.0.0.1:1", "", 0)
	_, err := node.Run(context.Background(), NewExecutionContext(nil), map[string]any{})
	if err == nil {
		t.Fatal("expected configuration error")
	}
}

type metaNode struct{}

func (metaNode) Type() string { return "meta" }

func (metaNode) Run(_ context.Context, ec *ExecutionContext, _ map[string]any) (map[string]any, error) {
	return map[string]any{"tenant": ec.Meta().TenantID, "trace": ec.Meta().TraceID}, nil
}

func TestHTTPRunPropagatesIdentityAndServiceToken(t *testing.T) {
	registry := NewRegistry()
	registry.Register(metaNode{})
	handler := NewHTTPHandler(NewEngine(registry, testLogger()), registry, WithServiceToken("svc"))

	body, _ := json.Marshal(HTTPRunRequest{
		DAG: DAG{Name: "t", Nodes: []NodeSpec{{ID: "a", Type: "meta"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without token status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/workflows/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-service-token", "svc")
	req.Header.Set("x-tenant-id", "t7")
	req.Header.Set("x-trace-id", "trace-7")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("x-trace-id") != "trace-7" {
		t.Fatalf("trace header = %q", rec.Header().Get("x-trace-id"))
	}
	var result RunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Output["a.tenant"] != "t7" || result.Output["a.trace"] != "trace-7" {
		t.Fatalf("meta not propagated: %v", result.Output)
	}
}
