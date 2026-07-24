// Package local executes black-box agents as local operating-system processes.
package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/araihu/paje/internal/runner"
)

// Runner executes one configured command without invoking a shell.
type Runner struct {
	command string
	args    []string
}

var _ runner.Runner = (*Runner)(nil)

// New constructs a local process runner. The task description is appended to
// args for each execution.
func New(command string, args ...string) (*Runner, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("create local runner: command is required")
	}
	return &Runner{
		command: command,
		args:    append([]string(nil), args...),
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

	command := exec.CommandContext(ctx, r.command, args...)
	command.Dir = req.WorkspacePath
	environment, err := mergedEnvironment(req.Env)
	if err != nil {
		return runner.ExecutionResult{}, err
	}
	command.Env = environment

	started := time.Now()
	output, runErr := command.CombinedOutput()
	result := runner.ExecutionResult{
		Output:   string(output),
		ExitCode: 0,
		Duration: time.Since(started).Seconds(),
	}
	if runErr == nil {
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, fmt.Errorf("run local agent: %w", ctxErr)
		}
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("run local agent: %w", ctxErr)
	}
	return result, fmt.Errorf("run local agent: %w", runErr)
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

func mergedEnvironment(overrides map[string]string) ([]string, error) {
	values := make(map[string]string, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if found {
			values[key] = value
		}
	}
	for key, value := range overrides {
		if key == "" || strings.ContainsRune(key, '=') {
			return nil, fmt.Errorf("run local agent: invalid environment key %q", key)
		}
		values[key] = value
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment, nil
}
