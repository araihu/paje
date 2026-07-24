// Package mock provides an in-memory implementation of the approval port.
package mock

import (
	"context"
	"sync"

	"github.com/araihu/paje/internal/approval"
)

// Gate records approval requests and returns a configured decision.
type Gate struct {
	mu       sync.Mutex
	approved bool
	err      error
	requests []approval.Request
}

var _ approval.Gate = (*Gate)(nil)

// NewGate constructs a recording approval gate.
func NewGate(approved bool, err error) *Gate {
	return &Gate{approved: approved, err: err}
}

// RequestApproval records req and returns the configured decision.
func (g *Gate) RequestApproval(ctx context.Context, req approval.Request) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, req)
	return g.approved, g.err
}

// Requests returns a snapshot of recorded requests.
func (g *Gate) Requests() []approval.Request {
	g.mu.Lock()
	defer g.mu.Unlock()

	result := make([]approval.Request, len(g.requests))
	copy(result, g.requests)
	return result
}
