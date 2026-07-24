package verification

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/araihu/paje/internal/executil"
)

const (
	failureEnvironment  = "environment"
	failureVerification = "verification"
	failureCanceled     = "canceled"
	failureInternal     = "internal"
)

var dockerEnvironmentDiagnostics = []string{
	"rootless docker not found",
	"cannot connect to the docker daemon",
	"error during connect",
	"is the docker daemon running",
}

// Runner is the verification execution port used by workflow services.
type Runner interface {
	Run(context.Context, Command, map[string]string) Result
}

// Executor runs compiled verification commands with bounded output.
type Executor struct {
	limits Limits
}

var _ Runner = (*Executor)(nil)

// NewExecutor validates execution bounds and constructs an executor.
func NewExecutor(limits Limits) (*Executor, error) {
	if err := validateLimits(limits); err != nil {
		return nil, fmt.Errorf("create verification executor: %w", err)
	}
	if limits.MaxOutputBytes <= 0 {
		return nil, fmt.Errorf("create verification executor: output limit must be positive")
	}
	return &Executor{limits: limits}, nil
}

// Run executes one precompiled shell-free command in an exact environment.
func (e *Executor) Run(ctx context.Context, command Command, values map[string]string) Result {
	result := Result{Command: resultCommand(command)}
	if err := ctx.Err(); err != nil {
		return failedResult(result, failureCanceled, "caller_canceled", command.Required)
	}
	if err := e.validateCommand(command); err != nil {
		return failedResult(result, failureInternal, "invalid_command", command.Required)
	}
	environment, err := exactEnvironment(mergeEnvironment(values, command.Environment))
	if err != nil {
		return failedResult(result, failureInternal, "invalid_environment", command.Required)
	}
	output, err := executil.NewLimitedBuffer(e.limits.MaxOutputBytes)
	if err != nil {
		return failedResult(result, failureInternal, "output_buffer", command.Required)
	}

	childCtx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()
	executable, err := ResolveExecutable(command.Executable, command.Directory, mergeEnvironment(values, command.Environment))
	if err != nil {
		return failedResult(result, failureEnvironment, "start", command.Required)
	}
	cmd := exec.CommandContext(childCtx, executable, command.Args...)
	executil.Configure(cmd)
	configuredCancel := cmd.Cancel
	var cancellationTerminated atomic.Bool
	cmd.Cancel = func() error {
		err := configuredCancel()
		if err == nil {
			cancellationTerminated.Store(true)
		}
		return err
	}
	cmd.Dir = command.Directory
	cmd.Env = environment
	cmd.Stdout = output
	cmd.Stderr = output

	started := time.Now()
	startErr := cmd.Start()
	if startErr != nil {
		result.Duration = time.Since(started)
		return finishStartFailure(result, output, startErr, ctx, childCtx, command.Required)
	}

	waitErr := cmd.Wait()
	result.Duration = time.Since(started)
	result.Output = string(output.Bytes())
	result.Truncated = output.Truncated()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	if waitErr == nil {
		result.Passed = true
		return result
	}
	if errors.As(waitErr, &exitErr) {
		if exitErr.ExitCode() >= 0 {
			return classifyExitFailure(result, command)
		}
		if cancellationTerminated.Load() || ctx.Err() != nil || errors.Is(childCtx.Err(), context.DeadlineExceeded) {
			return classifyCancellation(result, ctx, childCtx, command.Required)
		}
		return failedResult(result, failureVerification, "nonzero_exit", command.Required)
	}
	if cancellationTerminated.Load() || ctx.Err() != nil || errors.Is(childCtx.Err(), context.DeadlineExceeded) {
		return classifyCancellation(result, ctx, childCtx, command.Required)
	}
	return failedResult(result, failureInternal, "wait", command.Required)
}

func classifyCancellation(result Result, ctx, childCtx context.Context, required bool) Result {
	if err := ctx.Err(); err != nil {
		return failedResult(result, failureCanceled, "caller_canceled", required)
	}
	if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
		return failedResult(result, failureEnvironment, "timeout", required)
	}
	return failedResult(result, failureInternal, "cancellation", required)
}

func classifyExitFailure(result Result, command Command) Result {
	if isDockerEnvironmentFailure(command.Executable, result.Output) {
		return failedResult(result, failureEnvironment, "docker_unavailable", command.Required)
	}
	return failedResult(result, failureVerification, "nonzero_exit", command.Required)
}

