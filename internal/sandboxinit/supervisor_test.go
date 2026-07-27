package sandboxinit

import (
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
)

func TestSuperviseConfirmsExecBeforeReceiptAndPreservesExitStatus(t *testing.T) {
	receipt := supervisorReceipt(t)
	var confirmed atomic.Bool
	var published executor.ChildStartReceipt
	signals := make(chan os.Signal, 1)
	type outcome struct {
		status int
		err    error
	}
	finished := make(chan outcome, 1)
	publication := make(chan struct{})
	go func() {
		status, err := Supervise(SuperviseConfig{
			Executable: os.Args[0],
			Arguments:  []string{os.Args[0], "-test.run=TestSupervisorHelperProcess"},
			Environment: append(os.Environ(),
				"GO_WANT_SANDBOX_SUPERVISOR_HELPER=1",
				"SANDBOX_SUPERVISOR_SCENARIO=exit",
			),
			Receipt: receipt,
			ConfirmExec: func(pid int) error {
				if pid <= 0 {
					t.Errorf("invalid child PID %d", pid)
				}
				confirmed.Store(true)
				return nil
			},
			PublishReceipt: func(got executor.ChildStartReceipt) error {
				if !confirmed.Load() {
					t.Error("receipt published before OS exec confirmation")
				}
				published = got.Clone()
				close(publication)
				return nil
			},
			Signals:                signals,
			RequireAcknowledgement: true,
			GracePeriod:            100 * time.Millisecond,
		})
		finished <- outcome{status: status, err: err}
	}()
	<-publication
	select {
	case got := <-finished:
		t.Fatalf("Supervise returned before receipt acknowledgement: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
	signals <- syscall.SIGUSR1
	got := <-finished
	status, err := got.status, got.err
	if err != nil || status != 23 {
		t.Fatalf("Supervise() = %d, %v", status, err)
	}
	if !published.Matches(receipt) {
		t.Fatalf("published receipt = %#v", published)
	}
}

func TestSuperviseForwardsSignalToDeclaredChild(t *testing.T) {
	receipt := supervisorReceipt(t)
	signals := make(chan os.Signal, 1)
	published := make(chan struct{})
	type outcome struct {
		status int
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		status, err := Supervise(SuperviseConfig{
			Executable: os.Args[0],
			Arguments:  []string{os.Args[0], "-test.run=TestSupervisorHelperProcess"},
			Environment: append(os.Environ(),
				"GO_WANT_SANDBOX_SUPERVISOR_HELPER=1",
				"SANDBOX_SUPERVISOR_SCENARIO=signal",
			),
			Receipt:                receipt,
			ConfirmExec:            func(int) error { return nil },
			Signals:                signals,
			RequireAcknowledgement: true,
			GracePeriod:            100 * time.Millisecond,
			PublishReceipt: func(executor.ChildStartReceipt) error {
				close(published)
				return nil
			},
		})
		finished <- outcome{status: status, err: err}
	}()
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("child-start receipt was not published")
	}
	signals <- syscall.SIGUSR1
	// Exec confirmation intentionally does not claim application readiness;
	// allow the helper to install its test-only signal handler first.
	time.Sleep(100 * time.Millisecond)
	signals <- syscall.SIGTERM
	select {
	case got := <-finished:
		if got.err != nil || got.status != 42 {
			t.Fatalf("signal-forwarded outcome = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal-forwarded child did not exit")
	}
}

func TestSupervisorHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SANDBOX_SUPERVISOR_HELPER") != "1" {
		return
	}
	switch os.Getenv("SANDBOX_SUPERVISOR_SCENARIO") {
	case "exit":
		os.Exit(23)
	case "signal":
		received := make(chan os.Signal, 1)
		signal.Notify(received, syscall.SIGTERM)
		defer signal.Stop(received)
		<-received
		os.Exit(42)
	default:
		os.Exit(99)
	}
}

func supervisorReceipt(t *testing.T) executor.ChildStartReceipt {
	t.Helper()
	attempt := executor.AttemptID{
		RunID: "run-supervisor", Stage: "execute", Attempt: 1,
		StartedAt: time.Unix(100, 1).UTC(), Purpose: executor.PurposeAgent,
	}
	command := executor.Command{
		Executable: "codex", Args: []string{"exec"}, Directory: executor.SandboxWorkspaceRoot,
	}
	receipt, err := executor.NewChildStartReceipt(
		attempt,
		command,
		map[string]string{"PATH": executor.CanonicalSandboxPATH},
		nil,
		strings.Repeat("c", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt.Command.Args, []string{"exec"}) {
		t.Fatalf("receipt command = %#v", receipt.Command)
	}
	return receipt
}
