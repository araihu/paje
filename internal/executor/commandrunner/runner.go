// Package commandrunner adapts repository-relative verification commands to
// one-shot provider-neutral executor requests.
package commandrunner

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
)

const cleanupTimeout = 5 * time.Second

type Config struct {
	Executor    executor.Executor
	Profile     workerprofile.Snapshot
	Attempt     executor.AttemptID
	Workspace   string
	Environment map[string]string
	OutputLimit int64
	Writable    bool
}

type Runner struct {
	target      executor.Executor
	profile     workerprofile.Snapshot
	attempt     executor.AttemptID
	workspace   string
	environment map[string]string
	outputLimit int64
	writable    bool
	sequence    atomic.Int64
}

func New(config Config) (*Runner, error) {
	if nilExecutor(config.Executor) || config.OutputLimit <= 0 || config.Workspace == "" || !filepath.IsAbs(config.Workspace) ||
		filepath.Clean(config.Workspace) != config.Workspace || config.Workspace == string(filepath.Separator) {
		return nil, errors.New("command runner configuration is invalid")
	}
	if err := config.Attempt.Validate(); err != nil ||
		(config.Attempt.Purpose != executor.PurposeProbe && config.Attempt.Purpose != executor.PurposeVerification) {
		return nil, errors.New("command runner attempt must be probe or verification")
	}
	canonical, err := workerprofile.Canonicalize(config.Profile)
	if err != nil || config.Profile.Digest == "" || !reflect.DeepEqual(canonical, config.Profile) {
		return nil, errors.New("command runner profile is not exact")
	}
	runner := &Runner{
		target: config.Executor, profile: config.Profile.Clone(), attempt: config.Attempt,
		workspace: config.Workspace, environment: cloneMap(config.Environment),
		outputLimit: config.OutputLimit, writable: config.Writable,
	}
	runner.sequence.Store(int64(config.Attempt.Sequence))
	return runner, nil
}

func (runner *Runner) Run(ctx context.Context, command verification.Command) verification.Result {
	result := verification.Result{Command: evidenceCommand(command)}
	if runner == nil {
		return failed(result, "internal", "runner_nil", command.Required)
	}
	directory, ok := sandboxDirectory(command.Directory)
	if !ok {
		return failed(result, "internal", "invalid_command", command.Required)
	}
	sequence := runner.sequence.Add(1)
	if sequence <= 0 || sequence > int64(^uint(0)>>1) {
		return failed(result, "internal", "sequence_exhausted", command.Required)
	}
	attempt := runner.attempt
	attempt.Sequence = int(sequence)
	request := executor.Request{
		Attempt: attempt, Profile: runner.profile.Clone(),
		Command: executor.Command{
			Executable: command.Executable, Args: append([]string(nil), command.Args...), Directory: directory,
			Environment: cloneMap(command.Environment),
		},
		Workspace: executor.Workspace{
			HostPath: runner.workspace, SandboxPath: executor.SandboxWorkspaceRoot, Writable: runner.writable,
		},
		Environment: cloneMap(runner.environment), Timeout: command.Timeout, OutputLimit: runner.outputLimit,
	}
	defer request.Destroy()
	execution, executeErr := runner.target.Execute(ctx, request)
	defer execution.Destroy()
	result.ExitCode = execution.ExitCode
	result.Duration = execution.Duration
	if !execution.SecretDetected {
		result.Output = string(append(append([]byte(nil), execution.Stdout...), execution.Stderr...))
	}
	result.Truncated = execution.StdoutTruncated || execution.StderrTruncated
	classify(&result, execution, executeErr, command.Required)

	if execution.Created || ambiguousAttempt(executeErr) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		destroyErr := runner.target.Destroy(cleanupCtx, attempt)
		cancel()
		if destroyErr != nil {
			return failed(result, "cleanup", "destroy", command.Required)
		}
	}
	return result
}

func ambiguousAttempt(err error) bool {
	var providerError *executor.ProviderError
	return errors.As(err, &providerError) && providerError.CauseCode == "ambiguous_attempt"
}

func nilExecutor(target executor.Executor) bool {
	if target == nil {
		return true
	}
	value := reflect.ValueOf(target)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func sandboxDirectory(directory string) (string, bool) {
	directory = filepath.ToSlash(strings.TrimSpace(directory))
	if directory == "" {
		directory = "."
	}
	if strings.HasPrefix(directory, "/") || path.Clean(directory) != directory {
		return "", false
	}
	for _, component := range strings.Split(directory, "/") {
		if component == ".." {
			return "", false
		}
	}
	if directory == "." {
		return executor.SandboxWorkspaceRoot, true
	}
	return path.Join(executor.SandboxWorkspaceRoot, directory), true
}

func classify(result *verification.Result, execution executor.Result, err error, required bool) {
	switch {
	case execution.SecretDetected:
		*result = failed(*result, "policy", "secret_detected", required)
	case err != nil:
		var providerError *executor.ProviderError
		if errors.As(err, &providerError) {
			*result = failed(*result, providerError.Class, providerError.CauseCode, required)
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			*result = failed(*result, "canceled", "caller_canceled", required)
		} else {
			*result = failed(*result, "internal", "execute", required)
		}
	case !execution.Created || !execution.Started || !execution.Completed:
		*result = failed(*result, "internal", "incomplete_result", required)
	case execution.ExitCode != 0:
		*result = failed(*result, "verification", "nonzero_exit", required)
	default:
		result.Passed = true
	}
}

func failed(result verification.Result, class, cause string, required bool) verification.Result {
	result.Passed = false
	result.FailureClass = class
	result.CauseCode = cause
	result.Warning = !required && (class == "environment" || class == "verification")
	return result
}

func evidenceCommand(command verification.Command) verification.Command {
	command.Args = append([]string(nil), command.Args...)
	command.Environment = nil
	return command
}

func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
