package webhook

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
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type plainCipher struct{}

func (plainCipher) Encrypt(plain []byte) (string, error) { return "plain:" + string(plain), nil }
func (plainCipher) Decrypt(value string) ([]byte, error) {
	return []byte(strings.TrimPrefix(value, "plain:")), nil
}

func testSubscription(t *testing.T, store Store, tenant string, events []string, active bool) (Subscription, string) {
	t.Helper()
	secret := "s3cret"
	sub, err := store.CreateSubscription(context.Background(), Subscription{
		TenantID: tenant, Name: "sub", TargetURL: "http://example.test/hook",
		SecretEncrypted: "plain:" + secret, Events: events, IsActive: active,
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	return sub, secret
}

func TestDispatcherSignsAndRetries(t *testing.T) {
	store := NewMemoryStore()
	sub, secret := testSubscription(t, store, "t1", []string{EventTaskCompleted}, true)

	var (
		mu        sync.Mutex
		attempts  int
		signature string
		eventType string
		body      []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		signature = r.Header.Get("X-NexusFlow-Signature")
		eventType = r.Header.Get("X-NexusFlow-Event")
		body, _ = io.ReadAll(r.Body)
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	sub.TargetURL = server.URL
	// Point the stored subscription at the test server.
	store.subs[sub.ID] = sub

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 4, 5*time.Millisecond)
	dispatcher.Dispatch(context.Background(), Event{
		Type: EventTaskCompleted, TenantID: "t1",
		Payload: map[string]any{"task_id": "task-1"},
	})

	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if eventType != EventTaskCompleted {
		t.Fatalf("event header = %q", eventType)
	}
	if signature != Sign([]byte(secret), body) {
		t.Fatalf("signature = %q, want %q", signature, Sign([]byte(secret), body))
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if envelope["type"] != EventTaskCompleted || envelope["tenant_id"] != "t1" {
		t.Fatalf("envelope = %v", envelope)
	}
	data, _ := envelope["data"].(map[string]any)
	if data["task_id"] != "task-1" {
		t.Fatalf("data = %v", envelope["data"])
	}

	deliveries, err := store.ListDeliveries(context.Background(), "t1", sub.ID, 10)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %v, %v", deliveries, err)
	}
	if deliveries[0].Status != StatusSuccess || deliveries[0].AttemptCount != 3 {
		t.Fatalf("delivery = %+v", deliveries[0])
	}
}

func TestDispatcherMarksExhaustedDeliveryFailed(t *testing.T) {
	store := NewMemoryStore()
	sub, _ := testSubscription(t, store, "t1", []string{EventTaskFailed}, true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	sub.TargetURL = server.URL
	store.subs[sub.ID] = sub

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 2, time.Millisecond)
	dispatcher.Dispatch(context.Background(), Event{Type: EventTaskFailed, TenantID: "t1"})

	deliveries, _ := store.ListDeliveries(context.Background(), "t1", sub.ID, 10)
	if len(deliveries) != 1 || deliveries[0].Status != StatusFailed {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	if deliveries[0].AttemptCount != 2 || deliveries[0].HTTPStatus != http.StatusBadGateway {
		t.Fatalf("delivery = %+v", deliveries[0])
	}
}

func TestDispatchFiltersEventsAndTenant(t *testing.T) {
	store := NewMemoryStore()
	active, _ := testSubscription(t, store, "t1", []string{EventWorkflowCompleted}, true)
	inactive, _ := testSubscription(t, store, "t1", []string{EventWildcard}, false)
	other, _ := testSubscription(t, store, "t2", []string{EventWildcard}, true)

	called := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called[r.URL.Path]++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	for _, sub := range []Subscription{active, inactive, other} {
		sub.TargetURL = server.URL + "/" + sub.ID
		store.subs[sub.ID] = sub
	}

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 2, time.Millisecond)
	dispatcher.Dispatch(context.Background(), Event{Type: EventTaskCompleted, TenantID: "t1"})

	if called["/"+active.ID] != 0 || called["/"+inactive.ID] != 0 || called["/"+other.ID] != 0 {
		t.Fatalf("unexpected deliveries: %v", called)
	}

	dispatcher.Dispatch(context.Background(), Event{Type: EventWorkflowCompleted, TenantID: "t1"})
	if called["/"+active.ID] != 1 {
		t.Fatalf("matching subscription calls = %v", called)
	}
	if called["/"+inactive.ID] != 0 || called["/"+other.ID] != 0 {
		t.Fatalf("inactive/other tenant called: %v", called)
	}
}

func TestStoreDeleteRemovesDeliveries(t *testing.T) {
	store := NewMemoryStore()
	sub, _ := testSubscription(t, store, "t1", DefaultEvents, true)
	if _, err := store.CreateDelivery(context.Background(), Delivery{
		TenantID: "t1", WebhookID: sub.ID, EventType: EventTaskCompleted,
		Payload: json.RawMessage(`{}`), Status: StatusPending,
	}); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}
	if err := store.DeleteSubscription(context.Background(), "t1", sub.ID); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	deliveries, _ := store.ListDeliveries(context.Background(), "t1", sub.ID, 10)
	if len(deliveries) != 0 {
		t.Fatalf("deliveries after delete = %v", deliveries)
	}
	if _, err := store.GetSubscription(context.Background(), "t1", sub.ID); err != ErrNotFound {
		t.Fatalf("get after delete error = %v", err)
	}
}

func TestNormalizeEventsAndTarget(t *testing.T) {
	events, err := NormalizeEvents(nil)
	if err != nil || len(events) != 2 {
		t.Fatalf("defaults = %v, %v", events, err)
	}
	events, err = NormalizeEvents([]string{" task.completed ", "task.completed", "*"})
	if err != nil || len(events) != 2 {
		t.Fatalf("normalize = %v, %v", events, err)
	}
	if _, err := NormalizeEvents([]string{"nope"}); err != ErrInvalidEvent {
		t.Fatalf("invalid event error = %v", err)
	}
	if err := ValidateTarget("ftp://example.com"); err != ErrInvalidTarget {
		t.Fatalf("invalid target error = %v", err)
	}
	if err := ValidateTarget("https://example.com/hook"); err != nil {
		t.Fatalf("valid target error = %v", err)
	}
}
