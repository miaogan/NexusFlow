package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

const (
	defaultLoopConcurrency = 4
	maxLoopConcurrency     = 16
)

// LoopNode runs a sub-DAG once per item with bounded concurrency and aggregates
// each item's output. The parent run fails if any item fails unless
// `continue_on_error` is set.
type LoopNode struct {
	registry *Registry
	logger   *slog.Logger
}

func NewLoopNode(registry *Registry, logger *slog.Logger) *LoopNode {
	if logger == nil {
		logger = slog.Default()
	}
	return &LoopNode{registry: registry, logger: logger}
}

func (n *LoopNode) Type() string { return "loop" }

type loopOutcome struct {
	Index  int            `json:"index"`
	Status string         `json:"status"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

func (n *LoopNode) Run(
	ctx context.Context,
	ec *ExecutionContext,
	config map[string]any,
) (map[string]any, error) {
	items, err := loopItems(ec, config)
	if err != nil {
		return nil, err
	}
	subDAG, err := loopSubDAG(config)
	if err != nil {
		return nil, err
	}
	engine := NewEngine(n.registry, n.logger)
	if _, err := engine.Validate(subDAG); err != nil {
		return nil, fmt.Errorf("loop: invalid sub_dag: %w", err)
	}
	continueOnError, _ := config["continue_on_error"].(bool)

	outcomes := make([]loopOutcome, len(items))
	sem := make(chan struct{}, loopConcurrency(config))
	var wg sync.WaitGroup
	for index, item := range items {
		wg.Add(1)
		go func(index int, value any) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				outcomes[index] = loopOutcome{Index: index, Status: "FAILED", Error: ctx.Err().Error()}
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()

			run, runErr := engine.RunWithOptions(
				ctx, subDAG, loopInput(value, index), RunOptions{Meta: ec.Meta()},
			)
			if runErr != nil {
				outcomes[index] = loopOutcome{Index: index, Status: string(RunFailed), Error: runErr.Error()}
				return
			}
			outcomes[index] = loopOutcome{Index: index, Status: run.Status, Output: run.Output}
		}(index, item)
	}
	wg.Wait()

	results := make([]any, 0, len(outcomes))
	failed := 0
	for _, outcome := range outcomes {
		if outcome.Error != "" || outcome.Status == string(RunFailed) {
			failed++
		}
		results = append(results, outcome)
	}
	out := map[string]any{
		"count":   len(items),
		"failed":  failed,
		"results": results,
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if failed > 0 && !continueOnError {
		return out, fmt.Errorf("loop: %d of %d items failed", failed, len(items))
	}
	return out, nil
}

func loopConcurrency(config map[string]any) int {
	value := 0
	switch typed := config["max_concurrency"].(type) {
	case int:
		value = typed
	case float64:
		value = int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		value = int(parsed)
	}
	if value <= 0 {
		return defaultLoopConcurrency
	}
	if value > maxLoopConcurrency {
		return maxLoopConcurrency
	}
	return value
}

// loopItems resolves `items` ("${node.list}" or a literal array) or the
// shorthand `items_from` variable name.
func loopItems(ec *ExecutionContext, config map[string]any) ([]any, error) {
	raw, ok := config["items"]
	if !ok {
		from, _ := config["items_from"].(string)
		if strings.TrimSpace(from) == "" {
			return nil, fmt.Errorf("loop requires config.items or config.items_from")
		}
		raw = from
	}
	if text, isText := raw.(string); isText {
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "${") && strings.HasSuffix(trimmed, "}") {
			key := strings.TrimSuffix(strings.TrimPrefix(trimmed, "${"), "}")
			value, found := ec.Get(key)
			if !found {
				return nil, fmt.Errorf("loop: field %q not found", key)
			}
			raw = value
		} else if value, found := ec.Get(trimmed); found {
			raw = value
		}
	}
	switch list := raw.(type) {
	case []any:
		return list, nil
	case []string:
		out := make([]any, len(list))
		for index, item := range list {
			out[index] = item
		}
		return out, nil
	default:
		return nil, fmt.Errorf("loop: items must be an array, got %T", raw)
	}
}

func loopSubDAG(config map[string]any) (*DAG, error) {
	raw, ok := config["sub_dag"]
	if !ok {
		return nil, fmt.Errorf("loop requires config.sub_dag")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("loop: encode sub_dag: %w", err)
	}
	var sub DAG
	if err := json.Unmarshal(encoded, &sub); err != nil {
		return nil, fmt.Errorf("loop: invalid sub_dag: %w", err)
	}
	return &sub, nil
}

// loopInput exposes the item as `item`/`index` and spreads map fields so a
// sub-DAG can reference `title` instead of `item.title`.
func loopInput(item any, index int) map[string]any {
	input := map[string]any{"item": item, "index": index}
	if fields, ok := item.(map[string]any); ok {
		for key, value := range fields {
			if _, exists := input[key]; !exists {
				input[key] = value
			}
		}
	}
	return input
}
