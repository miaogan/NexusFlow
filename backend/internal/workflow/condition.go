package workflow

import (
	"context"
	"fmt"
	"strings"
)

// ConditionNode routes execution based on one value from the run context. It
// reports both the boolean result and the chosen branch; the engine then prunes
// the other branch declared in `then` / `else`.
type ConditionNode struct{}

func (ConditionNode) Type() string { return "condition" }

func (ConditionNode) Run(
	_ context.Context,
	ec *ExecutionContext,
	config map[string]any,
) (map[string]any, error) {
	field, _ := config["field"].(string)
	if strings.TrimSpace(field) == "" {
		return nil, fmt.Errorf("condition requires config.field")
	}
	value, found := lookupValue(ec, field)
	if !found {
		// A field that is not a context variable is treated as a literal, so
		// simple constant conditions ("field": "yes") still work.
		value = field
	}
	matched := evaluateCondition(config, value)
	branch := "else"
	if matched {
		branch = "then"
	}
	out := map[string]any{
		"result": matched,
		"branch": branch,
		"value":  fmt.Sprint(value),
	}
	if target, ok := config["then"].(string); ok && target != "" {
		out["then"] = target
	}
	if target, ok := config["else"].(string); ok && target != "" {
		out["else"] = target
	}
	return out, nil
}

// lookupValue resolves "${node.field}", "node.field" or a plain variable name.
func lookupValue(ec *ExecutionContext, field string) (any, bool) {
	key := strings.TrimSpace(field)
	if strings.HasPrefix(key, "${") && strings.HasSuffix(key, "}") {
		key = strings.TrimSuffix(strings.TrimPrefix(key, "${"), "}")
	}
	return ec.Get(key)
}

func evaluateCondition(config map[string]any, value any) bool {
	if expected, ok := config["equals"]; ok {
		return fmt.Sprint(value) == fmt.Sprint(expected)
	}
	if expected, ok := config["not_equals"]; ok {
		return fmt.Sprint(value) != fmt.Sprint(expected)
	}
	if expected, ok := config["contains"]; ok {
		return strings.Contains(fmt.Sprint(value), fmt.Sprint(expected))
	}
	if raw, ok := config["in"]; ok {
		return containsValue(raw, fmt.Sprint(value))
	}
	return truthy(value)
}

func containsValue(raw any, target string) bool {
	switch list := raw.(type) {
	case []any:
		for _, item := range list {
			if fmt.Sprint(item) == target {
				return true
			}
		}
	case []string:
		for _, item := range list {
			if item == target {
				return true
			}
		}
	case string:
		for _, item := range strings.Split(list, ",") {
			if strings.TrimSpace(item) == target {
				return true
			}
		}
	}
	return false
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		text := strings.ToLower(strings.TrimSpace(typed))
		return text != "" && text != "false" && text != "0"
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	default:
		return true
	}
}
