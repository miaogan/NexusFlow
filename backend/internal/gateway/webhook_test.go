package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/connector"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	"github.com/nexusflow/nexusflow/backend/internal/webhook"
)

func TestWebhookManagementAndDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu        sync.Mutex
		gotBody   []byte
		gotSig    string
		gotEvents int
	)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotEvents++
		gotSig = r.Header.Get("X-NexusFlow-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	store := webhook.NewMemoryStore()
	dispatcher := webhook.NewDispatcher(store, connector.NewCipher("webhook-test-key"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), 1, time.Millisecond)
	g := New(nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		"test-secret", false, WithWebhooks(dispatcher, "events-token"))

	admin, err := tenant.IssueToken("test-secret", tenant.Principal{
		TenantID: "t1", UserID: "admin", Permissions: []string{"*"},
	}, time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	rec := call(t, g.Router(), http.MethodPost, "/v1/webhooks", admin, map[string]any{
		"name": "ci", "target_url": target.URL, "events": []string{"task.completed"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Webhook webhook.Subscription `json:"webhook"`
		Secret  string               `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Secret == "" || created.Webhook.ID == "" {
		t.Fatalf("created = %+v", created)
	}

	rec = call(t, g.Router(), http.MethodGet, "/v1/webhooks", admin, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), created.Secret) {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Wrong token must be rejected.
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/events",
		strings.NewReader(`{"type":"task.completed","tenant_id":"t1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Token", "wrong")
	bad := httptest.NewRecorder()
	g.Router().ServeHTTP(bad, req)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d", bad.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/internal/events",
		strings.NewReader(`{"type":"task.completed","tenant_id":"t1","payload":{"task_id":"task-1"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Token", "events-token")
	rec = httptest.NewRecorder()
	g.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest status = %d body=%s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	var deliveries []webhook.Delivery
	for time.Now().Before(deadline) {
		deliveries, _ = store.ListDeliveries(context.Background(), "t1", created.Webhook.ID, 10)
		if len(deliveries) == 1 && deliveries[0].Status == webhook.StatusSuccess {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(deliveries) != 1 || deliveries[0].Status != webhook.StatusSuccess {
		t.Fatalf("deliveries = %+v", deliveries)
	}

	mu.Lock()
	if gotEvents != 1 {
		t.Fatalf("target calls = %d", gotEvents)
	}
	if gotSig != webhook.Sign([]byte(created.Secret), gotBody) {
		t.Fatalf("signature mismatch: %q", gotSig)
	}
	mu.Unlock()

	rec = call(t, g.Router(), http.MethodGet,
		"/v1/webhooks/"+created.Webhook.ID+"/deliveries", admin, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), webhook.StatusSuccess) {
		t.Fatalf("deliveries status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = call(t, g.Router(), http.MethodDelete, "/v1/webhooks/"+created.Webhook.ID, admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}
}
