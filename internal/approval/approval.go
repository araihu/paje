package approval

import "context"

// Request describes a human approval decision.
type Request struct {
	TaskID      string
	Description string
	Diff        string
}

// Gate requests a human decision for a proposed agent action.
type Gate interface {
	RequestApproval(ctx context.Context, req Request) (bool, error)
}
