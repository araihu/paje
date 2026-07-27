// Package host implements explicitly enabled, secret-free development
// execution on the coordinator host. It is not an isolation boundary.
package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/araihu/paje/internal/executil"
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
)

const (
	cancelWait            = 5 * time.Second
	destroyedHistoryLimit = 1024
)

type Config struct {
	Enabled        bool
	ProductionOnly bool
}

type attemptRecord struct {
	state           executor.State
	command         *exec.Cmd
	done            chan struct{}
	cancelRequested bool
}

type Executor struct {
	mu             sync.Mutex
	attempts       map[string]*attemptRecord
	destroyedOrder []string
	resolve        func(string, string, map[string]string) (string, error)
}

func New(config Config) (*Executor, error) {
	if !config.Enabled {
		return nil, errors.New("host executor is not explicitly enabled")
	}
	if config.ProductionOnly {
		return nil, errors.New("host executor is forbidden in production-only mode")
	}
	return &Executor{
		attempts:       make(map[string]*attemptRecord),
		destroyedOrder: make([]string, 0, destroyedHistoryLimit),
		resolve:        verification.ResolveExecutable,
	}, nil
}

func (*Executor) ValidateProfile(profile workerprofile.Snapshot) error {
	canonical, err := workerprofile.Canonicalize(profile)
	if err != nil || profile.Digest == "" || !reflect.DeepEqual(canonical, profile) {
		return errors.New("host executor profile is not exact and canonical")
	}
	if profile.Runtime.Kind != workerprofile.RuntimeHost || len(profile.Secrets) != 0 {
		return errors.New("host executor accepts only secret-free host profiles")
	}
	return nil
}

func (target *Executor) Execute(ctx context.Context, request executor.Request) (result executor.Result, returnedErr error) {
	if target == nil {
		return executor.Result{}, errors.New("host executor is nil")
	}
	if err := ctx.Err(); err != nil {
		return executor.Result{}, err
	}
	if err := request.Validate(); err != nil {
		return executor.Result{}, err
	}
	if err := target.ValidateProfile(request.Profile); err != nil || len(request.Secrets) != 0 {
		return executor.Result{}, errors.New("unsafe host executor request")
	}

	key := request.Attempt.Key()
	record := &attemptRecord{state: executor.StateCreated, done: make(chan struct{})}
	target.mu.Lock()
	if _, collision := target.attempts[key]; collision {
		target.mu.Unlock()
		return executor.Result{}, executor.ErrAttemptExists
	}
	target.attempts[key] = record
	target.mu.Unlock()

	finalState := executor.StateCreated
	defer func() {
		target.mu.Lock()
		if current := target.attempts[key]; current == record {
			if current.state != executor.StateDestroyed {
				current.state = finalState
			}
			current.command = nil
			close(current.done)
		}
		target.mu.Unlock()
	}()

	result = executor.Result{Created: true, SafeFacts: hostSafeFacts()}
	directory, environment, executable, err := target.prepare(request)
	if err != nil {
		return result, executor.WrapError("environment", "prepare", err)
	}
	stdout, err := executil.NewLimitedBuffer(request.OutputLimit)
	if err != nil {
		return result, executor.WrapError("internal", "stdout_buffer", err)
	}
	stderr, err := executil.NewLimitedBuffer(request.OutputLimit)
	if err != nil {
		return result, executor.WrapError("internal", "stderr_buffer", err)
	}

	childCtx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	command := exec.CommandContext(childCtx, executable, request.Command.Args...)
	executil.Configure(command)
	command.Dir = directory
	command.Env = environment
	command.Stdout = stdout
	command.Stderr = stderr
	receipt, err := executor.NewRandomChildStartReceipt(
		request.Attempt,
		request.Command,
		request.Environment,
		nil,
	)
	if err != nil {
		return result, executor.WrapError("internal", "receipt_binding", err)
	}

	startedAt := time.Now()
	target.mu.Lock()
	if record.cancelRequested || childCtx.Err() != nil {
		target.mu.Unlock()
		result.Duration = time.Since(startedAt)
		return result, executor.WrapError("canceled", "before_start", context.Canceled)
	}
	record.command = command
	startErr := command.Start()
	if startErr == nil {
		record.state = executor.StateRunning
		result.Started = true
		result.ChildStartReceipt = &receipt
	}
	target.mu.Unlock()
	if startErr != nil {
		result.Duration = time.Since(startedAt)
		copyOutput(&result, stdout, stderr)
		return result, executor.WrapError("environment", "start", startErr)
	}

	waitErr := command.Wait()
	result.Duration = time.Since(startedAt)
	copyOutput(&result, stdout, stderr)
	var exitErr *exec.ExitError
	if waitErr == nil || errors.As(waitErr, &exitErr) {
		result.Completed = true
		finalState = executor.StateCompleted
	}
	if errors.As(waitErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	if waitErr == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return result, executor.WrapError("canceled", "caller_canceled", ctx.Err())
	}
	if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
		return result, executor.WrapError("environment", "timeout", childCtx.Err())
	}
	target.mu.Lock()
	canceled := record.cancelRequested
	target.mu.Unlock()
	if canceled {
		return result, executor.WrapError("canceled", "executor_canceled", waitErr)
	}
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() >= 0 {
		return result, nil
	}
	finalState = executor.StateUnknown
	return result, executor.WrapError("internal", "wait", waitErr)
}

