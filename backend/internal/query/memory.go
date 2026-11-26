package query

import (
	"context"
	"sort"
	"strings"
)

// MemoryRetriever is a lightweight in-process recall path for local demo and
// tests. It reads from a shared DocStore so that newly indexed documents are
// immediately retrievable by every registered route. Scoring uses
// unigram/bigram overlap so Chinese text works without an external tokenizer.
type MemoryRetriever struct {
	name  string
	store *DocStore
}

func NewMemoryRetriever(name string, store *DocStore) *MemoryRetriever {
	return &MemoryRetriever{name: name, store: store}
}

func (m *MemoryRetriever) Name() string { return m.name }

func (m *MemoryRetriever) Retrieve(_ context.Context, query string, topK int, filters map[string]string) ([]Chunk, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	qBigrams := bigrams(q)

	type scored struct {
		chunk Chunk
		score float64
	}
	var hits []scored
	for _, doc := range m.store.All() {
		if !matchFilters(doc, filters) {
			continue
		}
		score := overlap(q, qBigrams, strings.ToLower(doc.Content))
		if score <= 0 {
			continue
		}
		c := doc
		c.Source = m.name
		hits = append(hits, scored{chunk: c, score: score})
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].chunk.ID < hits[j].chunk.ID
	})
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]Chunk, len(hits))
	for i, h := range hits {
		h.chunk.Score = h.score
		out[i] = h.chunk
	}
	return out, nil
}

func matchFilters(c Chunk, filters map[string]string) bool {
	for k, v := range filters {
		if c.Meta == nil {
			return false
		}
		if c.Meta[k] != v {
			return false
		}
	}
	return true
}

func bigrams(s string) map[string]struct{} {
	runes := []rune(s)
	set := make(map[string]struct{}, len(runes))
	for i := 0; i+1 < len(runes); i++ {
		set[string(runes[i:i+2])] = struct{}{}
	}
	if len(runes) == 1 {
		set[string(runes)] = struct{}{}
	}
	return set
}

func overlap(query string, qBigrams map[string]struct{}, content string) float64 {
	if strings.Contains(content, query) {
		return 1
	}
	if len(qBigrams) == 0 {
		return 0
	}
	cBigrams := bigrams(content)
	shared := 0
	for bg := range qBigrams {
		if _, ok := cBigrams[bg]; ok {
			shared++
		}
	}
	return float64(shared) / float64(len(qBigrams))
}
