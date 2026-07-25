package codechangehatchet

import (
	"context"
	"fmt"

	"github.com/araihu/paje/internal/approval"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

type approvalEventWaiter interface {
	WaitForEvent(eventKey, expression string) (hatchet.EventUnmarshaller, error)
}

type durableApprovalGate struct {
	waiter approvalEventWaiter
}

func newDurableApprovalGate(waiter approvalEventWaiter) approval.Gate {
	return durableApprovalGate{waiter: waiter}
}

func (g durableApprovalGate) RequestApproval(_ context.Context, req approval.Request) (approval.Result, error) {
	if g.waiter == nil {
		return approval.Result{}, fmt.Errorf("wait for approval event: durable waiter is required")
	}
	event, err := g.waiter.WaitForEvent("paje:approval:"+req.RunID, "")
	if err != nil {
		return approval.Result{}, fmt.Errorf("wait for approval event: %w", err)
	}
	if event == nil {
		return approval.Result{}, fmt.Errorf("decode approval event: event is required")
	}
	var result approval.Result
	if err := hatchet.EventInto(event, &result); err != nil {
		return approval.Result{}, fmt.Errorf("decode approval event: %w", err)
	}
	if err := result.Validate(req); err != nil {
		return approval.Result{}, err
	}
	return result, nil
}
