package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusflow/nexusflow/backend/internal/identity"
)

// openAIMessage accepts either a plain string or a content-part array, which
// covers the text-only subset used by OpenAI-compatible clients.
type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type openAIChatRequest struct {
	Model          string            `json:"model"`
	Messages       []openAIMessage   `json:"messages"`
	Stream         bool              `json:"stream"`
	Metadata       map[string]string `json:"metadata"`
	KBID           string            `json:"kb_id"`
	ConversationID string            `json:"conversation_id"`
	TopK           int               `json:"top_k"`
}

type openAIHistoryMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ragChatRequest mirrors the Agent Hub /v1/chat payload. Stateless requests
// carry the caller-supplied history so external clients stay self-contained.
type ragChatRequest struct {
	Message        string                 `json:"message"`
	ConversationID string                 `json:"conversation_id,omitempty"`
	KBID           string                 `json:"kb_id,omitempty"`
	TopK           int                    `json:"top_k,omitempty"`
	History        []openAIHistoryMessage `json:"history,omitempty"`
	Stateless      bool                   `json:"stateless,omitempty"`
}

type openAIUpstreamResponse struct {
	ConversationID string           `json:"conversation_id"`
	MessageID      string           `json:"message_id"`
	Answer         string           `json:"answer"`
	Citations      []map[string]any `json:"citations"`
	TraceID        string           `json:"trace_id"`
	CreatedAt      string           `json:"created_at"`
}

// openAIChat is the OpenAI-compatible facade over the RAG agent. It accepts
// either an API key or a console JWT and relays citations on the response.
func (g *Gateway) openAIChat(c *gin.Context) {
	if !g.requireRAG(c) {
		return
	}
	req, ok := g.readOpenAIBody(c)
	if !ok {
		return
	}
	message, history, ok := openAIHistory(req.Messages)
	if !ok {
		writeOpenAIError(c, http.StatusBadRequest, "messages must contain a non-empty user message", "invalid_request_error")
		return
	}
	if !g.allowQuery(c) {
		return
	}

	kbID := strings.TrimSpace(req.KBID)
	if kbID == "" {
		kbID = strings.TrimSpace(req.Metadata["kb_id"])
	}
	if kbID == "" {
		kbID = strings.TrimSpace(c.GetHeader("X-KB-ID"))
	}
	resolvedKB, role, ok := g.resolveKnowledgeBase(c, kbID)
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleViewer) {
		return
	}

	payload := ragChatRequest{Message: message, KBID: resolvedKB, TopK: req.TopK}
	if req.ConversationID != "" {
		// A caller-managed conversation reuses the hub store.
		payload.ConversationID = req.ConversationID
	} else {
		payload.Stateless = true
		payload.History = history
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "encode request: "+err.Error(), "invalid_request_error")
		return
	}

	if req.Stream {
		g.openAIStream(c, req, message, history, body)
		return
	}
	g.openAIComplete(c, req, message, history, body)
}

func (g *Gateway) readOpenAIBody(c *gin.Context) (openAIChatRequest, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxChatBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "read body: "+err.Error(), "invalid_request_error")
		return openAIChatRequest{}, false
	}
	var req openAIChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid json: "+err.Error(), "invalid_request_error")
		return openAIChatRequest{}, false
	}
	return req, true
}

// openAIHistory picks the last user message and flattens prior user/assistant
// turns into the hub's history field.
func openAIHistory(messages []openAIMessage) (string, []openAIHistoryMessage, bool) {
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && openAIContentText(messages[i].Content) != "" {
			last = i
			break
		}
	}
	if last < 0 {
		return "", nil, false
	}

	history := make([]openAIHistoryMessage, 0, last)
	for _, m := range messages[:last] {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		text := openAIContentText(m.Content)
		if text == "" {
			continue
		}
		history = append(history, openAIHistoryMessage{Role: m.Role, Content: text})
	}
	const maxHistory = 50
	if len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
	}
	return openAIContentText(messages[last].Content), history, true
}

