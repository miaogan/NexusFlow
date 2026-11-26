package tracing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type capturedRequest struct {
	auth string
	body map[string]any
}

func startCaptureServer(t *testing.T, status int) (*httptest.Server, *[]capturedRequest, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var reqs []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		reqs = append(reqs, capturedRequest{auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs, &mu
}

func allEvents(reqs []capturedRequest) []any {
	var out []any
	for _, r := range reqs {
		if batch, ok := r.body["batch"].([]any); ok {
			out = append(out, batch...)
		}
	}
	return out
}

func TestLangfuseTracer_IngestAndAuth(t *testing.T) {
	srv, reqs, mu := startCaptureServer(t, 200)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr := NewLangfuseTracer(srv.URL, "pk-xxx", "sk-yyy", logger)

	root, ctx := tr.Start(context.Background(), "query.search", WithInput(map[string]any{"q": "hi"}))
	child, _ := tr.Start(ctx, "recall.vector")
	child.SetOutput(map[string]any{"count": 3})
	child.End()
	root.End()
	_ = tr.AddScore(context.Background(), root.TraceID(), "user.rating", -1, "踩")

	if err := tr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]capturedRequest(nil), *reqs...)
	mu.Unlock()

	if len(got) == 0 {
		t.Fatal("no ingestion request captured")
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-xxx:sk-yyy"))
	if got[0].auth != wantAuth {
		t.Errorf("auth = %q, want %q", got[0].auth, wantAuth)
	}

	events := allEvents(got)
	var hasTrace, hasSpan, hasScore bool
	for _, e := range events {
		ev, ok := e.(map[string]any)
		if !ok {
			continue
		}
		switch ev["type"] {
		case "trace-create":
			hasTrace = true
		case "span-create":
			hasSpan = true
			body := ev["body"].(map[string]any)
			if body["name"] == "recall.vector" {
				if body["status"] != "OK" {
					t.Errorf("child span status = %v, want OK", body["status"])
				}
			}
		case "score-create":
			hasScore = true
			body := ev["body"].(map[string]any)
			if body["value"].(float64) != -1 {
				t.Errorf("score value = %v, want -1", body["value"])
			}
		}
	}
	if !hasTrace || !hasSpan || !hasScore {
		t.Fatalf("missing events: trace=%v span=%v score=%v (got %d events)", hasTrace, hasSpan, hasScore, len(events))
	}
}

func TestLangfuseTracer_FailOpenOnServerError(t *testing.T) {
	srv, _, _ := startCaptureServer(t, 500)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr := NewLangfuseTracer(srv.URL, "pk", "sk", logger)
	root, _ := tr.Start(context.Background(), "op")
	root.RecordError(context.DeadlineExceeded)
	root.End()

	done := make(chan error, 1)
	go func() { done <- tr.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown should not fail on ingestion error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown deadlocked on failing server")
	}
}
