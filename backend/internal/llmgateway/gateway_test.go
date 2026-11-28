package llmgateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestGatewayFailsOverToBackup(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer backup-key" {
			t.Errorf("missing backup authorization: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":20}}`))
	}))
	defer backup.Close()

	g := New(Options{
		Routes: []Route{
			{Name: "primary", BaseURL: primary.URL, APIKey: "primary-key"},
			{Name: "backup", BaseURL: backup.URL, APIKey: "backup-key"},
		},
		FailThreshold: 99,
		Logger:        testLogger(),
	})
	handler := NewHandler(g)
	rec := postChat(t, handler, map[string]any{
		"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestGatewayBreakerSkipsFailingPrimary(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	}))
	defer backup.Close()

	g := New(Options{
		Routes: []Route{
			{Name: "primary", BaseURL: primary.URL},
			{Name: "backup", BaseURL: backup.URL},
		},
		FailThreshold: 2,
		Cooldown:      time.Minute,
		Logger:        testLogger(),
	})
	handler := NewHandler(g)
	for i := 0; i < 4; i++ {
		postChat(t, handler, map[string]any{"messages": []any{map[string]any{"content": "hi"}}})
	}
	// Two failures trip the breaker; the remaining requests skip the primary.
	if primaryCalls.Load() != 2 {
		t.Fatalf("primary calls = %d, want 2", primaryCalls.Load())
	}
}

func TestGatewayQuotaRejectsAndReserves(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":10}}`))
	}))
	defer upstream.Close()

	g := New(Options{
		Routes: []Route{{Name: "primary", BaseURL: upstream.URL}},
		Quota:  NewQuota("memory", "", 10),
		Logger: testLogger(),
	})
	handler := NewHandler(g)

	// max_tokens=8 + prompt ≈ 8 tokens fits the 10-token budget.
	first := postChat(t, handler, map[string]any{
		"max_tokens": 8, "messages": []any{map[string]any{"content": "hi"}},
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	// A second identical request exceeds the remaining budget.
	second := postChat(t, handler, map[string]any{
		"max_tokens": 8, "messages": []any{map[string]any{"content": "hi"}},
	})
	if second.Code != http.StatusTooManyRequests || !strings.Contains(second.Body.String(), "quota_exceeded") {
		t.Fatalf("second status = %d body=%s", second.Code, second.Body.String())
	}
}

func TestQuotaMemoryReserveAdjust(t *testing.T) {
	quota := NewQuota("memory", "", 10)
	if ok, _ := quota.Reserve("k", 8); !ok {
		t.Fatal("first reserve should pass")
	}
	if ok, _ := quota.Reserve("k", 8); ok {
		t.Fatal("second reserve should fail")
	}
	if err := quota.Adjust("k", -3); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if ok, _ := quota.Reserve("k", 5); !ok {
		t.Fatal("reserve after adjustment should pass")
	}
}

func postChat(t *testing.T, handler http.Handler, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-tenant-id", "t1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
