package verification_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/araihu/paje/internal/verification"
)

func TestVerificationHelper(t *testing.T) {
	if os.Getenv("GO_WANT_VERIFY_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 || len(args) <= separator+1 {
		os.Exit(2)
	}

	switch args[separator+1] {
	case "exit-7":
		os.Exit(7)
	case "docker-rootless":
		fmt.Fprintln(os.Stderr, "rootless Docker not found")
		os.Exit(1)
	case "docker-daemon":
		fmt.Fprintln(os.Stderr, "Cannot connect to the Docker daemon")
		os.Exit(1)
	case "output":
		fmt.Fprint(os.Stdout, args[separator+2])
	case "descendant":
		pidFile := args[separator+2]
		child := mustStartHelper(t, "hold")
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(10 * time.Second)
	case "hold":
		time.Sleep(10 * time.Second)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestExecutorClassifiesCompletedNonzeroExit(t *testing.T) {
	workspace := t.TempDir()
	executor := newExecutor(t, 1024)
	result := executor.Run(context.Background(), verification.Command{
		Name:       "exit seven",
		Directory:  workspace,
		Executable: os.Args[0],
		Args:       helperArgs("exit-7"),
		Timeout:    5 * time.Second,
		Required:   true,
	}, helperEnvironment())
	if result.ExitCode != 7 || result.FailureClass != "verification" || result.Passed || result.Warning {
		t.Fatalf("result = %#v", result)
	}
}

func TestExecutorRejectsDirectoryEscapeBeforeStart(t *testing.T) {
	executor := newExecutor(t, 1024)
	marker := filepath.Join(t.TempDir(), "started")
	result := executor.Run(context.Background(), verification.Command{
		Name:       "escape",
		Directory:  "../outside",
		Executable: os.Args[0],
		Args:       helperArgs("output", marker),
		Timeout:    5 * time.Second,
		Required:   true,
	}, helperEnvironment())
	if result.FailureClass != "internal" || result.Passed {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("helper started despite rejected directory: %v", err)
	}
}

func TestExecutorClassifiesEnvironmentFailuresAndOptionalWarnings(t *testing.T) {
	workspace := t.TempDir()
	executor := newExecutor(t, 1024)

	for _, testCase := range []struct {
		name     string
		command  verification.Command
		required bool
	}{
		{
			name:    "missing executable",
			command: verification.Command{Name: "missing", Directory: workspace, Executable: filepath.Join(workspace, "missing"), Timeout: time.Second},
		},
		{
			name:    "rootless docker",
			command: dockerCommand(t, workspace, "docker-rootless"),
		},
		{
			name:    "docker daemon",
			command: dockerCommand(t, workspace, "docker-daemon"),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			optional := testCase.command
			optional.Required = false
			result := executor.Run(context.Background(), optional, helperEnvironment())
			if result.FailureClass != "environment" || result.Passed || !result.Warning {
				t.Fatalf("optional result = %#v", result)
			}

			required := testCase.command
			required.Required = true
			result = executor.Run(context.Background(), required, helperEnvironment())
			if result.FailureClass != "environment" || result.Passed || result.Warning {
				t.Fatalf("required result = %#v", result)
			}
		})
	}
}

func TestExecutorTimeoutKillsDescendant(t *testing.T) {
	workspace := t.TempDir()
	pidFile := filepath.Join(workspace, "descendant.pid")
	executor := newExecutor(t, 1024)
	started := time.Now()
	result := executor.Run(context.Background(), verification.Command{
		Name:       "timeout",
		Directory:  workspace,
		Executable: os.Args[0],
		Args:       helperArgs("descendant", pidFile),
		Timeout:    time.Second,
		Required:   true,
	}, helperEnvironment())
	if result.FailureClass != "environment" || result.Passed || result.Warning {
		t.Fatalf("result = %#v", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout run took %s, want process group cancellation promptly", elapsed)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", pidFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("Atoi(%q) error = %v", pidBytes, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant pid %d remained alive: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecutorBoundsCombinedOutputExactly(t *testing.T) {
	workspace := t.TempDir()
	executor := newExecutor(t, 4)
	result := executor.Run(context.Background(), verification.Command{
		Name:       "output",
		Directory:  workspace,
		Executable: os.Args[0],
		Args:       helperArgs("output", "abcdef"),
		Timeout:    5 * time.Second,
		Required:   true,
	}, helperEnvironment())
	if !result.Passed || result.Truncated != true || result.Output != "abcd" {
		t.Fatalf("result = %#v", result)
	}
}

func newExecutor(t *testing.T, maxOutput int64) *verification.Executor {
	t.Helper()
	executor, err := verification.NewExecutor(verification.Limits{
		MaxArguments:   16,
		MaxTimeout:     time.Minute,
		MaxOutputBytes: maxOutput,
	})
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	return executor
}

func helperArgs(action string, values ...string) []string {
	return append([]string{"-test.run=TestVerificationHelper", "--", action}, values...)
}

func helperEnvironment() map[string]string {
	return map[string]string{"GO_WANT_VERIFY_HELPER": "1"}
}

func dockerCommand(t *testing.T, workspace, action string) verification.Command {
	t.Helper()
	docker := filepath.Join(t.TempDir(), "docker")
	if err := os.Symlink(os.Args[0], docker); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	return verification.Command{
		Name:       action,
		Directory:  workspace,
		Executable: docker,
		Args:       helperArgs(action),
		Timeout:    time.Second,
	}
}

func mustStartHelper(t *testing.T, action string) *os.Process {
	t.Helper()
	process, err := os.StartProcess(os.Args[0], append([]string{os.Args[0]}, helperArgs(action)...), &os.ProcAttr{
		Env: helperEnvironmentList(),
	})
	if err != nil {
		t.Fatalf("StartProcess() error = %v", err)
	}
	return process
}

func helperEnvironmentList() []string {
	return []string{"GO_WANT_VERIFY_HELPER=1"}
}
