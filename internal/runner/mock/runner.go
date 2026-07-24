// Package mock provides an in-memory implementation of the runner port.
package mock

import (
	"context"
	"sync"

	"github.com/araihu/paje/internal/runner"
)

// Runner records requests and returns a configured result.
type Runner struct {
	mu       sync.Mutex
	result   runner.ExecutionResult
	err      error
	requests []runner.RunRequest
}

var _ runner.Runner = (*Runner)(nil)

// NewRunner constructs a recording runner.
func NewRunner(result runner.ExecutionResult, err error) *Runner {
	return &Runner{result: result, err: err}
}

// Run records req and returns the configured result and error.
func (r *Runner) Run(ctx context.Context, req runner.RunRequest) (runner.ExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return runner.ExecutionResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, cloneRequest(req))
	return r.result, r.err
}

// Requests returns a defensive snapshot of recorded requests.
func (r *Runner) Requests() []runner.RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]runner.RunRequest, len(r.requests))
	for i, req := range r.requests {
		result[i] = cloneRequest(req)
	}
	return result
}

func cloneRequest(req runner.RunRequest) runner.RunRequest {
	if req.Env == nil {
		return req
	}
	sourceEnv := req.Env
	req.Env = make(map[string]string, len(req.Env))
	for key, value := range sourceEnv {
		req.Env[key] = value
	}
	return req
}
