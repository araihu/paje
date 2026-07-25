// Package mock provides an in-memory implementation of the publisher port.
package mock

import (
	"context"
	"sync"

	"github.com/araihu/paje/internal/publisher"
)

// Publisher records publication requests and returns a configured result.
type Publisher struct {
	mu       sync.Mutex
	result   publisher.Result
	err      error
	requests []publisher.Request
}

var _ publisher.Publisher = (*Publisher)(nil)

// NewPublisher constructs a recording publisher.
func NewPublisher(result publisher.Result, err error) *Publisher {
	return &Publisher{result: result, err: err}
}

// Publish records a defensive request snapshot and returns the configured
// result.
func (p *Publisher) Publish(ctx context.Context, req publisher.Request) (publisher.Result, error) {
	if err := ctx.Err(); err != nil {
		return publisher.Result{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, publisher.CloneRequest(req))
	return p.result, p.err
}

// Requests returns independent snapshots of recorded requests.
func (p *Publisher) Requests() []publisher.Request {
	p.mu.Lock()
	defer p.mu.Unlock()

	result := make([]publisher.Request, len(p.requests))
	for i, req := range p.requests {
		result[i] = publisher.CloneRequest(req)
	}
	return result
}

// CallCount returns the number of recorded publication calls.
func (p *Publisher) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}
