package workflow

import (
	"context"
	"errors"
)

// ErrWaitingApproval is returned by human_approval nodes. The engine catches
// it, persists a checkpoint and reports WAITING_APPROVAL instead of failing.
var ErrWaitingApproval = errors.New("waiting for human approval")

// HumanApprovalNode suspends a run until an operator approves the node.
type HumanApprovalNode struct{}

func (HumanApprovalNode) Type() string { return "human_approval" }

func (HumanApprovalNode) Run(
	_ context.Context,
	_ *ExecutionContext,
	_ map[string]any,
) (map[string]any, error) {
	return nil, ErrWaitingApproval
}
