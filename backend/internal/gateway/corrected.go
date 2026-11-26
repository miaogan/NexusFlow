package gateway

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/feedback"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

const (
	// correctedCandidates caps how many recent feedback records are scored per
	// request; corrections are rare compared with ratings.
	correctedCandidates = 200
	// correctedDefaultLimit is the number of corrections injected into a prompt.
	correctedDefaultLimit = 3
	correctedMaxLimit     = 10
	// correctedAnswerChars bounds a single injected answer.
	correctedAnswerChars = 1200
	// correctedMinSimilarity drops unrelated historical questions.
	correctedMinSimilarity = 0.35
)

// CorrectedAnswer is a past answer a reviewer explicitly rewrote, matched to
// the current query so rag-agent can prefer it over regenerating.
type CorrectedAnswer struct {
	Query           string  `json:"query"`
	CorrectedAnswer string  `json:"corrected_answer"`
	MatchScore      float64 `json:"match_score"`
	UpdatedAt       string  `json:"updated_at"`
}

// listCorrectedAnswers exposes tenant-scoped human corrections for the RAG
// agent's pre-retrieval lookup: GET /v1/corrected-answers?query=...&limit=3
func (g *Gateway) listCorrectedAnswers(c *gin.Context) {
	query := strings.TrimSpace(c.Query("query"))
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
		return
	}
	limit := correctedDefaultLimit
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n > correctedMaxLimit {
			n = correctedMaxLimit
		}
		limit = n
	}

	p, _ := tenant.From(c.Request.Context())
	entries, err := g.feedbackStore.List(c.Request.Context(), feedback.Filter{
		TenantID: p.TenantID,
		Limit:    correctedCandidates,
	})
	if err != nil {
		g.logger.Error("list corrected answers failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	items := matchCorrectedAnswers(query, entries, limit)
	c.JSON(http.StatusOK, gin.H{"items": items, "count": len(items)})
}

// matchCorrectedAnswers keeps non-empty corrections whose recorded question is
// similar enough to query, ordered by similarity then recency.
func matchCorrectedAnswers(query string, entries []feedback.Entry, limit int) []CorrectedAnswer {
	out := make([]CorrectedAnswer, 0, limit)
	for _, e := range entries {
		answer := strings.TrimSpace(e.CorrectedAnswer)
		if answer == "" {
			continue
		}
		score := querySimilarity(query, e.Query)
		if score < correctedMinSimilarity {
			continue
		}
		if n := len([]rune(answer)); n > correctedAnswerChars {
			answer = string([]rune(answer)[:correctedAnswerChars])
		}
		out = append(out, CorrectedAnswer{
			Query:           strings.TrimSpace(e.Query),
			CorrectedAnswer: answer,
			MatchScore:      score,
			UpdatedAt:       e.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MatchScore != out[j].MatchScore {
			return out[i].MatchScore > out[j].MatchScore
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// querySimilarity scores two questions: exact match wins, containment counts as
// near-exact, otherwise rune-bigram Dice similarity (works for Chinese without
// an external tokenizer).
func querySimilarity(a, b string) float64 {
	a, b = normalizeText(a), normalizeText(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return 0.9
	}
	ab, bb := textBigrams(a), textBigrams(b)
	if len(ab) == 0 || len(bb) == 0 {
		return 0
	}
	shared := 0
	for bg := range ab {
		if _, ok := bb[bg]; ok {
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(ab)+len(bb))
}

func normalizeText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return -1
		case unicode.IsPunct(r):
			return -1
		default:
			return unicode.ToLower(r)
		}
	}, strings.TrimSpace(s))
}

func textBigrams(s string) map[string]struct{} {
	runes := []rune(s)
	out := make(map[string]struct{}, len(runes))
	if len(runes) == 1 {
		out[string(runes)] = struct{}{}
		return out
	}
	for i := 0; i+1 < len(runes); i++ {
		out[string(runes[i:i+2])] = struct{}{}
	}
	return out
}
