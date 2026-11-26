package query

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRerankerGenericProtocol(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("generic protocol should not send Authorization, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}]}`)
	}))
	defer server.Close()

	reranker := NewHTTPReranker(RerankerConfig{URL: server.URL, Model: "bge", Timeout: time.Second})
	out, err := reranker.Rerank(context.Background(), "q", []Chunk{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}, 2)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 2 || out[0].ID != "b" || out[1].ID != "a" {
		t.Fatalf("order = %+v, want b,a", out)
	}
	if got["model"] != "bge" || got["query"] != "q" {
		t.Errorf("payload = %v", got)
	}
	docs, _ := got["documents"].([]any)
	if len(docs) != 2 || docs[0] != "alpha" {
		t.Errorf("documents = %v", got["documents"])
	}
}

func TestHTTPRerankerDashScopeProtocol(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want bearer key", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"results":[{"index":0,"relevance_score":0.8}]}}`)
	}))
	defer server.Close()

	reranker := NewHTTPReranker(RerankerConfig{
		URL:      server.URL,
		Model:    "qwen3.7-text-rerank",
		Protocol: RerankProtocolDashScope,
		APIKey:   "sk-test",
		Timeout:  time.Second,
	})
	out, err := reranker.Rerank(context.Background(), "怎么部署", []Chunk{{ID: "a", Content: "使用 make up"}}, 1)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 1 || out[0].ID != "a" || out[0].Score != 0.8 {
		t.Fatalf("out = %+v", out)
	}

	if got["model"] != "qwen3.7-text-rerank" {
		t.Errorf("model = %v", got["model"])
	}
	input, _ := got["input"].(map[string]any)
	if input["query"] != "怎么部署" {
		t.Errorf("input = %v", input)
	}
	if docs, _ := input["documents"].([]any); len(docs) != 1 || docs[0] != "使用 make up" {
		t.Errorf("documents = %v", input["documents"])
	}
	params, _ := got["parameters"].(map[string]any)
	if params["top_n"] != float64(1) || params["return_documents"] != false {
		t.Errorf("parameters = %v", params)
	}
}

func TestHTTPRerankerPoolTruncatesCandidates(t *testing.T) {
	var docCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		docCount = len(body.Documents)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"index":1,"relevance_score":0.5}]}`)
	}))
	defer server.Close()

	reranker := NewHTTPReranker(RerankerConfig{URL: server.URL, Pool: 2, Timeout: time.Second})
	out, err := reranker.Rerank(context.Background(), "q", []Chunk{
		{ID: "a", Content: "one"},
		{ID: "b", Content: "two"},
		{ID: "c", Content: "three"},
	}, 2)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if docCount != 2 {
		t.Errorf("documents sent = %d, want pool size 2", docCount)
	}
	if len(out) != 1 || out[0].ID != "b" {
		t.Errorf("out = %+v, want only the pooled chunk b", out)
	}
}

func TestHTTPRerankerReturnsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := NewHTTPReranker(RerankerConfig{URL: server.URL, Timeout: time.Second}).
		Rerank(context.Background(), "q", []Chunk{{ID: "a", Content: "x"}}, 1)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want HTTP 429", err)
	}
}
