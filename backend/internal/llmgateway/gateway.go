// Package llmgateway is an OpenAI-compatible LLM router: it forwards chat
// completions and embeddings to a primary/backup provider, trips a circuit
// breaker on repeated failures, and enforces a per-tenant daily token quota.
package llmgateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Route is one upstream OpenAI-compatible provider.
type Route struct {
	Name    string
	BaseURL string
	APIKey  string
	// Model overrides the client-supplied model when non-empty.
	Model string
}

// Options configures the router.
type Options struct {
	Routes        []Route
	FailThreshold int
	Cooldown      time.Duration
	Timeout       time.Duration
	Quota         *Quota
	Logger        *slog.Logger
}

type routeState struct {
	route    Route
	mu       sync.Mutex
	failures int
	openTill time.Time
}

func (r *routeState) available(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return now.After(r.openTill)
}

func (r *routeState) fail(now time.Time, threshold int, cooldown time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures++
	if r.failures >= threshold {
		r.openTill = now.Add(cooldown)
		r.failures = 0
	}
}

func (r *routeState) succeed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = 0
	r.openTill = time.Time{}
}

// Gateway routes OpenAI-compatible calls.
type Gateway struct {
	routes        []*routeState
	client        *http.Client
	quota         *Quota
	logger        *slog.Logger
	failThreshold int
	cooldown      time.Duration
}

func New(opts Options) *Gateway {
	threshold := opts.FailThreshold
	if threshold <= 0 {
		threshold = 3
	}
	cooldown := opts.Cooldown
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	states := make([]*routeState, 0, len(opts.Routes))
	for _, route := range opts.Routes {
		states = append(states, &routeState{route: route})
	}
	return &Gateway{
		routes:        states,
		client:        &http.Client{Timeout: timeout},
		quota:         opts.Quota,
		logger:        logger,
		failThreshold: threshold,
		cooldown:      cooldown,
	}
}

// NewHandler exposes /v1/chat/completions, /v1/embeddings and /healthz.
func NewHandler(g *Gateway) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "routes": len(g.routes)})
	})
	mux.HandleFunc("/v1/chat/completions", g.handleChat)
	mux.HandleFunc("/v1/embeddings", g.handleEmbeddings)
	return mux
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	body, payload, err := readPayload(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	stream, _ := payload["stream"].(bool)
	tenant := tenantOf(r)
	estimate := estimateTokens(payload)
	key, ok := g.reserve(w, tenant, estimate)
	if !ok {
		return
	}
	if stream {
		payload["stream_options"] = map[string]any{"include_usage": true}
		body, _ = json.Marshal(payload)
	}
	g.forward(w, r, body, payload, estimate, key, stream)
}

func (g *Gateway) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	body, payload, err := readPayload(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	tenant := tenantOf(r)
	estimate := int64(len(body)) / 4
	key, ok := g.reserve(w, tenant, estimate)
	if !ok {
		return
	}
	g.forward(w, r, body, payload, estimate, key, false)
}

func (g *Gateway) reserve(w http.ResponseWriter, tenant string, estimate int64) (string, bool) {
	if g.quota == nil {
		return "", true
	}
	key := DailyKey(tenant, time.Now())
	allowed, err := g.quota.Reserve(key, estimate)
	if err != nil {
		g.logger.Warn("llm quota unavailable, allowing request", "error", err)
		return "", true
	}
	if !allowed {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message": "tenant daily token quota exceeded",
				"type":    "quota_exceeded",
			},
		})
		return "", false
	}
	return key, true
}

