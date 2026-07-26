package submission

import (
	"context"
	"encoding/json"

	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

// TriggerRequest is the immutable start binding for exactly one durable leaf
// workflow. Input must be the complete canonical template input.
type TriggerRequest struct {
	RunID string
	Input json.RawMessage
}

// TriggerState is a provider-neutral provider observation.
type TriggerState struct {
	Status Status
	Result *templatecodechange.Result
}

// Trigger starts, observes, and cancels one durable leaf workflow without
// leaking provider SDK types into the application domain.
//
// Start is restart-safe by contract. RunID is the provider idempotency identity
// and is permanently bound to the exact canonical Input bytes. Repeating Start
// with that exact binding must reconcile and return the original reference
// without creating another workflow, including after an ambiguous successful
// provider call. Reusing RunID with different Input must fail with
// ErrIdempotencyConflict. An adapter that cannot prove either outcome must fail
// closed rather than create another workflow.
type Trigger interface {
	Start(context.Context, TriggerRequest) (TriggerReference, error)
	Inspect(context.Context, TriggerReference) (TriggerState, error)
	Cancel(context.Context, TriggerReference) error
}
