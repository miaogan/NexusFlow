package query

import (
	"context"
	"sort"
)

// NegativeFeedbackCounter reports how many negative feedback signals each chunk
// has accumulated. Implementations aggregate feedback across users of a tenant.
type NegativeFeedbackCounter interface {
	// NegativeCounts returns negative signal counts keyed by chunk id. Chunks
	// without negative feedback may be omitted from the map.
	NegativeCounts(ctx context.Context, tenantID string, chunkIDs []string) (map[string]int, error)
}

// FeedbackPenalty is the online down-ranking stage: chunks whose accumulated
// negative feedback reaches Threshold have their score multiplied by Factor, so
// repeatedly unhelpful passages sink below better-answered alternatives.
type FeedbackPenalty struct {
	Counter   NegativeFeedbackCounter
	Threshold int
	Factor    float64
}

// NewFeedbackPenalty builds the stage with production defaults: three negative
// signals trigger a 0.5x score multiplier.
func NewFeedbackPenalty(c NegativeFeedbackCounter) *FeedbackPenalty {
	return &FeedbackPenalty{Counter: c, Threshold: 3, Factor: 0.5}
}

// Apply penalises matching chunks and re-sorts the list by the adjusted score.
// Counter failures fail open: retrieval must not break when feedback storage is
// unavailable. The input slice is returned unchanged in that case.
func (p *FeedbackPenalty) Apply(ctx context.Context, tenantID string, chunks []Chunk) []Chunk {
	if p == nil || p.Counter == nil || len(chunks) == 0 {
		return chunks
	}
	threshold := p.Threshold
	if threshold <= 0 {
		threshold = 3
	}
	factor := p.Factor
	if factor <= 0 || factor >= 1 {
		factor = 0.5
	}

	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return chunks
	}
	counts, err := p.Counter.NegativeCounts(ctx, tenantID, ids)
	if err != nil || len(counts) == 0 {
		return chunks
	}

	out := make([]Chunk, len(chunks))
	copy(out, chunks)
	for i := range out {
		if n := counts[out[i].ID]; n >= threshold {
			out[i].Score *= factor
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return chunkKey(out[i]) < chunkKey(out[j])
	})
	return out
}
