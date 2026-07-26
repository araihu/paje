package commandrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestNewRejectsTypedNilExecutorAndNoncanonicalProfile(t *testing.T) {
	config := Config{
		Profile: commandProfile(t), Attempt: commandAttempt(), Workspace: t.TempDir(),
		Environment: map[string]string{"PATH": "/usr/bin:/bin"}, OutputLimit: 1024,
	}
	var typedNil *recordingExecutor
	config.Executor = typedNil
	if _, err := New(config); err == nil {
		t.Fatal("typed-nil executor accepted")
	}

	config.Executor = &recordingExecutor{}
	profile := config.Profile.Clone()
	profile.Digest = ""
	profile.Tools = []workerprofile.Tool{
		{Name: "go", Version: "1.26.1", Probe: workerprofile.Probe{Executable: "go", Args: []string{"version"}, OutputContains: "go1.26.1"}},
		{Name: "git", Version: "2.53.0", Probe: workerprofile.Probe{Executable: "git", Args: []string{"--version"}, OutputContains: "2.53.0"}},
	}
	profile, err := workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(profile.Tools)
	config.Profile = profile
	if _, err := New(config); err == nil {
		t.Fatal("noncanonical profile order accepted")
	}
}

func TestRunnerDestroysAndSuppressesSecretDetectedOutputEverywhere(t *testing.T) {
	const secretSentinel = "verification-secret-sentinel"
	target := &recordingExecutor{result: executor.Result{
		Created: true, Started: true, Completed: true, SecretDetected: true,
		Stdout: []byte(secretSentinel), Stderr: []byte(secretSentinel),
	}}
	runner, err := New(Config{
		Executor: target, Profile: commandProfile(t), Attempt: commandAttempt(),
		Workspace: t.TempDir(), Environment: map[string]string{"PATH": "/usr/bin:/bin"}, OutputLimit: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runner.Run(context.Background(), verification.Command{
		Name: "git status", Directory: ".", Executable: "git", Timeout: time.Minute, Required: true,
	})
	if got.Passed || got.FailureClass != "policy" || got.CauseCode != "secret_detected" || got.Output != "" {
		t.Fatalf("Run() = %#v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	formattedValues := map[string]string{"json": string(encoded)}
	for _, format := range []string{"%v", "%+v", "%#v", "%q", "%s"} {
		formattedValues[format] = fmt.Sprintf(format, got)
	}
	for format, formatted := range formattedValues {
		if strings.Contains(formatted, secretSentinel) {
			t.Fatalf("%s leaked secret-detected output: %s", format, formatted)
		}
	}
	if string(target.returnedStdout) != strings.Repeat("\x00", len(secretSentinel)) ||
		string(target.returnedStderr) != strings.Repeat("\x00", len(secretSentinel)) {
		t.Fatal("runner did not destroy transient executor output")
	}

	profile, err := repository.NewGenericProfile(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	_, profileErr := profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace: t.TempDir(), Commands: runner,
	})
	if profileErr == nil || strings.Contains(profileErr.Error(), secretSentinel) {
		t.Fatalf("repository profile error = %v", profileErr)
	}
}

func TestRunnerTranslatesRelativeCommandAndCleansOneShotSandbox(t *testing.T) {
	target := &recordingExecutor{result: executor.Result{
		Created: true, Started: true, Completed: true,
		Stdout: []byte("out"), Stderr: []byte("err"), Duration: time.Second,
	}}
	runner, err := New(Config{
		Executor: target, Profile: commandProfile(t), Attempt: commandAttempt(),
		Workspace: t.TempDir(), Environment: map[string]string{"PATH": "/usr/bin:/bin"},
		OutputLimit: 1024, Writable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	command := verification.Command{
		Name: "go test", Directory: "site", Executable: "go", Args: []string{"test", "./..."},
		Environment: map[string]string{"GOWORK": "off"}, Timeout: time.Minute, Required: true,
	}
	got := runner.Run(context.Background(), command)
	if !got.Passed || got.Output != "outerr" || got.Command.Directory != "site" || got.Duration != time.Second {
		t.Fatalf("Run() = %#v", got)
	}
	if target.destroyCalls != 1 || len(target.requests) != 1 {
		t.Fatalf("execute/destroy calls = %d/%d", len(target.requests), target.destroyCalls)
	}
	request := target.requests[0]
	if request.Command.Directory != "/workspace/site" || request.Attempt.Purpose != executor.PurposeVerification || request.Attempt.Sequence != 5 ||
		!reflect.DeepEqual(request.Command.Args, command.Args) || len(request.Secrets) != 0 {
		t.Fatalf("executor request = %#v", request)
	}
	request.Destroy()
}

func TestRunnerCleanupFailureOverridesSuccess(t *testing.T) {
	target := &recordingExecutor{
		result:     executor.Result{Created: true, Started: true, Completed: true},
		destroyErr: errors.New("destroy failed"),
	}
	runner, err := New(Config{
		Executor: target, Profile: commandProfile(t), Attempt: commandAttempt(),
		Workspace: t.TempDir(), Environment: map[string]string{"PATH": "/usr/bin:/bin"}, OutputLimit: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runner.Run(context.Background(), verification.Command{
		Name: "go version", Directory: ".", Executable: "go", Timeout: time.Minute, Required: true,
	})
	if got.Passed || got.FailureClass != "cleanup" || got.CauseCode != "destroy" {
		t.Fatalf("Run() = %#v", got)
	}
}

func TestRunnerDoesNotDestroyCollidingAttemptItDidNotCreate(t *testing.T) {
	target := &recordingExecutor{err: executor.ErrAttemptExists}
	runner, err := New(Config{
		Executor: target, Profile: commandProfile(t), Attempt: commandAttempt(),
		Workspace: t.TempDir(), Environment: map[string]string{"PATH": "/usr/bin:/bin"}, OutputLimit: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runner.Run(context.Background(), verification.Command{
		Name: "go version", Directory: ".", Executable: "go", Timeout: time.Minute, Required: true,
	})
	if got.Passed || target.destroyCalls != 0 {
		t.Fatalf("Run()/destroy = %#v / %d", got, target.destroyCalls)
	}
}

func TestRunnerDestroysAmbiguousAttemptWithoutCreatedEvidence(t *testing.T) {
	target := &recordingExecutor{
		err: executor.WrapError("internal", "ambiguous_attempt", errors.New("create response lost")),
	}
	runner, err := New(Config{
		Executor: target, Profile: commandProfile(t), Attempt: commandAttempt(),
		Workspace: t.TempDir(), Environment: map[string]string{"PATH": "/usr/bin:/bin"}, OutputLimit: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runner.Run(context.Background(), verification.Command{
		Name: "go version", Directory: ".", Executable: "go", Timeout: time.Minute, Required: true,
	})
	if got.Passed || got.FailureClass != "internal" || got.CauseCode != "ambiguous_attempt" {
		t.Fatalf("Run() = %#v", got)
	}
	if target.destroyCalls != 1 {
		t.Fatalf("ambiguous attempt cleanup calls = %d, want 1", target.destroyCalls)
	}
}

type recordingExecutor struct {
	requests       []executor.Request
	result         executor.Result
	err            error
	destroyErr     error
	destroyCalls   int
	returnedStdout []byte
	returnedStderr []byte
}

func (target *recordingExecutor) Execute(_ context.Context, request executor.Request) (executor.Result, error) {
	target.requests = append(target.requests, request.Clone())
	result := target.result.Clone()
	target.returnedStdout = result.Stdout
	target.returnedStderr = result.Stderr
	return result, target.err
}
func (*recordingExecutor) Inspect(context.Context, executor.AttemptID) (executor.State, error) {
	return executor.StateAbsent, nil
}
func (*recordingExecutor) Cancel(context.Context, executor.AttemptID) error { return nil }
func (target *recordingExecutor) Destroy(context.Context, executor.AttemptID) error {
	target.destroyCalls++
	return target.destroyErr
}

func commandAttempt() executor.AttemptID {
	return executor.AttemptID{
		RunID: "run-1", Stage: "execute", Attempt: 1, StartedAt: time.Unix(100, 1).UTC(),
		Purpose: executor.PurposeVerification, Sequence: 4,
	}
}

func commandProfile(t *testing.T) workerprofile.Snapshot {
	t.Helper()
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "host-dev", Revision: 1},
		Runtime:    workerprofile.Runtime{Kind: workerprofile.RuntimeHost},
		Harness:    workerprofile.Harness{ID: "codex", Version: "0.144.5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
