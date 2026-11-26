package query

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestDocStore_AddTagsAndCounts(t *testing.T) {
	s := NewDocStore()
	s.Add("a", []Chunk{{ID: "a-0", Content: "hello"}})
	if got := s.Count(); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
	all := s.All()
	if all[0].Meta["doc_id"] != "a" {
		t.Fatalf("doc_id tag missing: %v", all[0].Meta)
	}
}

func TestDocStore_IdempotentReplace(t *testing.T) {
	s := NewDocStore()
	s.Add("a", []Chunk{{Content: "v1"}})
	s.Add("a", []Chunk{{Content: "v2a"}, {Content: "v2b"}})
	if got := s.Count(); got != 2 {
		t.Fatalf("count after replace = %d, want 2", got)
	}
	// other docs are untouched
	s.Add("b", []Chunk{{Content: "x"}})
	if got := s.Count(); got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
}

func TestDocStore_ConcurrentWrites(t *testing.T) {
	s := NewDocStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Add("doc", []Chunk{{Content: "x"}})
			_ = s.All()
			_ = s.Count()
		}()
	}
	wg.Wait()
	if got := s.Count(); got != 1 {
		t.Fatalf("count = %d, want 1 (idempotent replace under concurrency)", got)
	}
}

func TestChunkMarkdown_Paragraphs(t *testing.T) {
	md := "第一段内容。\n\n第二段内容用来测试分段。"
	chunks := ChunkMarkdown(md, 0)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if !strings.Contains(chunks[0].Content, "第一段") {
		t.Fatalf("first chunk lost content: %q", chunks[0].Content)
	}
}

func TestChunkMarkdown_LongRuneSplit(t *testing.T) {
	long := strings.Repeat("中", 1200)
	chunks := ChunkMarkdown(long, 500)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len([]rune(c.Content)) > 500 {
			t.Fatalf("chunk exceeds maxChars: %d runes", len([]rune(c.Content)))
		}
	}
}

func TestEngine_IndexMarkdown(t *testing.T) {
	e := NewEngine(nil, nil)
	store := e.Store()
	store.Add("seed", []Chunk{{Content: "demo"}})

	n, err := e.IndexMarkdown(context.Background(), "doc-1", "你好世界\n\n这是第二段", map[string]string{"tenant_id": "t1"})
	if err != nil {
		t.Fatalf("index markdown: %v", err)
	}
	if n != 2 {
		t.Fatalf("chunks = %d, want 2", n)
	}
	if got := store.Count(); got != 3 { // 1 seed + 2 new
		t.Fatalf("store count = %d, want 3", got)
	}
	// tagged with meta
	all := store.All()
	found := false
	for _, c := range all {
		if c.Meta["doc_id"] == "doc-1" && c.Meta["tenant_id"] == "t1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("indexed chunk missing expected meta")
	}
	// re-index same doc → no growth (idempotent)
	if _, err := e.IndexMarkdown(context.Background(), "doc-1", "你好世界\n\n这是第二段", map[string]string{"tenant_id": "t1"}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if got := store.Count(); got != 3 {
		t.Fatalf("store count after reindex = %d, want 3", got)
	}
}
