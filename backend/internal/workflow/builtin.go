package workflow

import (
	"context"
	"fmt"
)

// NoopNode is a pass-through node used to validate wiring and demos.
type NoopNode struct{}

func (NoopNode) Type() string { return "noop" }

func (NoopNode) Run(_ context.Context, _ *ExecutionContext, _ map[string]any) (map[string]any, error) {
	return map[string]any{"status": "ok"}, nil
}

// SetNode writes a literal value into the execution context, handy for
// prototyping a canvas before a dedicated node exists.
type SetNode struct{}

func (SetNode) Type() string { return "set" }

func (SetNode) Run(_ context.Context, _ *ExecutionContext, config map[string]any) (map[string]any, error) {
	key, _ := config["key"].(string)
	if key == "" {
		return nil, fmt.Errorf("set node requires config.key")
	}
	return map[string]any{key: config["value"]}, nil
}

// StageNode is a declarative pipeline stage. Real execution is delegated to
// the corresponding microservice, so the DAG engine can validate and schedule
// canvas flows before every integration is wired.
type StageNode struct {
	Name string
}

func (n StageNode) Type() string { return n.Name }

func (n StageNode) Run(_ context.Context, _ *ExecutionContext, _ map[string]any) (map[string]any, error) {
	return map[string]any{"stage": n.Name, "status": "ok"}, nil
}
