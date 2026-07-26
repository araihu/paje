package mock

import (
	"context"
	"sync"

	"github.com/araihu/paje/internal/secret"
)

type acquireResult struct {
	lease secret.Lease
	err   error
}

type Broker struct {
	mu sync.Mutex

	results      map[string]acquireResult
	revokeErrors map[string]error
	requests     []secret.AcquireRequest
	revocations  []string
}

func NewBroker() *Broker {
	return &Broker{
		results:      make(map[string]acquireResult),
		revokeErrors: make(map[string]error),
	}
}

func (broker *Broker) Acquire(ctx context.Context, request secret.AcquireRequest) (secret.Lease, error) {
	if err := ctx.Err(); err != nil {
		return secret.Lease{}, err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.requests = append(broker.requests, request)
	result, ok := broker.results[request.Capability]
	if !ok {
		return secret.Lease{}, secret.ErrBindingNotFound
	}
	if result.err != nil {
		return secret.Lease{}, result.err
	}
	return secret.NewLease(result.lease.ID(), result.lease.ExpiresAt(), result.lease.Materialization())
}

func (broker *Broker) Revoke(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.revocations = append(broker.revocations, id)
	return broker.revokeErrors[id]
}

func (broker *Broker) SetAcquireResult(capability string, lease secret.Lease, err error) {
	broker.mu.Lock()
	clone := lease
	if err == nil {
		materialization := lease.Materialization()
		cloned, cloneErr := secret.NewLease(lease.ID(), lease.ExpiresAt(), materialization)
		materialization.Destroy()
		if cloneErr != nil {
			clone = secret.Lease{}
			err = cloneErr
		} else {
			clone = cloned
		}
	}
	broker.results[capability] = acquireResult{lease: clone, err: err}
	broker.mu.Unlock()
}

func (broker *Broker) SetRevokeError(id string, err error) {
	broker.mu.Lock()
	if err == nil {
		delete(broker.revokeErrors, id)
	} else {
		broker.revokeErrors[id] = err
	}
	broker.mu.Unlock()
}

func (broker *Broker) Requests() []secret.AcquireRequest {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	requests := make([]secret.AcquireRequest, len(broker.requests))
	copy(requests, broker.requests)
	return requests
}

func (broker *Broker) Revocations() []string {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	revocations := make([]string, len(broker.revocations))
	copy(revocations, broker.revocations)
	return revocations
}

var _ secret.Broker = (*Broker)(nil)
