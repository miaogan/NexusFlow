package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const (
	defaultMaxAttempts = 5
	defaultBaseDelay   = time.Second
	maxRetryDelay      = time.Minute
	maxResponseBytes   = 2048
	deliveryTimeout    = 10 * time.Second
)

// Dispatcher fans events out to matching active subscriptions, signs each
// request with the subscription secret and retries with exponential backoff.
type Dispatcher struct {
	store       Store
	cipher      Cipher
	logger      *slog.Logger
	client      *http.Client
	maxAttempts int
	baseDelay   time.Duration
	sem         chan struct{}
}

// NewDispatcher builds a dispatcher. maxAttempts and baseDelay fall back to
// 5 and 1s when non-positive.
func NewDispatcher(store Store, cipher Cipher, logger *slog.Logger, maxAttempts int, baseDelay time.Duration) *Dispatcher {
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	if baseDelay <= 0 {
		baseDelay = defaultBaseDelay
	}
	return &Dispatcher{
		store:       store,
		cipher:      cipher,
		logger:      logger,
		client:      &http.Client{Timeout: deliveryTimeout},
		maxAttempts: maxAttempts,
		baseDelay:   baseDelay,
		sem:         make(chan struct{}, 32),
	}
}

// Store exposes the backing store for management APIs.
func (d *Dispatcher) Store() Store { return d.store }

// EncryptSecret encrypts a plaintext signing secret for storage.
func (d *Dispatcher) EncryptSecret(plain string) (string, error) {
	return d.cipher.Encrypt([]byte(plain))
}

// Publish dispatches asynchronously. It detaches from the caller context so a
// completed HTTP response never cancels in-flight deliveries.
func (d *Dispatcher) Publish(_ context.Context, evt Event) {
	if d == nil || evt.Type == "" || evt.TenantID == "" {
		return
	}
	evt = normalizeEvent(evt)
	d.enqueue(func() {
		if err := d.Dispatch(context.Background(), evt); err != nil {
			d.logger.Warn("webhook dispatch failed", "type", evt.Type, "error", err)
		}
	})
}

// SendTest delivers a synthetic webhook.test event directly to one
// subscription, bypassing its event filter.
func (d *Dispatcher) SendTest(ctx context.Context, tenantID, webhookID string) error {
	sub, err := d.store.GetSubscription(ctx, tenantID, webhookID)
	if err != nil {
		return err
	}
	evt := normalizeEvent(Event{
		Type: EventWebhookTest, TenantID: tenantID,
		Payload: map[string]any{
			"webhook_id": webhookID,
			"message":    "NexusFlow webhook test delivery",
		},
	})
	d.enqueue(func() { d.deliver(context.Background(), sub, evt) })
	return nil
}

// Replay re-delivers a stored delivery payload to its subscription. Missing
// deliveries or subscriptions return ErrNotFound.
func (d *Dispatcher) Replay(ctx context.Context, tenantID, webhookID, deliveryID string) error {
	delivery, err := d.store.GetDelivery(ctx, tenantID, webhookID, deliveryID)
	if err != nil {
		return err
	}
	sub, err := d.store.GetSubscription(ctx, tenantID, webhookID)
	if err != nil {
		return err
	}
	var envelope struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(delivery.Payload, &envelope); err != nil {
		return fmt.Errorf("webhook: decode delivery payload: %w", err)
	}
	eventType := envelope.Type
	if eventType == "" {
		eventType = delivery.EventType
	}
	evt := normalizeEvent(Event{
		Type: eventType, TenantID: delivery.TenantID, Payload: envelope.Data,
	})
	d.enqueue(func() { d.deliver(context.Background(), sub, evt) })
	return nil
}

func (d *Dispatcher) enqueue(fn func()) {
	go func() {
		d.sem <- struct{}{}
		defer func() { <-d.sem }()
		fn()
	}()
}

// Dispatch is the synchronous fan-out used by tests and the ingest endpoint
// when delivery counts matter.
func (d *Dispatcher) Dispatch(ctx context.Context, evt Event) error {
	if d == nil {
		return nil
	}
	if evt.Type == "" || evt.TenantID == "" {
		return fmt.Errorf("webhook: event type and tenant are required")
	}
	evt = normalizeEvent(evt)
	subs, err := d.store.ListActive(ctx, evt.TenantID)
	if err != nil {
		return fmt.Errorf("webhook: list subscriptions: %w", err)
	}
	for _, sub := range subs {
		if !subscribed(sub.Events, evt.Type) {
			continue
		}
		d.deliver(ctx, sub, evt)
	}
	return nil
}

