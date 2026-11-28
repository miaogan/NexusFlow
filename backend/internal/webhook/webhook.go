// Package webhook implements tenant-scoped event subscriptions with
// HMAC-SHA256 signed deliveries, exponential backoff retries and delivery logs.
package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Delivery statuses mirror the webhook_deliveries.status enum.
const (
	StatusPending = "PENDING"
	StatusSuccess = "SUCCESS"
	StatusFailed  = "FAILED"
)

// Event types emitted by the platform.
const (
	EventTaskCompleted     = "task.completed"
	EventTaskFailed        = "task.failed"
	EventIndexCompleted    = "index.completed"
	EventWorkflowCompleted = "workflow.completed"
	EventWorkflowFailed    = "workflow.failed"
	// Node-level lifecycle events emitted by the workflow engine EventSink.
	EventWorkflowNodeStarted         = "workflow.node.started"
	EventWorkflowNodeCompleted       = "workflow.node.completed"
	EventWorkflowNodeFailed          = "workflow.node.failed"
	EventWorkflowNodeWaitingApproval = "workflow.node.waiting_approval"
	EventWorkflowNodeSkipped         = "workflow.node.skipped"
	EventApprovalDecided             = "approval.decided"
	EventWebhookTest                 = "webhook.test"

	// EventWildcard subscribes to every event type.
	EventWildcard = "*"
)

// DefaultEvents is applied when a subscription does not declare events.
var DefaultEvents = []string{EventTaskCompleted, EventTaskFailed}

// KnownEvents is the catalogue accepted at subscription time.
var KnownEvents = []string{
	EventTaskCompleted,
	EventTaskFailed,
	EventIndexCompleted,
	EventWorkflowCompleted,
	EventWorkflowFailed,
	EventWorkflowNodeStarted,
	EventWorkflowNodeCompleted,
	EventWorkflowNodeFailed,
	EventWorkflowNodeWaitingApproval,
	EventWorkflowNodeSkipped,
	EventApprovalDecided,
	EventWebhookTest,
}

var (
	// ErrNotFound is returned when a subscription does not exist for the tenant.
	ErrNotFound = errors.New("webhook not found")
	// ErrInvalidEvent is returned for an event type outside KnownEvents.
	ErrInvalidEvent = errors.New("invalid webhook event type")
	// ErrInvalidTarget is returned for a non-http(s) target URL.
	ErrInvalidTarget = errors.New("webhook target must be an http(s) URL")
	// ErrPrivateTarget is returned when a target resolves to a non-public IP.
	ErrPrivateTarget = errors.New("webhook target resolves to a private address")
)

