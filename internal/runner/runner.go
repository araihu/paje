package runner

import "context"

// RunRequest describes one black-box agent execution.
type RunRequest struct {
	TaskDescription string
	WorkspacePath   string
	Env             map[string]string
}

// ExecutionResult captures the observable result of an agent process.
type ExecutionResult struct {
	Transcript string
	Output     string
	ExitCode   int
	Duration   float64
	Started    bool
	Completed  bool
	Truncated  bool
}

// Runner executes an agent as a black box.
type Runner interface {
	Run(ctx context.Context, req RunRequest) (ExecutionResult, error)
}