func (e *Executor) validateCommand(command Command) error {
	if strings.TrimSpace(command.Name) == "" || containsNUL(command.Name) {
		return fmt.Errorf("name is required")
	}
	if command.Directory == "" || containsNUL(command.Directory) || !filepath.IsAbs(command.Directory) || containsParentDirectory(command.Directory) {
		return fmt.Errorf("directory must be an absolute compiled path")
	}
	if command.Executable == "" || containsNUL(command.Executable) || strings.ContainsAny(command.Executable, " \t\r\n") || isShell(command.Executable) {
		return fmt.Errorf("executable must be one shell-free program")
	}
	if len(command.Args) > e.limits.MaxArguments {
		return fmt.Errorf("too many arguments")
	}
	for _, argument := range command.Args {
		if containsNUL(argument) {
			return fmt.Errorf("argument contains NUL byte")
		}
	}
	if command.Timeout < time.Second || command.Timeout > e.limits.MaxTimeout {
		return fmt.Errorf("timeout is outside executor limits")
	}
	if err := validateEnvironmentOverrides(command.Environment); err != nil {
		return err
	}
	return nil
}

func finishStartFailure(result Result, output *executil.LimitedBuffer, startErr error, ctx, childCtx context.Context, required bool) Result {
	result.Output = string(output.Bytes())
	result.Truncated = output.Truncated()
	if err := ctx.Err(); err != nil {
		return failedResult(result, failureCanceled, "caller_canceled", required)
	}
	if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
		return failedResult(result, failureEnvironment, "timeout", required)
	}
	if errors.Is(startErr, exec.ErrNotFound) || errors.Is(startErr, fs.ErrNotExist) || errors.Is(startErr, fs.ErrPermission) {
		return failedResult(result, failureEnvironment, "start", required)
	}
	return failedResult(result, failureInternal, "start", required)
}

func failedResult(result Result, failureClass, causeCode string, required bool) Result {
	result.Passed = false
	result.FailureClass = failureClass
	result.CauseCode = causeCode
	result.Warning = !required && (failureClass == failureEnvironment || failureClass == failureVerification)
	return result
}

func isDockerEnvironmentFailure(executable, output string) bool {
	if !strings.EqualFold(filepath.Base(executable), "docker") {
		return false
	}
	output = strings.ToLower(output)
	for _, diagnostic := range dockerEnvironmentDiagnostics {
		if strings.Contains(output, diagnostic) {
			return true
		}
	}
	return false
}

func exactEnvironment(values map[string]string) ([]string, error) {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, fmt.Errorf("invalid environment key")
		}
		if strings.IndexByte(values[key], 0) >= 0 {
			return nil, fmt.Errorf("invalid environment value")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment, nil
}

func resultCommand(command Command) Command {
	command.Args = append([]string(nil), command.Args...)
	command.Environment = nil
	return command
}

func validateEnvironmentOverrides(overrides map[string]string) error {
	for key, value := range overrides {
		if key != "GOWORK" || value != "off" {
			return fmt.Errorf("environment overrides only permit GOWORK=off")
		}
	}
	return nil
}

func mergeEnvironment(base, overrides map[string]string) map[string]string {
	values := make(map[string]string, len(base)+len(overrides))
	for key, value := range base {
		values[key] = value
	}
	for key, value := range overrides {
		values[key] = value
	}
	return values
}

// ResolveExecutable resolves an executable using only the supplied PATH. Empty
// and relative PATH entries are evaluated from directory, matching child-process
// lookup instead of the worker's current directory.
func ResolveExecutable(executable, directory string, environment map[string]string) (string, error) {
	if strings.ContainsRune(executable, filepath.Separator) {
		if !filepath.IsAbs(executable) {
			executable = filepath.Join(directory, executable)
		}
		if isExecutable(executable) {
			return executable, nil
		}
		return "", exec.ErrNotFound
	}
	path, ok := environment["PATH"]
	if !ok {
		return "", exec.ErrNotFound
	}
	for _, pathDirectory := range filepath.SplitList(path) {
		if pathDirectory == "" {
			pathDirectory = directory
		} else if !filepath.IsAbs(pathDirectory) {
			pathDirectory = filepath.Join(directory, pathDirectory)
		}
		candidate := filepath.Join(pathDirectory, executable)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
