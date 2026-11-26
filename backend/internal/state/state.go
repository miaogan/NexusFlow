package state

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State is the task state machine defined in the design doc (chapter 5).
type State string

const (
	Pending   State = "PENDING"
	Fetched   State = "FETCHED"
	Parsing   State = "PARSING"
	Parsed    State = "PARSED"
	Chunked   State = "CHUNKED"
	Embedded  State = "EMBEDDED"
	Indexed   State = "INDEXED"
	Completed State = "COMPLETED"
	Failed    State = "FAILED"

	// Sandbox run-state used by custom code nodes.
	SandboxCreating State = "SANDBOX_CREATING"
	SandboxRunning  State = "SANDBOX_RUNNING"
	SandboxDone     State = "SANDBOX_DONE"
)

var progress = map[State]int32{
	Pending: 5, Fetched: 10, Parsing: 30, Parsed: 45,
	Chunked: 60, Embedded: 75, Indexed: 90, Completed: 100,
	Failed:          0,
	SandboxCreating: 20, SandboxRunning: 50, SandboxDone: 80,
}

var transitions = map[State][]State{
	Pending:  {Fetched, Failed},
	Fetched:  {Parsing, Failed},
	Parsing:  {Parsed, Failed},
	Parsed:   {Chunked, Failed},
	Chunked:  {Embedded, Failed},
	Embedded: {Indexed, Failed},
	Indexed:  {Completed, Failed},
	Failed:   {Pending},

	SandboxCreating: {SandboxRunning, Failed},
	SandboxRunning:  {SandboxDone, Failed},
	SandboxDone:     {Parsed, Chunked, Embedded, Indexed, Completed, Pending},
}

var (
	ErrTaskNotFound  = errors.New("task not found")
	ErrInvalidState  = errors.New("invalid state transition")
	ErrDuplicateTask = errors.New("duplicate task")
)

// Task is the durable unit of work tracked across the pipeline.
type Task struct {
	ID          string            `json:"id"`
	TenantID    string            `json:"tenant_id"`
	State       State             `json:"state"`
	Progress    int32             `json:"progress"`
	ErrorMsg    string            `json:"error_msg,omitempty"`
	ContentHash string            `json:"content_hash,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Store is the task state persistence boundary. Machine is the in-memory
// implementation; RedisStore makes state shared across service processes.
type Store interface {
	Create(ctx context.Context, t Task) (Task, bool, error)
	Get(ctx context.Context, id string) (Task, bool, error)
	Transition(ctx context.Context, id string, to State, errMsg string) (Task, error)
}

type Machine struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	byKey map[string]string // tenant:content_hash -> task_id
}

func NewMachine() *Machine {
	return &Machine{tasks: make(map[string]*Task), byKey: make(map[string]string)}
}

func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Create registers a task. When a content hash is provided the (tenant, hash)
// pair acts as the idempotency key: an existing task is returned instead of
// creating a duplicate.
func (m *Machine) Create(_ context.Context, t Task) (Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.tasks[t.ID]; exists {
		return Task{}, false, ErrDuplicateTask
	}
	if t.ContentHash != "" {
		key := t.TenantID + ":" + t.ContentHash
		if id, ok := m.byKey[key]; ok {
			return *m.tasks[id], false, nil
		}
		m.byKey[key] = t.ID
	}
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if t.State == "" {
		t.State = Pending
	}
	t.Progress = progress[t.State]
	copied := t
	m.tasks[t.ID] = &copied
	return copied, true, nil
}

func (m *Machine) Get(_ context.Context, id string) (Task, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, false, nil
	}
	return *t, true, nil
}

func (m *Machine) Transition(_ context.Context, id string, to State, errMsg string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[id]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	if !CanTransition(t.State, to) {
		return *t, ErrInvalidState
	}
	t.State = to
	t.Progress = progress[to]
	t.ErrorMsg = errMsg
	t.UpdatedAt = time.Now()
	return *t, nil
}
