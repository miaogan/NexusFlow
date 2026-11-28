package processing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/mineru"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	"github.com/nexusflow/nexusflow/backend/internal/webhook"
)

type uploadedEvent struct {
	TaskID   string `json:"task_id"`
	TenantID string `json:"tenant_id"`
	FileURL  string `json:"file_url"`
	FileType string `json:"file_type"`
	FileName string `json:"file_name"`
}

type parsedEvent struct {
	TaskID   string `json:"task_id"`
	TenantID string `json:"tenant_id"`
	Markdown string `json:"markdown"`
}

// Worker consumes upload events, invokes MinerU with bounded retries and
// publishes the parsed Markdown for the LlamaIndex service.
type Worker struct {
	client       mineru.Client
	machine      state.Store
	pub          queue.Publisher
	logger       *slog.Logger
	pollInterval time.Duration
	maxPolls     int
	maxAttempts  int
	topicParsed  string
	topicDLQ     string
	serviceName  string
	// objects persists parsed markdown so Reindex can rebuild the index without
	// invoking MinerU again. Optional.
	objects store.ObjectStore
	// events publishes task lifecycle webhooks. Optional.
	events webhook.Publisher
}

// WorkerOption configures optional Worker collaborators.
type WorkerOption func(*Worker)

// WithObjectStore enables parsed-markdown persistence.
func WithObjectStore(objects store.ObjectStore) WorkerOption {
	return func(w *Worker) { w.objects = objects }
}

// WithEventPublisher emits task.completed / task.failed webhook events.
func WithEventPublisher(publisher webhook.Publisher) WorkerOption {
	return func(w *Worker) {
		if publisher != nil {
			w.events = publisher
		}
	}
}

func NewWorker(client mineru.Client, machine state.Store, pub queue.Publisher, logger *slog.Logger,
	pollInterval time.Duration, maxPolls, maxAttempts int, topicParsed, topicDLQ, serviceName string,
	opts ...WorkerOption) *Worker {
	worker := &Worker{
		client:       client,
		machine:      machine,
		pub:          pub,
		logger:       logger,
		pollInterval: pollInterval,
		maxPolls:     maxPolls,
		maxAttempts:  maxAttempts,
		topicParsed:  topicParsed,
		topicDLQ:     topicDLQ,
		serviceName:  serviceName,
	}
	for _, opt := range opts {
		opt(worker)
	}
	return worker
}

func (w *Worker) Handle(ctx context.Context, msg queue.Message) error {
	var evt uploadedEvent
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		w.logger.Error("bad upload event", "error", err)
		return err
	}
	task, ok, err := w.machine.Get(ctx, evt.TaskID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown task %s", evt.TaskID)
	}
	if task.State == state.Parsed || task.State == state.Completed {
		return nil // idempotent redelivery
	}

	if task.State == state.Pending {
		if _, err := w.machine.Transition(ctx, task.ID, state.Fetched, ""); err != nil {
			w.logger.Warn("transition to FETCHED failed", "task_id", task.ID, "error", err)
		}
	}
	if _, err := w.machine.Transition(ctx, task.ID, state.Parsing, ""); err != nil {
		w.logger.Warn("transition to PARSING failed", "task_id", task.ID, "error", err)
	}

	start := time.Now()
	var doc mineru.Document
	var parseErr error
	for attempt := 1; attempt <= w.maxAttempts; attempt++ {
		doc, parseErr = mineru.ParseDocument(ctx, w.client, evt.TaskID, evt.FileURL, evt.FileType, w.pollInterval, w.maxPolls)
		if parseErr == nil {
			break
		}
		w.logger.Warn("mineru parse failed", "task_id", task.ID, "attempt", attempt, "error", parseErr)
		if attempt == w.maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
	}

	metrics.ProcessingLatency.
		WithLabelValues(w.serviceName, "parse").
		Observe(time.Since(start).Seconds())

	if parseErr != nil {
		if _, err := w.machine.Transition(ctx, task.ID, state.Failed, parseErr.Error()); err != nil {
			w.logger.Warn("transition to FAILED failed", "task_id", task.ID, "error", err)
		}
		dead, _ := json.Marshal(map[string]string{
			"task_id": task.ID,
			"stage":   "parse",
			"error":   parseErr.Error(),
		})
		_ = w.pub.Publish(ctx, queue.Message{Topic: w.topicDLQ, Key: task.ID, Payload: dead})
		w.emit(task, webhook.EventTaskFailed, map[string]any{
			"task_id":   task.ID,
			"kb_id":     task.Metadata["kb_id"],
			"file_name": task.Metadata["file_name"],
			"stage":     "parse",
			"error":     parseErr.Error(),
		})
		w.logger.Error("task moved to DLQ", "task_id", task.ID, "error", parseErr)
		return nil
	}

	// Persist extracted images first, then point Markdown links at the media
	// endpoint so chunks and citations carry browser-usable image URLs.
	imageKeys := w.persistImages(ctx, task, doc.Images)
	markdown := rewriteImageLinks(doc.Markdown, imageKeys)
	if len(imageKeys) > 0 {
		keys := make([]string, 0, len(imageKeys))
		for _, key := range imageKeys {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if err := w.machine.UpdateMetadata(ctx, task.ID, map[string]string{
			"media_keys": strings.Join(keys, ","),
		}); err != nil {
			w.logger.Warn("record media keys failed", "task_id", task.ID, "error", err)
		}
	}

	// Persist the parsed markdown before publishing. Failure is non-fatal: the
	// document still indexes from the in-memory event, it just cannot be
	// re-indexed later without re-parsing.
	if w.objects != nil {
		if key, err := w.persistMarkdown(ctx, task, markdown); err != nil {
			w.logger.Warn("persist parsed markdown failed", "task_id", task.ID, "error", err)
		} else if err := w.machine.UpdateMetadata(ctx, task.ID, map[string]string{"markdown_key": key}); err != nil {
			w.logger.Warn("record markdown key failed", "task_id", task.ID, "error", err)
		}
	}

	// Publish before flipping state: if publishing fails we return an error and
	// the message is redelivered. The reverse order would mark the task PARSED
	// and then drop the event on redelivery (the state==PARSED short-circuit).
	parsed, _ := json.Marshal(parsedEvent{TaskID: task.ID, TenantID: task.TenantID, Markdown: markdown})
	if err := w.pub.Publish(ctx, queue.Message{
		Topic:   w.topicParsed,
		Key:     task.ID,
		Payload: parsed,
		Headers: map[string]string{"tenant_id": task.TenantID},
	}); err != nil {
		return fmt.Errorf("publish parsed event: %w", err)
	}

	if _, err := w.machine.Transition(ctx, task.ID, state.Parsed, ""); err != nil {
		return fmt.Errorf("transition to PARSED: %w", err)
	}

	w.logger.Info("parse completed", "task_id", task.ID, "markdown_bytes", len(markdown))
	return nil
}