// openAIContentText extracts text from a string or content-part array.
func openAIContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func (g *Gateway) openAIComplete(
	c *gin.Context,
	req openAIChatRequest,
	message string,
	history []openAIHistoryMessage,
	body []byte,
) {
	resp, err := g.rag.do(c, http.MethodPost, "/v1/chat", body)
	if err != nil {
		g.logger.Error("rag agent request failed", "path", "/v1/chat", "error", err)
		writeOpenAIError(c, http.StatusBadGateway, "rag agent unavailable", "upstream_error")
		return
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		writeOpenAIError(c, http.StatusBadGateway, upstreamErrorMessage(payload), "upstream_error")
		return
	}
	var out openAIUpstreamResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "invalid upstream response", "upstream_error")
		return
	}
	g.recordQuery(c)

	promptTokens := approxTokens(message)
	for _, h := range history {
		promptTokens += approxTokens(h.Content)
	}
	completionTokens := approxTokens(out.Answer)

	c.JSON(http.StatusOK, gin.H{
		"id":      "chatcmpl-" + uuid.NewString(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   openAIModel(req),
		"choices": []gin.H{{
			"index": 0,
			"message": gin.H{
				"role":      "assistant",
				"content":   out.Answer,
				"citations": nonNilCitations(out.Citations),
			},
			"finish_reason": "stop",
		}},
		"usage": gin.H{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
		"citations":       nonNilCitations(out.Citations),
		"conversation_id": out.ConversationID,
		"trace_id":        out.TraceID,
	})
}

func (g *Gateway) openAIStream(
	c *gin.Context,
	req openAIChatRequest,
	message string,
	history []openAIHistoryMessage,
	body []byte,
) {
	resp, err := g.rag.do(c, http.MethodPost, "/v1/chat/stream", body)
	if err != nil {
		g.logger.Error("rag agent stream failed", "error", err)
		writeOpenAIError(c, http.StatusBadGateway, "rag agent unavailable", "upstream_error")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes))
		writeOpenAIError(c, http.StatusBadGateway, upstreamErrorMessage(payload), "upstream_error")
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	g.recordQuery(c)

	flusher, _ := c.Writer.(http.Flusher)
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	model := openAIModel(req)
	citations := []map[string]any{}
	sentContent := false

	writeChunk := func(delta gin.H, finish string) bool {
		chunk := gin.H{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []gin.H{{
				"index":         0,
				"delta":         delta,
				"finish_reason": nil,
			}},
		}
		if finish != "" {
			chunk["choices"] = []gin.H{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}}
			if len(citations) > 0 {
				chunk["citations"] = citations
			}
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	if !writeChunk(gin.H{"role": "assistant"}, "") {
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type      string           `json:"type"`
			Content   string           `json:"content"`
			Error     string           `json:"error"`
			Answer    string           `json:"answer"`
			Citations []map[string]any `json:"citations"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event.Type {
		case "token":
			if event.Content == "" {
				continue
			}
			sentContent = true
			if !writeChunk(gin.H{"content": event.Content}, "") {
				return
			}
		case "sources", "done":
			if len(event.Citations) > 0 {
				citations = event.Citations
			}
			// Some intent/skill paths only emit the final answer in done.
			if event.Type == "done" && !sentContent && event.Answer != "" {
				sentContent = true
				if !writeChunk(gin.H{"content": event.Answer}, "") {
					return
				}
			}
		case "error":
			failure, _ := json.Marshal(gin.H{
				"error": gin.H{"message": event.Error, "type": "upstream_error"},
			})
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", failure)
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
			return
		}
	}

	if !writeChunk(gin.H{}, "stop") {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
	_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
}

func openAIModel(req openAIChatRequest) string {
	if strings.TrimSpace(req.Model) != "" {
		return req.Model
	}
	return "nexusflow"
}

func writeOpenAIError(c *gin.Context, status int, message, kind string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    kind,
			"code":    kind,
		},
	})
}

// upstreamErrorMessage extracts the {"error": "..."} shape used by the hub.
func upstreamErrorMessage(payload []byte) string {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && len(body.Error) > 0 {
		var text string
		if json.Unmarshal(body.Error, &text) == nil && text != "" {
			return text
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body.Error, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
	}
	return "upstream request failed"
}

func nonNilCitations(in []map[string]any) []map[string]any {
	if in == nil {
		return []map[string]any{}
	}
	return in
}

// approxTokens estimates token usage at ~4 characters per token.
func approxTokens(text string) int {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return 0
	}
	return (len(runes) + 3) / 4
}
