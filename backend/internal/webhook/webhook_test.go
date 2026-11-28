package webhook

import (
	"context"
	"encoding/json"
	"errors"
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

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 4, 5*time.Millisecond, TargetPolicy{AllowPrivate: true})
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

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 2, time.Millisecond, TargetPolicy{AllowPrivate: true})
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

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 2, time.Millisecond, TargetPolicy{AllowPrivate: true})
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

func TestSendTestDelivers(t *testing.T) {
	store := NewMemoryStore()
	sub, secret := testSubscription(t, store, "t1", []string{EventTaskCompleted}, true)

	var (
		mu      sync.Mutex
		body    []byte
		sig     string
		evtType string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ = io.ReadAll(r.Body)
		sig = r.Header.Get("X-NexusFlow-Signature")
		evtType = r.Header.Get("X-NexusFlow-Event")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	sub.TargetURL = server.URL
	store.subs[sub.ID] = sub

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 2, time.Millisecond, TargetPolicy{AllowPrivate: true})
	if err := dispatcher.SendTest(context.Background(), "t1", sub.ID); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	items := waitForDeliveries(t, store, sub.ID, 1)
	if items[0].Status != StatusSuccess || items[0].EventType != EventWebhookTest {
		t.Fatalf("delivery = %+v", items[0])
	}

	mu.Lock()
	defer mu.Unlock()
	if evtType != EventWebhookTest {
		t.Fatalf("event header = %q", evtType)
	}
	if sig != Sign([]byte(secret), body) {
		t.Fatalf("signature = %q", sig)
	}
}

func TestReplayDelivery(t *testing.T) {
	store := NewMemoryStore()
	sub, _ := testSubscription(t, store, "t1", []string{EventTaskCompleted}, true)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	sub.TargetURL = target.URL
	store.subs[sub.ID] = sub

	original, err := store.CreateDelivery(context.Background(), Delivery{
		TenantID: "t1", WebhookID: sub.ID, EventType: EventTaskCompleted,
		Payload: json.RawMessage(`{"id":"evt-1","type":"task.completed","tenant_id":"t1","data":{"task_id":"task-1"}}`),
		Status:  StatusFailed, AttemptCount: 1,
	})
	if err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 2, time.Millisecond, TargetPolicy{AllowPrivate: true})
	if err := dispatcher.Replay(context.Background(), "t1", sub.ID, original.ID); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	items := waitForDeliveries(t, store, sub.ID, 2)
	var replayed *Delivery
	for i := range items {
		if items[i].ID != original.ID {
			replayed = &items[i]
		}
	}
	if replayed == nil || replayed.Status != StatusSuccess || replayed.EventType != EventTaskCompleted {
		t.Fatalf("deliveries = %+v", items)
	}
}

func TestTargetPolicyHostAllowlist(t *testing.T) {
	policy := TargetPolicy{
		AllowPrivate:  true,
		HostAllowlist: []string{"hooks.example.com", "*.trusted.test"},
	}
	ctx := context.Background()
	for _, target := range []string{
		"https://hooks.example.com/events",
		"https://a.trusted.test/events",
		"https://SUB.Trusted.Test/events",
	} {
		if err := policy.Validate(ctx, target); err != nil {
			t.Fatalf("%s rejected: %v", target, err)
		}
	}
	for _, target := range []string{
		"https://trusted.test/events", // wildcard matches subdomains only
		"https://evil.example.com/events",
		"https://evil-example.com/events",
	} {
		if err := policy.Validate(ctx, target); !errors.Is(err, ErrHostNotAllowed) {
			t.Fatalf("%s error = %v, want ErrHostNotAllowed", target, err)
		}
	}
}

func TestTargetPolicyDialRejectsNonAllowlistedHost(t *testing.T) {
	policy := TargetPolicy{
		AllowPrivate:  true,
		HostAllowlist: []string{"hooks.example.com"},
	}
	_, err := policy.DialContext(context.Background(), "tcp", "evil.example.com:443")
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("dial error = %v, want ErrHostNotAllowed", err)
	}
}

