package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"github.com/araihu/paje/internal/submission"
)

// Trigger is a concurrency-safe idempotent in-memory trigger.
type Trigger struct {
	mu         sync.Mutex
	references map[string]submission.TriggerReference
	bindings   map[string]submission.TriggerRequest
	states     map[submission.TriggerReference]submission.TriggerState
	requests   []submission.TriggerRequest
	cancels    map[submission.TriggerReference]int
	startErr   error
	inspectErr error
	cancelErr  error
}

// NewTrigger returns a trigger that accepts runs and reports accepted state.
func NewTrigger() *Trigger {
	return &Trigger{
		references: make(map[string]submission.TriggerReference),
		bindings:   make(map[string]submission.TriggerRequest),
		states:     make(map[submission.TriggerReference]submission.TriggerState),
		cancels:    make(map[submission.TriggerReference]int),
	}
}

func (t *Trigger) Start(
	ctx context.Context,
	request submission.TriggerRequest,
) (submission.TriggerReference, error) {
	if err := ctx.Err(); err != nil {
		return submission.TriggerReference{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if reference, exists := t.references[request.RunID]; exists {
		binding := t.bindings[request.RunID]
		if !bytes.Equal(binding.Input, request.Input) {
			return submission.TriggerReference{}, submission.ErrIdempotencyConflict
		}
		return reference, nil
	}
	if t.startErr != nil {
		return submission.TriggerReference{}, t.startErr
	}
	reference := submission.TriggerReference{
		Provider:      "mock",
		ExternalRunID: "mock_" + request.RunID,
	}
	t.references[request.RunID] = reference
	t.bindings[request.RunID] = submission.TriggerRequest{
		RunID: request.RunID,
		Input: append(json.RawMessage(nil), request.Input...),
	}
	t.states[reference] = submission.TriggerState{Status: submission.StatusAccepted}
	t.requests = append(t.requests, submission.TriggerRequest{
		RunID: request.RunID,
		Input: append(json.RawMessage(nil), request.Input...),
	})
	return reference, nil
}

func (t *Trigger) Inspect(
	ctx context.Context,
	reference submission.TriggerReference,
) (submission.TriggerState, error) {
	if err := ctx.Err(); err != nil {
		return submission.TriggerState{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inspectErr != nil {
		return submission.TriggerState{}, t.inspectErr
	}
	state, exists := t.states[reference]
	if !exists {
		return submission.TriggerState{}, submission.ErrNotFound
	}
	return cloneState(state), nil
}

func (t *Trigger) Cancel(
	ctx context.Context,
	reference submission.TriggerReference,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.states[reference]; !exists {
		return submission.ErrNotFound
	}
	t.cancels[reference]++
	return t.cancelErr
}

// StartRequests returns a deep copy of the unique starts accepted by the
// trigger, in acceptance order.
func (t *Trigger) StartRequests() []submission.TriggerRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	requests := make([]submission.TriggerRequest, len(t.requests))
	for index, request := range t.requests {
		requests[index] = submission.TriggerRequest{
			RunID: request.RunID,
			Input: append(json.RawMessage(nil), request.Input...),
		}
	}
	return requests
}

// SetStartError configures the deterministic start failure fixture.
func (t *Trigger) SetStartError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.startErr = err
}

// SetInspectError configures the deterministic inspection failure fixture.
func (t *Trigger) SetInspectError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inspectErr = err
}

// SetState replaces the state observed for an existing reference.
func (t *Trigger) SetState(
	reference submission.TriggerReference,
	state submission.TriggerState,
) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.states[reference] = cloneState(state)
}

// SetCancelError configures the deterministic cancellation result.
func (t *Trigger) SetCancelError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancelErr = err
}

// CancelCalls returns the number of cancellation attempts for reference.
func (t *Trigger) CancelCalls(reference submission.TriggerReference) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancels[reference]
}

func cloneState(source submission.TriggerState) submission.TriggerState {
	cloned := source
	if source.Result != nil {
		value := *source.Result
		cloned.Result = &value
	}
	return cloned
}

var _ submission.Trigger = (*Trigger)(nil)