func (g *Gateway) forward(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	payload map[string]any,
	estimate int64,
	quotaKey string,
	stream bool,
) {
	path := r.URL.Path
	var lastStatus int
	for _, state := range g.routes {
		if !state.available(time.Now()) {
			continue
		}
		requestBody := body
		if state.route.Model != "" {
			payload["model"] = state.route.Model
			requestBody, _ = json.Marshal(payload)
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			strings.TrimRight(state.route.BaseURL, "/")+path, bytes.NewReader(requestBody))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if state.route.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+state.route.APIKey)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			g.logger.Warn("llm upstream unavailable", "route", state.route.Name, "error", err)
			state.fail(time.Now(), g.failThreshold, g.cooldown)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastStatus = resp.StatusCode
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			g.logger.Warn("llm upstream failed", "route", state.route.Name, "status", resp.StatusCode)
			state.fail(time.Now(), g.failThreshold, g.cooldown)
			continue
		}
		state.succeed()
		if stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			g.relayStream(w, resp, estimate, quotaKey)
			return
		}
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if readErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": readErr.Error()})
			return
		}
		g.adjustQuota(quotaKey, usageTokens(raw, estimate)-estimate)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	status := http.StatusBadGateway
	if lastStatus == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": "all llm routes are unavailable", "type": "upstream_unavailable"},
	})
}

// relayStream passes SSE through and records usage from the final usage chunk.
func (g *Gateway) relayStream(w http.ResponseWriter, resp *http.Response, estimate int64, quotaKey string) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	usage := int64(0)
	contentChars := 0
	buffer := make([]byte, 32<<10)
	pending := ""
	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			pending += string(buffer[:n])
			for {
				index := strings.IndexByte(pending, '\n')
				if index < 0 {
					break
				}
				line := pending[:index+1]
				pending = pending[index+1:]
				_, _ = io.WriteString(w, line)
				if flusher != nil {
					flusher.Flush()
				}
				if tokens, chars := parseSSELine(line); tokens > 0 {
					usage = tokens
				} else {
					contentChars += chars
				}
			}
		}
		if err != nil {
			break
		}
	}
	if pending != "" {
		_, _ = io.WriteString(w, pending)
	}
	if usage <= 0 {
		usage = estimate + int64(contentChars)/4
	}
	g.adjustQuota(quotaKey, usage-estimate)
}

func parseSSELine(line string) (int64, int) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return 0, 0
	}
	data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if data == "" || data == "[DONE]" {
		return 0, 0
	}
	var chunk struct {
		Usage *struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return 0, 0
	}
	if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
		return chunk.Usage.TotalTokens, 0
	}
	chars := 0
	for _, choice := range chunk.Choices {
		chars += len(choice.Delta.Content)
	}
	return 0, chars
}

func (g *Gateway) adjustQuota(key string, delta int64) {
	if g.quota == nil || key == "" || delta == 0 {
		return
	}
	if err := g.quota.Adjust(key, delta); err != nil {
		g.logger.Warn("llm quota adjust failed", "error", err)
	}
}

func readPayload(r *http.Request) ([]byte, map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return body, payload, nil
}

// estimateTokens reserves prompt size plus the requested completion budget.
func estimateTokens(payload map[string]any) int64 {
	promptChars := 0
	if messages, ok := payload["messages"].([]any); ok {
		for _, message := range messages {
			if item, ok := message.(map[string]any); ok {
				if text, ok := item["content"].(string); ok {
					promptChars += len(text)
				}
			}
		}
	}
	if prompt, ok := payload["prompt"].(string); ok {
		promptChars += len(prompt)
	}
	maxTokens := int64(512)
	if value, ok := payload["max_tokens"].(float64); ok && value > 0 {
		maxTokens = int64(value)
	} else if value, ok := payload["max_completion_tokens"].(float64); ok && value > 0 {
		maxTokens = int64(value)
	}
	return int64(promptChars)/4 + maxTokens
}

func usageTokens(raw []byte, fallback int64) int64 {
	var parsed struct {
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Usage.TotalTokens > 0 {
		return parsed.Usage.TotalTokens
	}
	return fallback
}

func tenantOf(r *http.Request) string {
	if tenant := strings.TrimSpace(r.Header.Get("x-tenant-id")); tenant != "" {
		return tenant
	}
	return "default"
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
