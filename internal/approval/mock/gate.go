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
	result   approval.Result
	err      error
	requests []approval.Request
}

var _ approval.Gate = (*Gate)(nil)

// NewGate constructs a recording approval gate.
func NewGate(result approval.Result, err error) *Gate {
	return &Gate{result: result, err: err}
}

// RequestApproval records a defensive request snapshot and returns the
// configured decision.
func (g *Gate) RequestApproval(ctx context.Context, req approval.Request) (approval.Result, error) {
	if err := ctx.Err(); err != nil {
		return approval.Result{}, err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, approval.CloneRequest(req))
	return g.result, g.err
}

// Requests returns independent snapshots of recorded requests.
func (g *Gate) Requests() []approval.Request {
	g.mu.Lock()
	defer g.mu.Unlock()

	result := make([]approval.Request, len(g.requests))
	for i, req := range g.requests {
		result[i] = approval.CloneRequest(req)
	}
	return result
}