func (target *Executor) Inspect(ctx context.Context, attempt executor.AttemptID) (executor.State, error) {
	if target == nil {
		return executor.StateUnknown, errors.New("host executor is nil")
	}
	if err := ctx.Err(); err != nil {
		return executor.StateUnknown, err
	}
	if err := attempt.Validate(); err != nil {
		return executor.StateUnknown, err
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	record, ok := target.attempts[attempt.Key()]
	if !ok {
		return executor.StateAbsent, nil
	}
	return record.state, nil
}

func (target *Executor) Cancel(ctx context.Context, attempt executor.AttemptID) error {
	if target == nil {
		return errors.New("host executor is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := attempt.Validate(); err != nil {
		return err
	}
	target.mu.Lock()
	record, ok := target.attempts[attempt.Key()]
	if !ok || record.state == executor.StateDestroyed || record.state == executor.StateCompleted {
		target.mu.Unlock()
		return nil
	}
	record.cancelRequested = true
	command := record.command
	done := record.done
	target.mu.Unlock()

	var signalErr error
	if command != nil && command.Cancel != nil {
		if err := command.Cancel(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			signalErr = err
		}
	}
	timer := time.NewTimer(cancelWait)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return executor.WrapError("cleanup", "cancel_timeout", errors.Join(
			errors.New("host process termination was not confirmed"), signalErr,
		))
	}
}

func (target *Executor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	if target == nil {
		return errors.New("host executor is nil")
	}
	if err := target.Cancel(ctx, attempt); err != nil {
		return err
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	key := attempt.Key()
	record, ok := target.attempts[key]
	if !ok {
		return nil
	}
	if record.state == executor.StateDestroyed {
		return nil
	}
	record.state = executor.StateDestroyed
	record.command = nil
	target.destroyedOrder = append(target.destroyedOrder, key)
	for len(target.destroyedOrder) > destroyedHistoryLimit {
		oldest := target.destroyedOrder[0]
		target.destroyedOrder[0] = ""
		target.destroyedOrder = target.destroyedOrder[1:]
		if oldRecord, exists := target.attempts[oldest]; exists && oldRecord.state == executor.StateDestroyed {
			delete(target.attempts, oldest)
		}
	}
	return nil
}

func (target *Executor) prepare(request executor.Request) (string, []string, string, error) {
	workspace, err := filepath.EvalSymlinks(request.Workspace.HostPath)
	if err != nil {
		return "", nil, "", err
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return "", nil, "", errors.New("host workspace is not a directory")
	}
	relative, err := filepath.Rel(request.Workspace.SandboxPath, request.Command.Directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", nil, "", errors.New("host command directory escapes workspace")
	}
	directory, err := filepath.EvalSymlinks(filepath.Join(workspace, relative))
	if err != nil || !pathWithin(workspace, directory) {
		return "", nil, "", errors.New("host command directory escapes workspace")
	}
	values := make(map[string]string, len(request.Environment)+len(request.Command.Environment))
	for key, value := range request.Environment {
		values[key] = value
	}
	for key, value := range request.Command.Environment {
		values[key] = value
	}
	environment := exactEnvironment(values)
	if target.resolve == nil {
		return "", nil, "", errors.New("host executable resolver is unavailable")
	}
	executable, err := target.resolve(request.Command.Executable, directory, values)
	if err != nil {
		return "", nil, "", err
	}
	return directory, environment, executable, nil
}

func exactEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func copyOutput(result *executor.Result, stdout, stderr *executil.LimitedBuffer) {
	result.Stdout = stdout.Bytes()
	result.Stderr = stderr.Bytes()
	result.StdoutTruncated = stdout.Truncated()
	result.StderrTruncated = stderr.Truncated()
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func hostSafeFacts() map[string]string {
	return map[string]string{"runtime_kind": workerprofile.RuntimeHost, "certified": "false", "isolated": "false"}
}

var (
	_ executor.Executor         = (*Executor)(nil)
	_ executor.ProfileValidator = (*Executor)(nil)
)