func (d *Dispatcher) deliver(ctx context.Context, sub Subscription, evt Event) {
	secret, err := d.cipher.Decrypt(sub.SecretEncrypted)
	if err != nil {
		d.logger.Error("decrypt webhook secret failed", "webhook", sub.ID, "error", err)
		d.recordFailure(ctx, sub, evt, nil, 0, "decrypt secret: "+err.Error(), StatusFailed)
		return
	}

	envelope := map[string]any{
		"id":         evt.ID,
		"type":       evt.Type,
		"tenant_id":  evt.TenantID,
		"created_at": evt.CreatedAt,
		"data":       evt.Payload,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return
	}

	delivery, err := d.store.CreateDelivery(ctx, Delivery{
		ID:           uuid.NewString(),
		TenantID:     evt.TenantID,
		WebhookID:    sub.ID,
		EventType:    evt.Type,
		Payload:      append(json.RawMessage(nil), body...),
		AttemptCount: 1,
		Status:       StatusPending,
		CreatedAt:    time.Now().UTC(),
	})
	if err != nil {
		d.logger.Warn("create webhook delivery failed", "webhook", sub.ID, "error", err)
		return
	}

	delay := d.baseDelay
	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		status, respBody, postErr := d.post(ctx, sub, secret, evt, delivery.ID, body)
		delivery.AttemptCount = attempt
		delivery.HTTPStatus = status
		delivery.ResponseBody = truncate(respBody)

		if postErr == nil && status >= 200 && status < 300 {
			delivery.Status = StatusSuccess
			delivery.NextRetryAt = nil
			if err := d.store.UpdateDelivery(ctx, delivery); err != nil {
				d.logger.Warn("update webhook delivery failed", "id", delivery.ID, "error", err)
			}
			return
		}
		if postErr != nil {
			delivery.ResponseBody = truncate([]byte(postErr.Error()))
		}

		if attempt == d.maxAttempts {
			delivery.Status = StatusFailed
			delivery.NextRetryAt = nil
			if err := d.store.UpdateDelivery(ctx, delivery); err != nil {
				d.logger.Warn("update webhook delivery failed", "id", delivery.ID, "error", err)
			}
			d.logger.Warn("webhook delivery exhausted retries",
				"webhook", sub.ID, "event", evt.Type, "attempts", attempt)
			return
		}

		next := time.Now().UTC().Add(delay)
		delivery.Status = StatusPending
		delivery.NextRetryAt = &next
		if err := d.store.UpdateDelivery(ctx, delivery); err != nil {
			d.logger.Warn("update webhook delivery failed", "id", delivery.ID, "error", err)
		}
		if !sleep(ctx, delay) {
			return
		}
		delay *= 2
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
	}
}

func (d *Dispatcher) post(
	ctx context.Context,
	sub Subscription,
	secret []byte,
	evt Event,
	deliveryID string,
	body []byte,
) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.TargetURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	timestamp := fmt.Sprint(time.Now().Unix())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "NexusFlow-Webhook/1.0")
	req.Header.Set("X-NexusFlow-Event", evt.Type)
	req.Header.Set("X-NexusFlow-Delivery", deliveryID)
	req.Header.Set("X-NexusFlow-Timestamp", timestamp)
	req.Header.Set("X-NexusFlow-Signature", Sign(secret, body))

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return resp.StatusCode, respBody, nil
}

func (d *Dispatcher) recordFailure(
	ctx context.Context,
	sub Subscription,
	evt Event,
	body []byte,
	status int,
	message string,
	state string,
) {
	if len(body) == 0 {
		body = []byte("{}")
	}
	_, err := d.store.CreateDelivery(ctx, Delivery{
		ID:           uuid.NewString(),
		TenantID:     evt.TenantID,
		WebhookID:    sub.ID,
		EventType:    evt.Type,
		Payload:      append(json.RawMessage(nil), body...),
		AttemptCount: 1,
		HTTPStatus:   status,
		ResponseBody: truncate([]byte(message)),
		Status:       state,
		CreatedAt:    time.Now().UTC(),
	})
	if err != nil {
		d.logger.Warn("create webhook delivery failed", "webhook", sub.ID, "error", err)
	}
}

// Sign returns the HMAC-SHA256 signature header value for a request body.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func subscribed(events []string, event string) bool {
	for _, candidate := range events {
		if candidate == EventWildcard || candidate == event {
			return true
		}
	}
	return false
}

func normalizeEvent(evt Event) Event {
	if evt.ID == "" {
		evt.ID = uuid.NewString()
	}
	if evt.CreatedAt.IsZero() {
		evt.CreatedAt = time.Now().UTC()
	}
	return evt
}

func truncate(raw []byte) string {
	if len(raw) > maxResponseBytes {
		raw = raw[:maxResponseBytes]
	}
	return string(raw)
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
