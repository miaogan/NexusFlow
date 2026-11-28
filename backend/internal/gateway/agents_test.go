package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCallAgentProxiesToHub(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var captured map[string]any
	var authorization string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/call" {
			t.Errorf("upstream path = %s, want /v1/call", r.URL.Path)
		}
		authorization = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"agent":"sandbox.agent","data":{"output":{"x":1}}}`))
	}))
	defer hub.Close()

	g := &Gateway{
		rag:    newRAGProxy(hub.URL),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/agents/sandbox.agent/call",
		strings.NewReader(`{"capability":"agent.call","input":{"text":"hi"}}`),
	)
	c.Request.Header.Set("Authorization", "Bearer user-token")
	c.Params = gin.Params{{Key: "name", Value: "sandbox.agent"}}

	g.callAgent(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if authorization != "Bearer user-token" {
		t.Fatalf("authorization = %q", authorization)
	}
	agent, _ := captured["agent"].(map[string]any)
	if agent["name"] != "sandbox.agent" || agent["capability"] != "agent.call" {
		t.Fatalf("agent target = %v", agent)
	}
	input, _ := agent["input"].(map[string]any)
	if input["text"] != "hi" {
		t.Fatalf("input = %v", input)
	}
}

func TestListAuditForwardsQueryOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var path, rawQuery string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, rawQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[]}`))
	}))
	defer hub.Close()

	g := &Gateway{
		rag:    newRAGProxy(hub.URL),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/audit?limit=25&action=tool.call", nil)

	g.listAudit(c)

	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/audit" || query.Get("limit") != "25" || query.Get("action") != "tool.call" || len(query) != 2 {
		t.Fatalf("upstream target = %s?%s, want a single query string", path, rawQuery)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCallAgentRequiresCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g := &Gateway{
		rag:    newRAGProxy("http://127.0.0.1:1"),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/agents/sandbox.agent/call",
		strings.NewReader(`{"input":{"text":"hi"}}`),
	)
	c.Params = gin.Params{{Key: "name", Value: "sandbox.agent"}}

	g.callAgent(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
