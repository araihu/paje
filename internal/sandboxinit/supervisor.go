package sandboxinit

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/araihu/paje/internal/executor"
)

const defaultSupervisorGrace = 500 * time.Millisecond

// SuperviseConfig contains only the already-validated exact child declaration
// and private receipt operations. Supervise never invokes a shell.
type SuperviseConfig struct {
	Executable             string
	Arguments              []string
	Environment            []string
	Receipt                executor.ChildStartReceipt
	ConfirmExec            func(int) error
	PublishReceipt         func(executor.ChildStartReceipt) error
	Signals                <-chan os.Signal
	RequireAcknowledgement bool
	GracePeriod            time.Duration
}

// Supervise starts the declared child, acknowledges only after the OS exec
// boundary is confirmed, forwards signals to its process group, reaps adopted
// descendants, and returns the declared child's exact exit status.
func Supervise(config SuperviseConfig) (int, error) {
	if !filepath.IsAbs(config.Executable) || strings.IndexByte(config.Executable, 0) >= 0 ||
		len(config.Arguments) == 0 || config.Arguments[0] == "" ||
		config.ConfirmExec == nil || config.PublishReceipt == nil ||
		config.RequireAcknowledgement && config.Signals == nil {
		return 0, errors.New("sandbox supervisor declaration is incomplete")
	}
	for _, argument := range config.Arguments {
		if strings.IndexByte(argument, 0) >= 0 {
			return 0, errors.New("sandbox supervisor argument is invalid")
		}
	}
	for _, value := range config.Environment {
		if strings.IndexByte(value, 0) >= 0 || !strings.Contains(value, "=") {
			return 0, errors.New("sandbox supervisor environment is invalid")
		}
	}
	if err := config.Receipt.Validate(); err != nil {
		return 0, err
	}
	if config.GracePeriod == 0 {
		config.GracePeriod = defaultSupervisorGrace
	}
	if config.GracePeriod < 0 || config.GracePeriod > 10*time.Second {
		return 0, errors.New("sandbox supervisor grace period is invalid")
	}

	command := exec.Command(config.Executable, config.Arguments[1:]...)
	command.Args = slices.Clone(config.Arguments)
	command.Env = slices.Clone(config.Environment)
	command.Stdin = nil
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cmd.Start waits on the child fork/exec error pipe. A nil result therefore
	// proves that the exact argv reached a successful OS exec boundary, even if
	// the declared process exits before the parent can probe its PID.
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	failStarted := func(cause error) (int, error) {
		terminateProcessGroup(pid, config.GracePeriod)
		_, waitErr := waitStatus(command)
		reapExitedDescendants()
		return 0, errors.Join(cause, waitErr)
	}
	if err := config.ConfirmExec(pid); err != nil {
		return failStarted(err)
	}
	if err := config.PublishReceipt(config.Receipt.Clone()); err != nil {
		return failStarted(err)
	}

	done := make(chan struct{})
	forwarded := make(chan struct{})
	acknowledged := make(chan struct{}, 1)
	if config.Signals != nil {
		go func() {
			defer close(forwarded)
			for {
				select {
				case signal, ok := <-config.Signals:
					if !ok {
						return
					}
					if value, ok := signal.(syscall.Signal); ok {
						if config.RequireAcknowledgement && value == syscall.SIGUSR1 {
							select {
							case acknowledged <- struct{}{}:
							default:
							}
							continue
						}
						_ = syscall.Kill(-pid, value)
					}
				case <-done:
					return
				}
			}
		}()
	} else {
		close(forwarded)
	}
	status, waitErr := waitStatus(command)
	if config.RequireAcknowledgement {
		select {
		case <-acknowledged:
		case <-forwarded:
			terminateProcessGroup(pid, config.GracePeriod)
			reapExitedDescendants()
			return 0, errors.Join(waitErr, errors.New("child-start receipt acknowledgement was not received"))
		}
	}
	close(done)
	<-forwarded
	terminateProcessGroup(pid, config.GracePeriod)
	reapExitedDescendants()
	return status, waitErr
}

func waitStatus(command *exec.Cmd) (int, error) {
	err := command.Wait()
	if command.ProcessState == nil {
		return 0, err
	}
	status := command.ProcessState.ExitCode()
	if status >= 0 {
		var exitErr *exec.ExitError
		if err == nil || errors.As(err, &exitErr) {
			return status, nil
		}
		return status, err
	}
	if wait, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
		return 128 + int(wait.Signal()), nil
	}
	return 0, err
}

func terminateProcessGroup(pid int, grace time.Duration) {
	if pid <= 0 || !processGroupExists(pid) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for processGroupExists(pid) && time.Now().Before(deadline) {
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
	if processGroupExists(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func processGroupExists(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func reapExitedDescendants() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 || errors.Is(err, syscall.ECHILD) {
			return
		}
		if err != nil {
			return
		}
	}
}
