package query

import "sync"

// DocStore is the shared, concurrency-safe in-memory chunk collection that
// backs the dev-mode retrievers. In production this role is played by Qdrant /
// Milvus; the interface boundary (Retriever) is unchanged.
type DocStore struct {
	mu     sync.RWMutex
	chunks []Chunk
}

func NewDocStore() *DocStore {
	return &DocStore{}
}

// Add inserts (or replaces) all chunks belonging to docID, making re-indexing
// idempotent. Every chunk is tagged with Meta["doc_id"].
func (s *DocStore) Add(docID string, chunks []Chunk) {
	if docID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := make([]Chunk, 0, len(s.chunks)+len(chunks))
	for _, c := range s.chunks {
		if c.Meta != nil && c.Meta["doc_id"] == docID {
			continue // drop previous version of this doc
		}
		kept = append(kept, c)
	}
	for _, c := range chunks {
		if c.Meta == nil {
			c.Meta = make(map[string]string, 1)
		}
		c.Meta["doc_id"] = docID
		kept = append(kept, c)
	}
	s.chunks = kept
}

// All returns a copy of every chunk currently indexed.
func (s *DocStore) All() []Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Chunk, len(s.chunks))
	copy(out, s.chunks)
	return out
}

// Count returns the number of indexed chunks.
func (s *DocStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.chunks)
}