func TestReplayDeadLettersBulk(t *testing.T) {
	store := NewMemoryStore()
	sub, _ := testSubscription(t, store, "t1", []string{EventTaskCompleted}, true)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	sub.TargetURL = target.URL
	store.subs[sub.ID] = sub
	ctx := context.Background()

	var failedIDs []string
	for _, taskID := range []string{"task-1", "task-2"} {
		d, err := store.CreateDelivery(ctx, Delivery{
			TenantID: "t1", WebhookID: sub.ID, EventType: EventTaskCompleted,
			Payload: json.RawMessage(`{"id":"` + taskID + `","type":"task.completed","tenant_id":"t1","data":{"task_id":"` + taskID + `"}}`),
			Status:  StatusFailed, AttemptCount: 1,
		})
		if err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}
		failedIDs = append(failedIDs, d.ID)
	}
	if _, err := store.CreateDelivery(ctx, Delivery{
		TenantID: "t1", WebhookID: sub.ID, EventType: EventTaskCompleted,
		Payload: json.RawMessage(`{"id":"ok","type":"task.completed","tenant_id":"t1","data":{}}`),
		Status:  StatusSuccess, AttemptCount: 1,
	}); err != nil {
		t.Fatalf("CreateDelivery success: %v", err)
	}

	dispatcher := NewDispatcher(store, plainCipher{}, discardLogger(), 2, time.Millisecond, TargetPolicy{AllowPrivate: true})
	count, err := dispatcher.ReplayDeadLetters(ctx, "t1", sub.ID, 10)
	if err != nil || count != 2 {
		t.Fatalf("ReplayDeadLetters = %d, %v", count, err)
	}
	waitForDeliveries(t, store, sub.ID, 5)
	for _, id := range failedIDs {
		got, err := store.GetDelivery(ctx, "t1", sub.ID, id)
		if err != nil || got.Status != StatusReplayed {
			t.Fatalf("delivery %s = %+v, %v", id, got, err)
		}
	}
	again, err := dispatcher.ReplayDeadLetters(ctx, "t1", sub.ID, 10)
	if err != nil || again != 0 {
		t.Fatalf("second ReplayDeadLetters = %d, %v", again, err)
	}
}

func TestStoreUpdateSubscription(t *testing.T) {
	store := NewMemoryStore()
	sub, _ := testSubscription(t, store, "t1", DefaultEvents, true)
	sub.Name = "updated"
	sub.SecretEncrypted = "plain:rotated"
	updated, err := store.UpdateSubscription(context.Background(), sub)
	if err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	if updated.Name != "updated" || updated.SecretEncrypted != "plain:rotated" {
		t.Fatalf("updated = %+v", updated)
	}
	if _, err := store.UpdateSubscription(context.Background(), Subscription{
		ID: "missing", TenantID: "t1",
	}); err != ErrNotFound {
		t.Fatalf("missing update error = %v", err)
	}
}

func waitForDeliveries(t *testing.T, store *MemoryStore, hookID string, want int) []Delivery {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := store.ListDeliveries(context.Background(), "t1", hookID, 20)
		if len(items) >= want {
			return items
		}
		time.Sleep(5 * time.Millisecond)
	}
	items, _ := store.ListDeliveries(context.Background(), "t1", hookID, 20)
	t.Fatalf("deliveries = %d, want %d: %+v", len(items), want, items)
	return nil
}

func TestTargetPolicyBlocksPrivateAddresses(t *testing.T) {
	policy := TargetPolicy{}
	for _, target := range []string{
		"http://127.0.0.1:8080/hook",
		"http://[::1]:8080/hook",
		"http://10.0.0.1/hook",
		"http://169.254.169.254/latest/meta-data",
	} {
		if err := policy.Validate(context.Background(), target); err != ErrPrivateTarget {
			t.Fatalf("target %s error = %v, want ErrPrivateTarget", target, err)
		}
	}
	if err := policy.Validate(context.Background(), "https://8.8.8.8/hook"); err != nil {
		t.Fatalf("public target error = %v", err)
	}
	if err := policy.Validate(context.Background(), "ftp://example.com/hook"); err != ErrInvalidTarget {
		t.Fatalf("scheme error = %v", err)
	}

	allow := TargetPolicy{AllowPrivate: true}
	if err := allow.Validate(context.Background(), "http://127.0.0.1:8080/hook"); err != nil {
		t.Fatalf("allow-private error = %v", err)
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
