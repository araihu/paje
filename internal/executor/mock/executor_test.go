package mock

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestValidateProfileRejectsNoncanonicalSnapshotOrder(t *testing.T) {
	target := New()
	request := validRequest(t)
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: request.Profile.APIVersion,
		Kind:       request.Profile.Kind,
		Metadata:   request.Profile.Metadata,
		Runtime:    request.Profile.Runtime,
		Harness:    request.Profile.Harness,
		Tools: []workerprofile.Tool{
			{Name: "go", Version: "1.26.1", Probe: workerprofile.Probe{Executable: "go", Args: []string{"version"}, OutputContains: "go1.26.1"}},
			{Name: "git", Version: "2.53.0", Probe: workerprofile.Probe{Executable: "git", Args: []string{"--version"}, OutputContains: "2.53.0"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(profile.Tools)
	if err := target.ValidateProfile(profile); err == nil {
		t.Fatal("noncanonical snapshot order accepted")
	}
}

func TestExecutorRecordsDefensiveRequestsAndLifecycle(t *testing.T) {
	target := New()
	request := validRequest(t)
	target.SetResult(request.Attempt, executor.Result{
		Created: true, Started: true, Completed: true, ExitCode: 0,
		Stdout: []byte("ok"), SafeFacts: map[string]string{"runtime": "mock"},
	}, nil)

	got, err := target.Execute(context.Background(), request)
	if err != nil || string(got.Stdout) != "ok" {
		t.Fatalf("Execute() = %#v, %v", got, err)
	}
	got.Stdout[0] = 'X'
	requests := target.Requests()
	if len(requests) != 1 || requests[0].Command.Args[0] != "version" {
		t.Fatalf("Requests() = %#v", requests)
	}
	requests[0].Command.Args[0] = "mutated"
	requests[0].Destroy()
	if target.Requests()[0].Command.Args[0] != "version" {
		t.Fatal("Requests returned mock-owned storage")
	}

	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateCompleted {
		t.Fatalf("Inspect() = %q, %v", state, err)
	}
	if err := target.Cancel(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := target.Cancel(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := target.Destroy(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := target.Destroy(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}
	state, err = target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateDestroyed {
		t.Fatalf("Inspect() after destroy = %q, %v", state, err)
	}
	request.Destroy()
}

func TestExecutorRejectsIdentityCollisionAndReturnsConfiguredErrors(t *testing.T) {
	target := New()
	request := validRequest(t)
	want := errors.New("provider unavailable")
	target.SetResult(request.Attempt, executor.Result{}, want)
	if _, err := target.Execute(context.Background(), request); !errors.Is(err, want) {
		t.Fatalf("Execute() error = %v", err)
	}

	target.SetResult(request.Attempt, executor.Result{Created: true, Started: true}, nil)
	if _, err := target.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Execute(context.Background(), request); !errors.Is(err, executor.ErrAttemptExists) {
		t.Fatalf("duplicate Execute() error = %v", err)
	}
	request.Destroy()
}

func TestExecutorPreservesCreatedStateWhenStartFails(t *testing.T) {
	target := New()
	request := validRequest(t)
	target.SetResult(request.Attempt, executor.Result{Created: true}, errors.New("start failed"))
	if _, err := target.Execute(context.Background(), request); err == nil {
		t.Fatal("Execute() succeeded")
	}
	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateCreated {
		t.Fatalf("Inspect() = %q, %v", state, err)
	}
	request.Destroy()
}

func validRequest(t *testing.T) executor.Request {
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
	return executor.Request{
		Attempt: executor.AttemptID{
			RunID: "run-1", Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeProbe,
		},
		Profile:     profile,
		Command:     executor.Command{Executable: "go", Args: []string{"version"}, Directory: "/workspace"},
		Workspace:   executor.Workspace{HostPath: t.TempDir(), SandboxPath: "/workspace"},
		Environment: map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout:     time.Minute, OutputLimit: 1024,
	}
}
