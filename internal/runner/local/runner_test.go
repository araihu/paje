package local_test

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	default:
		fmt.Fprintf(os.Stderr, "unknown helper task %q", task)
		os.Exit(2)
	}
	os.Exit(0)
}
