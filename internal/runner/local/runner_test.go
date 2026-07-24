package local_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/runner/local"
)

func TestRunPassesTaskWorkingDirectoryAndEnvironment(t *testing.T) {
	t.Parallel()

	executor, err := local.New(os.Args[0], "-test.run=TestHelperProcess", "--")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	workspace := t.TempDir()

	result, err := executor.Run(context.Background(), runner.RunRequest{
		TaskDescription: "inspect",
		WorkspacePath:   workspace,
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"PAJE_TEST_VALUE":        "from-request",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", workspace, err)
	}
	for _, want := range []string{
		"task=inspect",
		"cwd=" + resolvedWorkspace,
		"env=from-request",
		"stderr=combined",
	} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("Output = %q, want it to contain %q", result.Output, want)
		}
	}
	if result.Duration < 0 {
		t.Errorf("Duration = %f, want non-negative", result.Duration)
	}
}

func TestRunUsesOnlyRequestEnvironment(t *testing.T) {
	t.Setenv("PAJE_PARENT_SECRET", "must-not-leak")

	executor, err := local.New(os.Args[0], "-test.run=TestHelperProcess", "--")
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Run(context.Background(), runner.RunRequest{
		TaskDescription: "print-parent-secret",
		WorkspacePath:   t.TempDir(),
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Output, "must-not-leak") {
		t.Fatalf("ambient secret leaked: %q", result.Output)
	}
}

func TestRunBoundsCombinedOutput(t *testing.T) {
	executor, err := local.NewConfigured(
		os.Args[0],
		[]string{"-test.run=TestHelperProcess", "--"},
		32,
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := executor.Run(context.Background(), runner.RunRequest{
		TaskDescription: "large-output",
		WorkspacePath:   t.TempDir(),
		Env:             map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Transcript) != 32 {
		t.Fatalf("result = %#v, want 32 truncated bytes", got)
	}
}

func TestRunReturnsNonZeroExitAsResult(t *testing.T) {
	t.Parallel()

	executor, err := local.New(os.Args[0], "-test.run=TestHelperProcess", "--")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := executor.Run(context.Background(), runner.RunRequest{
		TaskDescription: "exit-7",
		WorkspacePath:   t.TempDir(),
		Env:             map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for process exit", err)
	}
	if result.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Output, "exiting") {
		t.Errorf("Output = %q, want captured process output", result.Output)
	}
}

func TestRunReturnsContextCancellation(t *testing.T) {
	t.Parallel()

	executor, err := local.New(os.Args[0], "-test.run=TestHelperProcess", "--")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, err = executor.Run(ctx, runner.RunRequest{
		TaskDescription: "sleep",
		WorkspacePath:   t.TempDir(),
		Env:             map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context deadline exceeded", err)
	}
}

func TestRunCancellationTerminatesDescendantProcesses(t *testing.T) {
	executor, err := local.New(os.Args[0], "-test.run=TestHelperProcess", "--")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workspace := t.TempDir()
	type runResult struct {
		result runner.ExecutionResult
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, err := executor.Run(ctx, runner.RunRequest{
			TaskDescription: "spawn-child",
			WorkspacePath:   workspace,
			Env:             map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		})
		done <- runResult{result: result, err: err}
	}()

	readyDeadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(filepath.Join(workspace, "child-started")); err == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatal("child process did not start within 1s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	started := time.Now()
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context canceled", got.err)
		}
		if !got.result.Started {
			t.Fatal("Started = false, want true")
		}
		if got.result.Completed {
			t.Fatal("Completed = true, want false for a canceled process")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("Run() took %v, want descendant process cancellation within 1s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not cancel descendant process within 1s")
	}
}

func TestNewRejectsEmptyCommand(t *testing.T) {
	t.Parallel()

	if _, err := local.New(" "); err == nil {
		t.Fatal("New() error = nil, want validation error")
	}
}

func TestRunRejectsIncompleteRequest(t *testing.T) {
	t.Parallel()

	executor, err := local.New("unused")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	testCases := []runner.RunRequest{
		{WorkspacePath: t.TempDir()},
		{TaskDescription: "task"},
	}
	for _, req := range testCases {
		if _, err := executor.Run(context.Background(), req); err == nil {
			t.Errorf("Run(%#v) error = nil, want validation error", req)
		}
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	task := os.Args[len(os.Args)-1]
	switch task {
	case "inspect":
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "getwd: %v", err)
			os.Exit(2)
		}
		fmt.Printf("task=%s cwd=%s env=%s\n", task, filepath.Clean(cwd), os.Getenv("PAJE_TEST_VALUE"))
		fmt.Fprintln(os.Stderr, "stderr=combined")
	case "exit-7":
		fmt.Println("exiting")
		os.Exit(7)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "print-parent-secret":
		fmt.Fprintln(os.Stdout, os.Getenv("PAJE_PARENT_SECRET"))
	case "large-output":
		fmt.Fprint(os.Stdout, strings.Repeat("o", 32))
		fmt.Fprint(os.Stderr, strings.Repeat("e", 32))
	case "spawn-child":
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "child-sleep")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start child: %v", err)
			os.Exit(2)
		}
		if err := os.WriteFile("child-started", []byte("1"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "mark child start: %v", err)
			os.Exit(2)
		}
		time.Sleep(10 * time.Second)
	case "child-sleep":
		time.Sleep(3 * time.Second)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper task %q", task)
		os.Exit(2)
	}
	os.Exit(0)
}
