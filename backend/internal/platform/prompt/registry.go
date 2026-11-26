// Package prompt is a minimal, versioned prompt registry. It is the seed of
// the "Prompt 版本管理" pillar: prompts are named + versioned, and every LLM
// call records which prompt (name + version) it used into the trace. In
// production the registry can be backed by Langfuse's hosted prompt API; the
// in-memory default keeps dev self-contained.
package prompt

import "sync"

// Prompt is a named, versioned text template.
type Prompt struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Content string            `json:"content"`
	Tags    map[string]string `json:"tags,omitempty"`
}

// Registry holds prompts keyed by name (latest version wins on Register).
type Registry struct {
	mu      sync.RWMutex
	prompts map[string]Prompt
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{prompts: make(map[string]Prompt)}
}

// NewDefaultRegistry seeds the built-in prompts used across the platform.
func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(Prompt{
		Name:    "answer.grounding",
		Version: "1",
		Content: defaultAnswerGrounding,
		Tags:    map[string]string{"stage": "answer", "lang": "zh"},
	})
	r.Register(Prompt{
		Name:    "rerank.instruction",
		Version: "1",
		Content: defaultRerankInstruction,
		Tags:    map[string]string{"stage": "rerank", "lang": "zh"},
	})
	return r
}

func (r *Registry) Register(p Prompt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts[p.Name] = p
}

// Get returns the prompt by name.
func (r *Registry) Get(name string) (Prompt, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.prompts[name]
	return p, ok
}

// All returns all registered prompts (unsorted).
func (r *Registry) All() []Prompt {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Prompt, 0, len(r.prompts))
	for _, p := range r.prompts {
		out = append(out, p)
	}
	return out
}

const defaultAnswerGrounding = `你是 NexusFlow 的知识助手。请仅依据下方检索到的片段回答用户问题；若片段不足以回答，请明确说明"未检索到足够信息"，不要编造。

用户问题：{{query}}

检索片段：
{{context}}`

const defaultRerankInstruction = `请根据与用户问题的相关度对以下候选片段排序，最相关的排在前面。`
