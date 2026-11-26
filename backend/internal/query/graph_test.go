package query

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

func TestGraphRetrieverCallsGraphRoute(t *testing.T) {
	var got llamaIndexSearchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/search" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"chunks":[{"chunk_id":"g1","content":"graph context","score":0.7,"meta":{"file_name":"a.md"}}],"trace_id":"t1"}`)
	}))
	defer server.Close()

	retriever := NewGraphRetriever(server.URL, time.Second)
	if retriever.Name() != "graph" {
		t.Errorf("Name() = %q, want graph", retriever.Name())
	}

	chunks, err := retriever.Retrieve(context.Background(), "nexusflow 图谱", 5, map[string]string{
		"tenant_id": "t1",
		"kb_id":     "kb1",
	})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(chunks) != 1 || chunks[0].ID != "g1" || chunks[0].Source != "graph" {
		t.Fatalf("chunks = %+v, want graph chunk g1", chunks)
	}

	if got.Query != "nexusflow 图谱" || got.FinalTopN != 5 || got.KBID != "kb1" {
		t.Errorf("request = %+v", got)
	}
	if len(got.Routes) != 1 || got.Routes[0].Type != "graph" || got.Routes[0].TopK != 5 {
		t.Errorf("routes = %+v, want one graph route", got.Routes)
	}
	if _, ok := got.Filters["kb_id"]; ok {
		t.Errorf("kb_id must be sent separately, filters = %v", got.Filters)
	}
	if got.Filters["tenant_id"] != "t1" {
		t.Errorf("filters = %v, want tenant_id kept", got.Filters)
	}
}

func TestLlamaIndexRetrieverSupportsEveryRoute(t *testing.T) {
	for _, route := range []string{"vector", "bm25", "graph"} {
		t.Run(route, func(t *testing.T) {
			var got llamaIndexSearchRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"chunks":[{"chunk_id":"`+route+`-1","content":"ctx","score":0.4,"meta":{}}]}`)
			}))
			defer server.Close()

			retriever := NewLlamaIndexRetriever(route, server.URL, time.Second)
			if retriever.Name() != route {
				t.Fatalf("Name() = %q, want %q", retriever.Name(), route)
			}
			chunks, err := retriever.Retrieve(context.Background(), "q", 4, nil)
			if err != nil {
				t.Fatalf("retrieve: %v", err)
			}
			if len(chunks) != 1 || chunks[0].Source != route {
				t.Fatalf("chunks = %+v, want source %q", chunks, route)
			}
			if len(got.Routes) != 1 || got.Routes[0].Type != route || got.Routes[0].TopK != 4 {
				t.Fatalf("routes = %+v, want a single %s route", got.Routes, route)
			}
		})
	}
}

func TestLlamaIndexRetrieverAddsTenantFilter(t *testing.T) {
	var got llamaIndexSearchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"chunks":[]}`)
	}))
	defer server.Close()

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-tenant-id", "tenant-9"))
	if _, err := NewLlamaIndexRetriever("vector", server.URL, time.Second).
		Retrieve(ctx, "q", 5, map[string]string{"file_name": "a.md"}); err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if got.Filters["tenant_id"] != "tenant-9" {
		t.Errorf("tenant filter = %q, want tenant-9", got.Filters["tenant_id"])
	}
	if got.Filters["file_name"] != "a.md" {
		t.Errorf("metadata filters lost: %v", got.Filters)
	}
}

func TestGraphRetrieverFailsOpenOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "graph backend down", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	if _, err := NewGraphRetriever(server.URL, time.Second).Retrieve(
		context.Background(), "q", 5, nil,
	); err == nil {
		t.Fatal("expected an error for HTTP 503")
	}
}

func TestEngineFusesGraphRouteWithMemoryRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"chunks":[{"chunk_id":"g1","content":"nexusflow 检索图谱","score":0.7,"meta":{}}]}`)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(logger, nil)
	store := NewDocStore()
	store.Add("seed", []Chunk{{ID: "m1", Content: "nexusflow 检索图谱"}})
	engine.Register(NewMemoryRetriever("vector", store))
	engine.Register(NewGraphRetriever(server.URL, time.Second))

	res, err := engine.Search(context.Background(), SearchOptions{
		Query:     "nexusflow 检索图谱",
		FinalTopN: 5,
		Routes: []Route{
			{Type: "vector", TopK: 5},
			{Type: "graph", TopK: 5},
		},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range res.Chunks {
		ids[c.ID] = true
	}
	if !ids["m1"] || !ids["g1"] {
		t.Fatalf("fused ids = %v, want both memory and graph chunks", ids)
	}
}
