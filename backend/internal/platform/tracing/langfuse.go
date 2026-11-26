package tracing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// LangfuseTracer ships spans to a Langfuse instance (self-hosted or cloud)
// via its public ingestion API. It is fail-open: ingestion errors are logged
// and dropped, never blocking the instrumented request path.
//
// Configuration (env):
//
//	LANGFUSE_BASE_URL   e.g. https://cloud.langfuse.com  or  http://localhost:3000
//	LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY  (project API keys)
type LangfuseTracer struct {
	baseURL      string
	publicKey    string
	secretKey    string
	logger       *slog.Logger
	http         *http.Client
	flushEvery   time.Duration
	batchTrigger int

	mu      sync.Mutex
	buffer  []langfuseEvent
	stop    chan struct{}
	stopped bool
	wg      sync.WaitGroup
}

// NewLangfuseTracer constructs an exporter and starts its background flush
// loop. Call Shutdown to drain the final batch.
func NewLangfuseTracer(baseURL, publicKey, secretKey string, logger *slog.Logger) *LangfuseTracer {
	t := &LangfuseTracer{
		baseURL:      baseURL,
		publicKey:    publicKey,
		secretKey:    secretKey,
		logger:       logger,
		http:         &http.Client{Timeout: 5 * time.Second},
		flushEvery:   2 * time.Second,
		batchTrigger: 50,
		stop:         make(chan struct{}),
	}
	t.wg.Add(1)
	go t.loop()
	return t
}

func (t *LangfuseTracer) Start(ctx context.Context, name string, opts ...StartOption) (Span, context.Context) {
	ctx, traceID := EnsureTraceID(ctx)
	cfg := &startConfig{}
	for _, o := range opts {
		o(cfg)
	}
	parent := spanFromContext(ctx)
	data := &SpanData{
		TraceID:    traceID,
		SpanID:     NewID(),
		Name:       name,
		StartTime:  time.Now(),
		Status:     StatusUnset,
		Input:      cfg.input,
		Attributes: cfg.attributes,
	}
	if parent != nil {
		data.ParentID = parent.SpanID()
	}
	s := &langfuseSpan{data: data, tracer: t}
	return s, withSpan(ctx, s)
}

func (t *LangfuseTracer) AddScore(_ context.Context, traceID, name string, value float64, comment string) error {
	t.enqueue(langfuseEvent{
		ID:        NewID(),
		Type:      "score-create",
		Timestamp: time.Now(),
		Body: map[string]any{
			"traceId":  traceID,
			"name":     name,
			"value":    value,
			"comment":  comment,
			"dataType": "NUMERIC",
		},
	})
	return nil
}

// Shutdown signals the flush loop to stop and drains the remaining batch.
func (t *LangfuseTracer) Shutdown(ctx context.Context) error {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return nil
	}
	t.stopped = true
	close(t.stop)
	t.mu.Unlock()

	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	// Final best-effort flush of anything left.
	t.flush()
	return nil
}

func (t *LangfuseTracer) enqueueSpan(data *SpanData) {
	events := t.toEvents(data)
	t.enqueue(events...)
}

func (t *LangfuseTracer) enqueue(evs ...langfuseEvent) {
	t.mu.Lock()
	t.buffer = append(t.buffer, evs...)
	flushNow := len(t.buffer) >= t.batchTrigger
	t.mu.Unlock()
	if flushNow {
		t.flush()
	}
}

func (t *LangfuseTracer) loop() {
	defer t.wg.Done()
	ticker := time.NewTicker(t.flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.flush()
		case <-t.stop:
			return
		}
	}
}

func (t *LangfuseTracer) flush() {
	t.mu.Lock()
	if len(t.buffer) == 0 {
		t.mu.Unlock()
		return
	}
	batch := t.buffer
	t.buffer = nil
	t.mu.Unlock()

	if err := t.post(batch); err != nil {
		if t.logger != nil {
			t.logger.Warn("langfuse ingestion failed, dropping batch", "error", err, "events", len(batch))
		}
	}
}

func (t *LangfuseTracer) post(batch []langfuseEvent) error {
	body, err := json.Marshal(map[string]any{"batch": batch})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, t.baseURL+"/api/public/ingestion", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.publicKey != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(t.publicKey + ":" + t.secretKey))
		req.Header.Set("Authorization", "Basic "+cred)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &httpError{Status: resp.StatusCode}
	}
	return nil
}

type httpError struct{ Status int }

func (e *httpError) Error() string { return "langfuse ingestion status " + strconv.Itoa(e.Status) }

func (t *LangfuseTracer) toEvents(data *SpanData) []langfuseEvent {
	now := time.Now()
	events := make([]langfuseEvent, 0, 2)
	if data.ParentID == "" {
		events = append(events, langfuseEvent{
			ID:        NewID(),
			Type:      "trace-create",
			Timestamp: data.StartTime,
			Body: map[string]any{
				"id":       data.TraceID,
				"name":     data.Name,
				"input":    data.Input,
				"output":   data.Output,
				"metadata": data.Attributes,
			},
		})
	}
	spanBody := map[string]any{
		"traceId":       data.TraceID,
		"id":            data.SpanID,
		"name":          data.Name,
		"input":         data.Input,
		"output":        data.Output,
		"startTime":     data.StartTime,
		"endTime":       data.EndTime,
		"metadata":      data.Attributes,
		"level":         "DEFAULT",
		"status":        mapLangfuseStatus(data.Status),
		"statusMessage": data.StatusMsg,
	}
	if data.ParentID != "" {
		spanBody["parentObservationId"] = data.ParentID
	}
	events = append(events, langfuseEvent{
		ID:        NewID(),
		Type:      "span-create",
		Timestamp: now,
		Body:      spanBody,
	})
	return events
}

func mapLangfuseStatus(s SpanStatus) string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusError:
		return "ERROR"
	default:
		return "DEFAULT"
	}
}

type langfuseEvent struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Body      any       `json:"body"`
}

type langfuseSpan struct {
	data   *SpanData
	tracer *LangfuseTracer
	done   bool
}

func (s *langfuseSpan) TraceID() string  { return s.data.TraceID }
func (s *langfuseSpan) SpanID() string   { return s.data.SpanID }
func (s *langfuseSpan) ParentID() string { return s.data.ParentID }
func (s *langfuseSpan) Name() string     { return s.data.Name }

func (s *langfuseSpan) SetInput(v any)                 { s.data.Input = v }
func (s *langfuseSpan) SetOutput(v any)                { s.data.Output = v }
func (s *langfuseSpan) SetAttributes(m map[string]any) { s.data.Attributes = m }
func (s *langfuseSpan) SetStatus(st SpanStatus, msg string) {
	s.data.Status = st
	s.data.StatusMsg = msg
}
func (s *langfuseSpan) RecordError(err error) {
	if err == nil {
		return
	}
	s.data.Status = StatusError
	s.data.StatusMsg = err.Error()
}

func (s *langfuseSpan) End() {
	if s.done {
		return
	}
	s.done = true
	s.data.EndTime = time.Now()
	if s.data.Status == StatusUnset {
		s.data.Status = StatusOK
	}
	s.tracer.enqueueSpan(s.data)
}
