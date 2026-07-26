// Package codex adapts the Codex CLI to Pajé's runner port.
package codex

import (
	"context"
	"fmt"

	"github.com/araihu/paje/internal/executor"
	harnesscodex "github.com/araihu/paje/internal/harness/codex"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/runner/local"
)

// Runner executes Codex with its deterministic non-interactive JSON protocol.
type Runner struct {
	delegate runner.Runner
	protocol *harnesscodex.Adapter
}

var _ runner.Runner = (*Runner)(nil)

// New constructs a Codex runner using command as the Codex executable.
func New(command string) (*Runner, error) {
	protocol, err := harnesscodex.New(harnesscodex.SupportedVersion)
	if err != nil {
		return nil, fmt.Errorf("create Codex protocol: %w", err)
	}
	agentCommand, err := protocol.AgentCommand("protocol-prompt-placeholder")
	if err != nil {
		return nil, fmt.Errorf("create Codex protocol command: %w", err)
	}
	delegate, err := local.New(command, agentCommand.Args[:len(agentCommand.Args)-1]...)
	if err != nil {
		return nil, fmt.Errorf("create Codex runner: %w", err)
	}
	return &Runner{delegate: delegate, protocol: protocol}, nil
}

// Run executes Codex and returns its final completed agent message.
func (r *Runner) Run(ctx context.Context, req runner.RunRequest) (runner.ExecutionResult, error) {
	result, err := r.delegate.Run(ctx, req)
	if err != nil || result.ExitCode != 0 {
		return result, err
	}

	message, err := r.protocol.Parse(executor.Result{
		Created: result.Started, Started: result.Started, Completed: result.Completed,
		ExitCode: result.ExitCode, Stdout: []byte(result.Transcript),
		StdoutTruncated: result.Truncated,
	})
	if err != nil {
		return result, fmt.Errorf("run Codex agent: %w", err)
	}
	result.Output = message
	return result, nil
}
