package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/apikey"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

func newAPIKeyGateway(t *testing.T, store apikey.Store) *Gateway {
	t.Helper()
	return New(nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		"test-secret", false, WithAPIKeys(store))
}

func createTestKey(t *testing.T, store apikey.Store, allowed []string, scopes []string) (string, apikey.Key) {
	t.Helper()
	plain, prefix, err := apikey.NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	key := apikey.Key{
		TenantID:     "t1",
		Name:         "test",
		KeyPrefix:    prefix,
		KeyHash:      apikey.Hash(plain),
		AllowedKBIDs: allowed,
		Scopes:       scopes,
		RateLimitRPM: 600,
		Status:       apikey.StatusActive,
		CreatedBy:    "admin",
	}
	saved, err := store.Create(context.Background(), key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return plain, saved
}

func TestAPIKeyManagementAndRevocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := apikey.NewMemoryStore()
	g := newAPIKeyGateway(t, store)

	token, err := tenant.IssueToken("test-secret", tenant.Principal{
		TenantID: "t1", UserID: "admin", Permissions: []string{"*"},
	}, time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	rec := call(t, g.Router(), http.MethodPost, "/v1/api-keys", token, map[string]any{
		"name": "ci", "allowed_kb_ids": []string{"kb-a"}, "scopes": []string{"chat:completions"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key  string     `json:"key"`
		Item apikey.Key `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(created.Key, apikey.Prefix) {
		t.Fatalf("plaintext key = %q", created.Key)
	}
	if created.Item.Name != "ci" || created.Item.RateLimitRPM != 60 {
		t.Fatalf("issued key = %+v", created.Item)
	}

	rec = call(t, g.Router(), http.MethodGet, "/v1/api-keys", token, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ci") {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), created.Key) {
		t.Fatal("listing leaked the plaintext key")
	}

	rec = call(t, g.Router(), http.MethodDelete, "/v1/api-keys/"+created.Item.ID, token, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), apikey.StatusRevoked) {
		t.Fatalf("revoke status = %d body=%s", rec.Code, rec.Body.String())
	}

	// The revoked key must no longer authenticate.
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	w := httptest.NewRecorder()
	g.Router().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestOpenAICompletionsWithAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var received map[string]any
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"conversation_id":"","message_id":"m1","answer":"hello","citations":[{"chunk_id":"c1","content":"ctx","score":0.5,"meta":{}}],"trace_id":"tr-1","created_at":"now"}`))
	}))
	defer hub.Close()

	store := apikey.NewMemoryStore()
	g := newAPIKeyGateway(t, store)
	g.rag = newRAGProxy(hub.URL)

	plain, _ := createTestKey(t, store, nil, []string{apikey.ScopeChatCompletions, apikey.ScopeSearchUse})

	body := `{"model":"gpt-test","temperature":0.3,"max_tokens":256,"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/openai/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	g.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if received["message"] != "hi" || received["stateless"] != true {
		t.Fatalf("upstream payload = %+v", received)
	}
	if received["kb_id"] != "default" {
		t.Fatalf("kb_id = %v", received["kb_id"])
	}
	if received["system_prompt"] != "be terse" {
		t.Fatalf("system_prompt = %v", received["system_prompt"])
	}
	if received["temperature"] != 0.3 {
		t.Fatalf("temperature = %v", received["temperature"])
	}
	if received["max_tokens"] != float64(256) {
		t.Fatalf("max_tokens = %v", received["max_tokens"])
	}
	history, _ := received["history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history = %v", received["history"])
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["object"] != "chat.completion" || resp["model"] != "gpt-test" {
		t.Fatalf("response = %+v", resp)
	}
	citations, _ := resp["citations"].([]any)
	if len(citations) != 1 {
		t.Fatalf("citations = %v", resp["citations"])
	}
}

func TestOpenAIStreamWithAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/stream" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, event := range []string{
			`{"type":"sources","citations":[{"chunk_id":"c1"}]}`,
			`{"type":"token","content":"hel"}`,
			`{"type":"token","content":"lo"}`,
			`{"type":"done","answer":"hello","citations":[{"chunk_id":"c1"}]}`,
		} {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer hub.Close()

	store := apikey.NewMemoryStore()
	g := newAPIKeyGateway(t, store)
	g.rag = newRAGProxy(hub.URL)
	plain, _ := createTestKey(t, store, nil, []string{apikey.ScopeChatCompletions})

	body := `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/openai/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	g.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		"chat.completion.chunk",
		`"content":"hel"`,
		`"finish_reason":"stop"`,
		`"citations":[{"chunk_id":"c1"}]`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %q: %s", want, out)
		}
	}
}

func TestOpenAIRejectsInvalidSamplingParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := apikey.NewMemoryStore()
	g := newAPIKeyGateway(t, store)
	g.rag = newRAGProxy("http://127.0.0.1:1")
	plain, _ := createTestKey(t, store, nil, []string{apikey.ScopeChatCompletions})

	cases := []string{
		`{"messages":[{"role":"user","content":"hi"}],"temperature":2.5}`,
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":50000}`,
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":-1}`,
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":0}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/openai/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		g.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestExpiredAPIKeyRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := apikey.NewMemoryStore()
	g := newAPIKeyGateway(t, store)

	plain, prefix, err := apikey.NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if _, err := store.Create(context.Background(), apikey.Key{
		ID: "expired", TenantID: "t1", Name: "old", KeyPrefix: prefix,
		KeyHash: apikey.Hash(plain), Scopes: []string{apikey.ScopeSearchUse},
		RateLimitRPM: 60, Status: apikey.StatusActive, ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	g.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyKnowledgeBaseScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := apikey.NewMemoryStore()
	g := newAPIKeyGateway(t, store)
	// A reachable upstream is never called because the kb check rejects first.
	g.rag = newRAGProxy("http://127.0.0.1:1")

	plain, _ := createTestKey(t, store, []string{"kb-a"}, []string{apikey.ScopeChatCompletions})

	body := `{"messages":[{"role":"user","content":"hi"}],"kb_id":"kb-b"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/openai/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	g.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
