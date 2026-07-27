// Package mock provides a controllable provider-neutral executor double.
package mock

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/workerprofile"
)

type configuredResult struct {
	result executor.Result
	err    error
}

type Executor struct {
	mu sync.Mutex

	configured map[string]configuredResult
	states     map[string]executor.State
	requests   []executor.Request
	before     func(context.Context, executor.Request)
}

func New() *Executor {
	return &Executor{
		configured: make(map[string]configuredResult),
		states:     make(map[string]executor.State),
	}
}

func (target *Executor) ValidateProfile(profile workerprofile.Snapshot) error {
	canonical, err := workerprofile.Canonicalize(profile)
	if err != nil || profile.Digest == "" || !reflect.DeepEqual(canonical, profile) {
		return errors.New("mock executor profile snapshot is not exact and canonical")
	}
	return nil
}

func (target *Executor) Execute(ctx context.Context, request executor.Request) (executor.Result, error) {
	if err := ctx.Err(); err != nil {
		return executor.Result{}, err
	}
	if err := request.Validate(); err != nil {
		return executor.Result{}, err
	}
	target.mu.Lock()
	before := target.before
	target.mu.Unlock()
	if before != nil {
		hookRequest := request.Clone()
		before(ctx, hookRequest)
		hookRequest.Destroy()
	}
	key := request.Attempt.Key()
	target.mu.Lock()
	defer target.mu.Unlock()
	if state, exists := target.states[key]; exists && state != executor.StateAbsent && state != executor.StateDestroyed {
		return executor.Result{}, executor.ErrAttemptExists
	}
	target.requests = append(target.requests, request.Clone())
	configured, ok := target.configured[key]
	if !ok {
		configured.result = executor.Result{Created: true, Started: true, Completed: true}
	}
	result := configured.result.Clone()
	if result.Started {
		receipt, receiptErr := boundReceipt(request, result.ChildStartReceipt)
		if receiptErr != nil {
			result.Started = false
			result.Completed = false
			result.ChildStartReceipt = nil
			target.states[key] = executor.StateUnknown
			return result, executor.WrapError("internal", "ambiguous_attempt", receiptErr)
		}
		result.ChildStartReceipt = &receipt
	}
	if configured.err != nil {
		if state := stateForResult(result); state != executor.StateAbsent {
			target.states[key] = state
		}
		return result, configured.err
	}
	target.states[key] = stateForResult(result)
	return result, nil
}

func boundReceipt(request executor.Request, configured *executor.ChildStartReceipt) (executor.ChildStartReceipt, error) {
	if configured == nil {
		return executor.NewRandomChildStartReceipt(
			request.Attempt,
			request.Command,
			request.Environment,
			nil,
		)
	}
	expected, err := executor.NewChildStartReceipt(
		request.Attempt,
		request.Command,
		request.Environment,
		nil,
		configured.Challenge,
	)
	if err != nil {
		return executor.ChildStartReceipt{}, err
	}
	if !configured.Matches(expected) {
		return executor.ChildStartReceipt{}, errors.New("mock child-start receipt was rebound")
	}
	return configured.Clone(), nil
}

func (target *Executor) Inspect(ctx context.Context, attempt executor.AttemptID) (executor.State, error) {
	if err := ctx.Err(); err != nil {
		return executor.StateUnknown, err
	}
	if err := attempt.Validate(); err != nil {
		return executor.StateUnknown, err
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	state, ok := target.states[attempt.Key()]
	if !ok {
		return executor.StateAbsent, nil
	}
	return state, nil
}

func (target *Executor) Cancel(ctx context.Context, attempt executor.AttemptID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return attempt.Validate()
}

func (target *Executor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := attempt.Validate(); err != nil {
		return err
	}
	key := attempt.Key()
	target.mu.Lock()
	target.states[key] = executor.StateDestroyed
	for index := range target.requests {
		if target.requests[index].Attempt.Key() == key {
			target.requests[index].Destroy()
		}
	}
	target.mu.Unlock()
	return nil
}

func (target *Executor) SetResult(attempt executor.AttemptID, result executor.Result, err error) {
	target.mu.Lock()
	target.configured[attempt.Key()] = configuredResult{result: result.Clone(), err: err}
	target.mu.Unlock()
}

func (target *Executor) SetState(attempt executor.AttemptID, state executor.State) {
	target.mu.Lock()
	target.states[attempt.Key()] = state
	target.mu.Unlock()
}

func (target *Executor) SetBeforeExecute(before func(context.Context, executor.Request)) {
	target.mu.Lock()
	target.before = before
	target.mu.Unlock()
}

func (target *Executor) Requests() []executor.Request {
	target.mu.Lock()
	defer target.mu.Unlock()
	requests := make([]executor.Request, len(target.requests))
	for index := range target.requests {
		requests[index] = target.requests[index].Clone()
	}
	return requests
}

func stateForResult(result executor.Result) executor.State {
	switch {
	case result.Completed:
		return executor.StateCompleted
	case result.Started:
		return executor.StateRunning
	case result.Created:
		return executor.StateCreated
	default:
		return executor.StateAbsent
	}
}

var (
	_ executor.Executor         = (*Executor)(nil)
	_ executor.ProfileValidator = (*Executor)(nil)
)
