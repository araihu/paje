package verification_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	case "exit-7-with-descendant":
		writeDescendantPIDAndExit(t, args[separator+2], args[separator+3], "hold-until-parent-reaped", 7)
	case "docker-rootless":
		fmt.Fprintln(os.Stderr, "rootless Docker not found")
		os.Exit(1)
	case "docker-rootless-with-descendant":
		fmt.Fprintln(os.Stderr, "rootless Docker not found")
		writeDescendantPIDAndExit(t, args[separator+2], args[separator+3], "hold-until-parent-reaped", 1)
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
	case "hold-until-parent-reaped":
		parentPID, err := strconv.Atoi(args[separator+2])
		if err != nil {
			os.Exit(4)
		}
		waitForParentReaped(parentPID)
		if err := os.WriteFile(args[separator+3], []byte(fmt.Sprintf("%d %d", os.Getpid(), parentPID)), 0o600); err != nil {
			os.Exit(5)
		}
		waitForRelease(args[separator+4])
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

func TestExecutorPreservesCompletedExitClassificationAfterContextCompletion(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		context  func() (context.Context, func())
		command  func(t *testing.T, workspace, readyFile, releaseFIFO string) verification.Command
		failure  string
		exitCode int
	}{
		{
			name: "caller cancellation after exit seven",
			context: func() (context.Context, func()) {
				ctx, cancel := context.WithCancel(context.Background())
				return ctx, cancel
			},
			command: func(_ *testing.T, workspace, readyFile, releaseFIFO string) verification.Command {
				return helperCommand(workspace, "exit-7-with-descendant", readyFile, releaseFIFO)
			},
			failure:  "verification",
			exitCode: 7,
		},
		{
			name: "caller deadline after docker diagnostic",
			context: func() (context.Context, func()) {
				ctx := newCompletionContext(context.DeadlineExceeded)
				return ctx, ctx.complete
			},
			command: func(t *testing.T, workspace, readyFile, releaseFIFO string) verification.Command {
				return dockerCommand(t, workspace, "docker-rootless-with-descendant", readyFile, releaseFIFO)
			},
			failure:  "environment",
			exitCode: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := t.TempDir()
			readyFile := filepath.Join(workspace, "parent-reaped")
			releaseFIFO := filepath.Join(workspace, "release")
			if err := syscall.Mkfifo(releaseFIFO, 0o600); err != nil {
				t.Fatalf("Mkfifo() error = %v", err)
			}
			ctx, complete := testCase.context()
			executor := newExecutor(t, 1024)
			command := testCase.command(t, workspace, readyFile, releaseFIFO)
			results := make(chan verification.Result, 1)
			go func() {
				results <- executor.Run(ctx, command, helperEnvironment())
			}()
			waitForFile(t, readyFile)
			descendantPID, executorProcessGroupID := readBoundaryPIDs(t, readyFile)
			releaseFD := -1
			cleanupActive := true
			t.Cleanup(func() {
				if cleanupActive {
					_ = syscall.Kill(descendantPID, syscall.SIGKILL)
					if releaseFD >= 0 {
						_ = syscall.Close(releaseFD)
					}
				}
			})
			descendantProcessGroupID, err := syscall.Getpgid(descendantPID)
			if err != nil {
				t.Fatalf("Getpgid(%d) error = %v", descendantPID, err)
			}
			if descendantProcessGroupID == executorProcessGroupID {
				t.Fatalf("pipe-holding descendant process group = executor process group %d", executorProcessGroupID)
			}
			releaseFD = openReleaseWriter(t, releaseFIFO)
			complete()
			select {
			case result := <-results:
				t.Fatalf("Run returned before explicit descendant release: %#v", result)
			default:
			}
			if _, err := syscall.Write(releaseFD, []byte{1}); err != nil {
				t.Fatalf("Write(release FIFO) error = %v", err)
			}
			if err := syscall.Close(releaseFD); err != nil {
				t.Fatalf("Close(release FIFO) error = %v", err)
			}
			result := <-results
			cleanupActive = false
			if result.ExitCode != testCase.exitCode || result.FailureClass != testCase.failure || result.Passed {
				t.Fatalf("result = %#v", result)
			}
		})
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

func dockerCommand(t *testing.T, workspace, action string, args ...string) verification.Command {
	t.Helper()
	docker := filepath.Join(t.TempDir(), "docker")
	if err := os.Symlink(os.Args[0], docker); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	return verification.Command{
		Name:       action,
		Directory:  workspace,
		Executable: docker,
		Args:       helperArgs(action, args...),
		Timeout:    time.Second,
	}
}

func helperCommand(workspace, action string, args ...string) verification.Command {
	return verification.Command{
		Name:       action,
		Directory:  workspace,
		Executable: os.Args[0],
		Args:       helperArgs(action, args...),
		Timeout:    5 * time.Second,
		Required:   true,
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

func writeDescendantPIDAndExit(t *testing.T, readyFile, releaseFIFO, action string, exitCode int) {
	t.Helper()
	child := exec.Command(os.Args[0], helperArgs(
		action,
		strconv.Itoa(os.Getpid()),
		readyFile,
		releaseFIFO,
	)...)
	child.Env = helperEnvironmentList()
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	os.Exit(exitCode)
}

func waitForParentReaped(pid int) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return
		}
		if err != nil || time.Now().After(deadline) {
			os.Exit(4)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForRelease(releaseFIFO string) {
	release, err := os.Open(releaseFIFO)
	if err != nil {
		os.Exit(6)
	}
	defer release.Close()
	var byteValue [1]byte
	if _, err := release.Read(byteValue[:]); err != nil {
		os.Exit(7)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readBoundaryPIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	fields := strings.Fields(string(contents))
	if len(fields) != 2 {
		t.Fatalf("boundary PIDs = %q, want descendant and executor process group", contents)
	}
	descendantPID, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("Atoi(descendant %q) error = %v", fields[0], err)
	}
	executorProcessGroupID, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("Atoi(executor process group %q) error = %v", fields[1], err)
	}
	return descendantPID, executorProcessGroupID
}

func openReleaseWriter(t *testing.T, releaseFIFO string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fd, err := syscall.Open(releaseFIFO, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return fd
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("Open(release FIFO) error = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for descendant to block on release FIFO")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type completionContext struct {
	done chan struct{}
	err  error
}

func newCompletionContext(err error) *completionContext {
	return &completionContext{done: make(chan struct{}), err: err}
}

func (c *completionContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *completionContext) Done() <-chan struct{}       { return c.done }
func (c *completionContext) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}
func (c *completionContext) Value(any) any { return nil }
func (c *completionContext) complete()     { close(c.done) }