// emit publishes a task lifecycle event without blocking the pipeline.
func (w *Worker) emit(task state.Task, eventType string, payload map[string]any) {
	if w.events == nil {
		return
	}
	w.events.Publish(context.Background(), webhook.Event{
		Type: eventType, TenantID: task.TenantID, Payload: payload,
	})
}

// persistMarkdown stores the parsed markdown under tenant/kb/parsed/<task>.md.
func (w *Worker) persistMarkdown(ctx context.Context, task state.Task, markdown string) (string, error) {
	kbID := task.Metadata["kb_id"]
	if kbID == "" {
		kbID = "default"
	}
	key := fmt.Sprintf("%s/%s/parsed/%s.md", task.TenantID, kbID, task.ID)
	if err := w.objects.Put(ctx, key, strings.NewReader(markdown), int64(len(markdown)), "text/markdown"); err != nil {
		return "", err
	}
	return key, nil
}

var markdownImageLink = regexp.MustCompile(`!\[([^\]]*)\]\(\s*([^)\s]+)\s*\)`)

// persistImages stores extracted images under tenant/kb/parsed/<task>/images/
// and returns archive path -> object key for Markdown rewriting.
func (w *Worker) persistImages(ctx context.Context, task state.Task, images map[string][]byte) map[string]string {
	if w.objects == nil || len(images) == 0 {
		return nil
	}
	kbID := task.Metadata["kb_id"]
	if kbID == "" {
		kbID = "default"
	}
	keys := make(map[string]string, len(images))
	for name, data := range images {
		clean := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, "\\", "/")), "/")
		base := path.Base(clean)
		if base == "" || base == "." || base == ".." || strings.ContainsAny(base, ":\\") {
			continue
		}
		key := fmt.Sprintf("%s/%s/parsed/%s/images/%s", task.TenantID, kbID, task.ID, base)
		contentType := mime.TypeByExtension(path.Ext(base))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if err := w.objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), contentType); err != nil {
			w.logger.Warn("persist parsed image failed", "task_id", task.ID, "image", base, "error", err)
			continue
		}
		keys[clean] = key
	}
	return keys
}

// rewriteImageLinks points Markdown image references at the gateway media
// endpoint, so indexed chunks and chat citations carry fetchable URLs.
func rewriteImageLinks(markdown string, keys map[string]string) string {
	if len(keys) == 0 {
		return markdown
	}
	return markdownImageLink.ReplaceAllStringFunc(markdown, func(match string) string {
		parts := markdownImageLink.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		target := strings.TrimPrefix(parts[2], "./")
		key, ok := keys[target]
		if !ok {
			key, ok = keys["images/"+target]
		}
		if !ok {
			return match
		}
		return "![" + parts[1] + "](/v1/media?key=" + url.QueryEscape(key) + ")"
	})
}
