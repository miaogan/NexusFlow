package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/feedback"
)

func TestQuerySimilarity(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		min  float64
		max  float64
	}{
		{"exact", "如何部署 NexusFlow？", "如何部署 nexusflow", 1, 1},
		{"containment", "怎么部署", "怎么部署 NexusFlow 服务", 0.9, 0.9},
		{"bigram overlap", "NexusFlow 检索链路", "NexusFlow 检索流程", 0.35, 0.9},
		{"unrelated", "如何部署", "沙箱如何隔离网络", 0, 0.35},
		{"empty", "", "问题", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := querySimilarity(tc.a, tc.b)
			if got < tc.min || got > tc.max {
				t.Errorf("querySimilarity(%q, %q) = %v, want within [%v, %v]", tc.a, tc.b, got, tc.min, tc.max)
			}
		})
	}
}

func TestMatchCorrectedAnswersFiltersAndOrders(t *testing.T) {
	now := time.Now().UTC()
	entries := []feedback.Entry{
		{Query: "如何部署 NexusFlow？", CorrectedAnswer: "使用 make up。", UpdatedAt: now.Add(-time.Hour)},
		{Query: "如何部署 NexusFlow？", CorrectedAnswer: "最新方式：docker compose up。", UpdatedAt: now},
		{Query: "沙箱如何隔离？", CorrectedAnswer: "OpenSandbox 强隔离。", UpdatedAt: now},
		{Query: "如何部署 NexusFlow？", CorrectedAnswer: "   ", UpdatedAt: now},
	}

	items := matchCorrectedAnswers("如何部署 NexusFlow", entries, 2)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	// Equal scores keep the most recent correction first.
	if items[0].CorrectedAnswer != "最新方式：docker compose up。" {
		t.Errorf("first item = %q, want newest exact match", items[0].CorrectedAnswer)
	}
	if items[0].MatchScore != 1 {
		t.Errorf("exact match score = %v, want 1", items[0].MatchScore)
	}

	if unrelated := matchCorrectedAnswers("完全无关的问题 xyz", entries, 2); len(unrelated) != 0 {
		t.Errorf("unrelated query matched %d corrections", len(unrelated))
	}
}

func TestCorrectedAnswersEndpoint(t *testing.T) {
	g := newIdentityGateway(t)
	ctx := context.Background()
	if _, err := g.feedbackStore.Upsert(ctx, feedback.Entry{
		TenantID: "default", UserID: "u1", SubjectID: "m1", Name: "user.rating",
		Rating: -1, Query: "如何部署 NexusFlow？", CorrectedAnswer: "使用 make up 启动。",
	}); err != nil {
		t.Fatalf("seed feedback: %v", err)
	}
	if _, err := g.feedbackStore.Upsert(ctx, feedback.Entry{
		TenantID: "other", UserID: "u2", SubjectID: "m2", Name: "user.rating",
		Rating: -1, Query: "如何部署 NexusFlow？", CorrectedAnswer: "不该被跨租户看到。",
	}); err != nil {
		t.Fatalf("seed other tenant feedback: %v", err)
	}

	h := g.Router()
	token := loginToken(t, h, "admin", "admin123")
	w := call(t, h, http.MethodGet,
		"/v1/corrected-answers?query="+url.QueryEscape("如何部署 NexusFlow"), token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Items []CorrectedAnswer `json:"items"`
		Count int               `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || len(body.Items) != 1 {
		t.Fatalf("items = %+v, want the single tenant correction", body.Items)
	}
	if body.Items[0].CorrectedAnswer != "使用 make up 启动。" {
		t.Errorf("corrected answer = %q", body.Items[0].CorrectedAnswer)
	}

	// Missing query is a client error; unrelated queries return nothing.
	if w := call(t, h, http.MethodGet, "/v1/corrected-answers", token, nil); w.Code != http.StatusBadRequest {
		t.Errorf("missing query = %d, want 400", w.Code)
	}
	w = call(t, h, http.MethodGet,
		"/v1/corrected-answers?query="+url.QueryEscape("完全无关"), token, nil)
	if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) {
		t.Errorf("unrelated query status = %d body = %s", w.Code, w.Body.String())
	}
}
