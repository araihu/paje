// Package local executes black-box agents as local operating-system processes.
package local

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/araihu/paje/internal/executil"
	"github.com/araihu/paje/internal/runner"
)

const defaultOutputLimit int64 = 1 << 20

// Runner executes one configured command without invoking a shell.
type Runner struct {
	command     string
	args        []string
	outputLimit int64
}

var _ runner.Runner = (*Runner)(nil)

// New constructs a local process runner. The task description is appended to
// args for each execution.
func New(command string, args ...string) (*Runner, error) {
	return NewConfigured(command, args, defaultOutputLimit)
}

// NewConfigured constructs a local process runner with a bounded transcript.
func NewConfigured(command string, args []string, outputLimit int64) (*Runner, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("create local runner: command is required")
	}
	if outputLimit <= 0 {
		return nil, fmt.Errorf("create local runner: output limit must be positive")
	}
	return &Runner{
		command:     command,
		args:        append([]string(nil), args...),
		outputLimit: outputLimit,
	}, nil
}

// Run executes the configured command inside req.WorkspacePath.
func (r *Runner) Run(ctx context.Context, req runner.RunRequest) (runner.ExecutionResult, error) {
	if err := validateRequest(req); err != nil {
		return runner.ExecutionResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return runner.ExecutionResult{}, err
	}

	args := make([]string, 0, len(r.args)+1)
	args = append(args, r.args...)
	args = append(args, req.TaskDescription)

	environment, err := exactEnvironment(req.Env)
	if err != nil {
		return runner.ExecutionResult{}, err
	}
	output, err := executil.NewLimitedBuffer(r.outputLimit)
	if err != nil {
		return runner.ExecutionResult{}, fmt.Errorf("run local agent: create output buffer: %w", err)
	}

	command := exec.CommandContext(ctx, r.command, args...)
	command.Dir = req.WorkspacePath
	command.Env = environment
	command.Stdout = output
	command.Stderr = output
	executil.Configure(command)

	started := time.Now()
	startErr := command.Start()
	result := runner.ExecutionResult{
		Started:   startErr == nil,
		Truncated: output.Truncated(),
	}
	if startErr != nil {
		result.Duration = time.Since(started).Seconds()
		return withOutput(result, output), fmt.Errorf("run local agent: %w", startErr)
	}

	waitErr := command.Wait()
	result.Duration = time.Since(started).Seconds()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	if ctx.Err() == nil {
		result.Completed = waitErr == nil || errors.As(waitErr, &exitErr)
	}
	result = withOutput(result, output)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("run local agent: %w", ctxErr)
	}
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return result, fmt.Errorf("run local agent: %w", waitErr)
	}
	return result, nil
}

func withOutput(result runner.ExecutionResult, output *executil.LimitedBuffer) runner.ExecutionResult {
	transcript := string(output.Bytes())
	result.Transcript = transcript
	result.Output = transcript
	result.Truncated = output.Truncated()
	return result
}

func validateRequest(req runner.RunRequest) error {
	if strings.TrimSpace(req.TaskDescription) == "" {
		return fmt.Errorf("run local agent: task description is required")
	}
	if strings.TrimSpace(req.WorkspacePath) == "" {
		return fmt.Errorf("run local agent: workspace path is required")
	}
	return nil
}

func exactEnvironment(values map[string]string) ([]string, error) {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key == "" || strings.ContainsRune(key, '=') {
			return nil, fmt.Errorf("run local agent: invalid environment key %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}
