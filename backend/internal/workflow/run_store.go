package workflow

import (
	"sync"
	"time"
)

// RunStatus is the lifecycle of a persisted workflow run.
type RunStatus string

const (
	RunRunning         RunStatus = "RUNNING"
	RunWaitingApproval RunStatus = "WAITING_APPROVAL"
	// RunSucceeded matches workflow_executions.status in the target schema.
	RunSucceeded RunStatus = "COMPLETED"
	RunFailed    RunStatus = "FAILED"
	RunRejected  RunStatus = "REJECTED"
)

// RunState is the checkpoint saved when a run suspends on human approval.
type RunState struct {
	ID          string         `json:"id"`
	DAG         DAG            `json:"dag"`
	Input       map[string]any `json:"input"`
	Meta        Meta           `json:"meta"`
	Status      string         `json:"status"`
	Order       []string       `json:"order,omitempty"`
	Results     []NodeResult   `json:"results,omitempty"`
	Output      map[string]any `json:"output,omitempty"`
	PendingNode string         `json:"pending_node,omitempty"`
	Error       string         `json:"error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// RunStore persists run checkpoints. MemoryRunStore is the single-process
// implementation; a PostgreSQL-backed store can replace it later.
type RunStore interface {
	Save(*RunState)
	Get(id string) (*RunState, bool)
	List(limit int) []*RunState
}

// MemoryRunStore keeps the most recent runs in memory.
type MemoryRunStore struct {
	mu    sync.RWMutex
	runs  map[string]*RunState
	order []string
	max   int
}

func NewMemoryRunStore(max int) *MemoryRunStore {
	if max <= 0 {
		max = 500
	}
	return &MemoryRunStore{runs: make(map[string]*RunState), max: max}
}

func (s *MemoryRunStore) Save(state *RunState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[state.ID]; !exists {
		s.order = append(s.order, state.ID)
	}
	s.runs[state.ID] = cloneRun(state)
	for len(s.order) > s.max {
		delete(s.runs, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *MemoryRunStore) Get(id string) (*RunState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.runs[id]
	if !ok {
		return nil, false
	}
	return cloneRun(state), true
}

func (s *MemoryRunStore) List(limit int) []*RunState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.order) {
		limit = len(s.order)
	}
	out := make([]*RunState, 0, limit)
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		if state, ok := s.runs[s.order[i]]; ok {
			out = append(out, cloneRun(state))
		}
	}
	return out
}

func cloneRun(state *RunState) *RunState {
	clone := *state
	clone.Order = append([]string(nil), state.Order...)
	clone.Results = append([]NodeResult(nil), state.Results...)
	if state.Output != nil {
		clone.Output = make(map[string]any, len(state.Output))
		for k, v := range state.Output {
			clone.Output[k] = v
		}
	}
	if state.Input != nil {
		clone.Input = make(map[string]any, len(state.Input))
		for k, v := range state.Input {
			clone.Input[k] = v
		}
	}
	return &clone
}