// Subscription is a tenant-scoped webhook endpoint.
type Subscription struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id"`
	Name            string    `json:"name"`
	TargetURL       string    `json:"target_url"`
	SecretEncrypted string    `json:"-"`
	Events          []string  `json:"events"`
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Delivery is one attempt log row for a subscription/event pair.
type Delivery struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id"`
	WebhookID    string          `json:"webhook_id"`
	EventType    string          `json:"event_type"`
	Payload      json.RawMessage `json:"payload"`
	AttemptCount int             `json:"attempt_count"`
	HTTPStatus   int             `json:"http_status"`
	ResponseBody string          `json:"response_body"`
	Status       string          `json:"status"`
	NextRetryAt  *time.Time      `json:"next_retry_at"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Event is the platform event fanned out to matching subscriptions.
type Event struct {
	ID        string         `json:"id,omitempty"`
	Type      string         `json:"type"`
	TenantID  string         `json:"tenant_id"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"created_at,omitempty"`
}

// Publisher dispatches events. Implementations must be safe for concurrent use
// and return quickly (delivery happens asynchronously).
type Publisher interface {
	Publish(ctx context.Context, evt Event)
}

// Cipher encrypts webhook secrets at rest. *connector.Cipher satisfies it.
type Cipher interface {
	Encrypt([]byte) (string, error)
	Decrypt(string) ([]byte, error)
}

// Store persists subscriptions and delivery logs.
type Store interface {
	CreateSubscription(ctx context.Context, sub Subscription) (Subscription, error)
	UpdateSubscription(ctx context.Context, sub Subscription) (Subscription, error)
	ListSubscriptions(ctx context.Context, tenantID string) ([]Subscription, error)
	GetSubscription(ctx context.Context, tenantID, id string) (Subscription, error)
	DeleteSubscription(ctx context.Context, tenantID, id string) error
	ListActive(ctx context.Context, tenantID string) ([]Subscription, error)
	CreateDelivery(ctx context.Context, d Delivery) (Delivery, error)
	UpdateDelivery(ctx context.Context, d Delivery) error
	GetDelivery(ctx context.Context, tenantID, webhookID, id string) (Delivery, error)
	ListDeliveries(ctx context.Context, tenantID, webhookID string, limit int) ([]Delivery, error)
	Close() error
}

// NewStore selects a backend: memory (default) or postgres.
func NewStore(driver, dsn string) (Store, error) {
	switch driver {
	case "", "memory":
		return NewMemoryStore(), nil
	case "postgres":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return NewPostgresStore(ctx, dsn)
	default:
		return nil, &ConfigError{Driver: driver}
	}
}

// ConfigError reports an unsupported webhook backend.
type ConfigError struct{ Driver string }

func (e *ConfigError) Error() string {
	return "unsupported webhook driver: " + e.Driver
}

// NormalizeEvents trims, de-duplicates and validates subscription events. An
// empty list falls back to DefaultEvents.
func NormalizeEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return append([]string(nil), DefaultEvents...), nil
	}
	seen := make(map[string]struct{}, len(events))
	out := make([]string, 0, len(events))
	for _, raw := range events {
		event := strings.TrimSpace(raw)
		if event == "" {
			continue
		}
		if !knownEvent(event) {
			return nil, ErrInvalidEvent
		}
		if _, ok := seen[event]; ok {
			continue
		}
		seen[event] = struct{}{}
		out = append(out, event)
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultEvents...), nil
	}
	return out, nil
}

func knownEvent(event string) bool {
	if event == EventWildcard {
		return true
	}
	for _, known := range KnownEvents {
		if event == known {
			return true
		}
	}
	return false
}

// ValidateTarget ensures the endpoint is an absolute http(s) URL.
func ValidateTarget(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrInvalidTarget
	}
	return nil
}

// TargetPolicy validates and dials webhook targets, optionally rejecting
// loopback/private/link-local addresses to mitigate SSRF.
type TargetPolicy struct {
	// AllowPrivate permits private networks; intended for local development.
	AllowPrivate bool
}

// Validate checks the URL format and, unless private targets are allowed,
// resolves the host and rejects non-public addresses.
func (p TargetPolicy) Validate(ctx context.Context, raw string) error {
	if err := ValidateTarget(raw); err != nil {
		return err
	}
	if p.AllowPrivate {
		return nil
	}
	u, _ := url.Parse(strings.TrimSpace(raw))
	host := u.Hostname()
	if host == "" {
		return ErrInvalidTarget
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("webhook: resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return ErrPrivateTarget
		}
	}
	return nil
}

// DialContext resolves and validates the target on every attempt, preventing
// DNS-rebinding attacks between subscription creation and delivery.
func (p TargetPolicy) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{}
	if p.AllowPrivate {
		return dialer.DialContext(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("webhook: resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			continue
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	return nil, ErrPrivateTarget
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

// NewSecret returns a 32-byte hex signing secret.
func NewSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// HTTPPublisher forwards events to the gateway ingest endpoint. It is used by
// services that do not own the webhook store (processing worker, workflow).
type HTTPPublisher struct {
	endpoint string
	token    string
	client   *http.Client
	logger   *slog.Logger
}

// NewHTTPPublisher returns nil when either the gateway URL or token is empty,
// so callers can attach it unconditionally.
func NewHTTPPublisher(gatewayURL, token string, logger *slog.Logger) *HTTPPublisher {
	gatewayURL = strings.TrimRight(strings.TrimSpace(gatewayURL), "/")
	token = strings.TrimSpace(token)
	if gatewayURL == "" || token == "" {
		return nil
	}
	return &HTTPPublisher{
		endpoint: gatewayURL + "/v1/internal/events",
		token:    token,
		client:   &http.Client{Timeout: 10 * time.Second},
		logger:   logger,
	}
}

// Publish posts the event to the gateway asynchronously.
func (p *HTTPPublisher) Publish(_ context.Context, evt Event) {
	if p == nil || evt.Type == "" || evt.TenantID == "" {
		return
	}
	if evt.ID == "" {
		evt.ID = uuid.NewString()
	}
	if evt.CreatedAt.IsZero() {
		evt.CreatedAt = time.Now().UTC()
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return
	}
	go func() {
		req, err := http.NewRequest(http.MethodPost, p.endpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Webhook-Token", p.token)
		resp, err := p.client.Do(req)
		if err != nil {
			p.logger.Warn("publish webhook event failed", "type", evt.Type, "error", err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode >= 300 {
			p.logger.Warn("publish webhook event rejected", "type", evt.Type, "status", resp.StatusCode)
		}
	}()
}
