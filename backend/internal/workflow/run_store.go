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
	ID  string `json:"id"`
	DAG DAG    `json:"dag"`
	// DefinitionID links a run to the saved workflow it was started from.
	DefinitionID string         `json:"definition_id,omitempty"`
	Input        map[string]any `json:"input"`
	Meta         Meta           `json:"meta"`
	Status       string         `json:"status"`
	Order        []string       `json:"order,omitempty"`
	Results      []NodeResult   `json:"results,omitempty"`
	// Skipped lists nodes pruned by an unchosen condition branch.
	Skipped     []string       `json:"skipped,omitempty"`
	Output      map[string]any `json:"output,omitempty"`
	PendingNode string         `json:"pending_node,omitempty"`
	Error       string         `json:"error,omitempty"`
	// ApprovedBy and ApprovalReason record who resumed the run so audit and
	// approval tickets keep the reviewer instead of the original requester.
	ApprovedBy     string `json:"approved_by,omitempty"`
	ApprovalReason string `json:"approval_reason,omitempty"`
	// ParentRunID/ParentNodeID/ParentIndex link loop sub-runs to the parent
	// execution so each iteration stays inspectable without polluting the
	// top-level run list.
	ParentRunID  string    `json:"parent_run_id,omitempty"`
	ParentNodeID string    `json:"parent_node_id,omitempty"`
	ParentIndex  *int      `json:"parent_index,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RunStore persists run checkpoints. MemoryRunStore is the single-process
// implementation; PostgresRunStore keeps runs across replicas and restarts.
type RunStore interface {
	Save(*RunState)
	Get(id string) (*RunState, bool)
	// List returns top-level runs only; loop sub-runs are exposed by Children.
	List(limit int) []*RunState
	// Children returns the loop sub-runs of a parent execution, newest first.
	Children(parentID string, limit int) []*RunState
}

// RunTransitioner is an optional RunStore extension that applies a status
// change atomically. decideRun uses it to guarantee two replicas cannot both
// approve the same suspended run; stores without it fall back to Get + Save.
type RunTransitioner interface {
	// Transition applies mutate when the stored run is still in `from` status.
	// It returns false when the run is missing or already past that status.
	Transition(id string, from RunStatus, mutate func(*RunState) error) (bool, error)
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
		if state, ok := s.runs[s.order[i]]; ok && state.ParentRunID == "" {
			out = append(out, cloneRun(state))
		}
	}
	return out
}

// Children returns persisted loop sub-runs of the given parent execution.
func (s *MemoryRunStore) Children(parentID string, limit int) []*RunState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.order) {
		limit = len(s.order)
	}
	out := make([]*RunState, 0, limit)
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		if state, ok := s.runs[s.order[i]]; ok && state.ParentRunID == parentID {
			out = append(out, cloneRun(state))
		}
	}
	return out
}

// Transition applies a status change under the store lock.
func (s *MemoryRunStore) Transition(
	id string, from RunStatus, mutate func(*RunState) error,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.runs[id]
	if !ok || RunStatus(state.Status) != from {
		return false, nil
	}
	next := cloneRun(state)
	if err := mutate(next); err != nil {
		return false, err
	}
	s.runs[id] = next
	return true, nil
}

func cloneRun(state *RunState) *RunState {
	clone := *state
	clone.Order = append([]string(nil), state.Order...)
	clone.Results = append([]NodeResult(nil), state.Results...)
	clone.Skipped = append([]string(nil), state.Skipped...)
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
