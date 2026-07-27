// paje-sandbox-init consumes one private executor-created command document and
// supervises the exact declared child process without a shell.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
	"golang.org/x/sys/unix"
)

type operations struct {
	readFile  func(string, int64) ([]byte, error)
	writeAll  func([]byte) error
	remove    func(string) error
	chdir     func(string) error
	resolve   func(string, string, map[string]string) (string, error)
	supervise func(sandboxinit.SuperviseConfig) (int, error)
}

type childExitError struct {
	Code int
}

func (err *childExitError) Error() string { return "sandbox child exited" }

func main() {
	if err := run(os.Args[1:], os.Stdin, realOperations()); err != nil {
		var exitErr *childExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.Code)
		}
		_, _ = fmt.Fprintln(os.Stderr, "paje-sandbox-init: initialization failed")
		os.Exit(1)
	}
}

func run(arguments []string, stdin io.Reader, ops operations) error {
	switch {
	case len(arguments) == 0:
		return runDocument(sandboxinit.DocumentPath, ops)
	case len(arguments) == 1 && arguments[0] == "--bootstrap-stdin":
		return runBootstrap(stdin, "/", ops)
	case len(arguments) == 1 && arguments[0] == "--emit-child-start-receipt":
		return emitChildStartReceipt(ops)
	case len(arguments) == 1 && filepath.IsAbs(arguments[0]):
		return runDocument(arguments[0], ops)
	default:
		return errors.New("sandbox init invocation is invalid")
	}
}

func emitChildStartReceipt(ops operations) error {
	if ops.readFile == nil || ops.writeAll == nil {
		return errors.New("sandbox receipt emission operations are incomplete")
	}
	encoded, err := ops.readFile(sandboxinit.ChildStartReceiptPath, sandboxinit.MaxDocumentBytes)
	if err != nil {
		return errors.New("read child-start receipt")
	}
	defer zero(encoded)
	if err := ops.writeAll(encoded); err != nil {
		return errors.New("emit child-start receipt")
	}
	return nil
}

func runBootstrap(stdin io.Reader, root string, ops operations) error {
	if err := sandboxinit.ExtractBootstrap(stdin, root); err != nil {
		return err
	}
	return runDocument(sandboxinit.DocumentPath, ops)
}

func runDocument(documentPath string, ops operations) error {
	if ops.readFile == nil || ops.remove == nil {
		return errors.New("sandbox init operations are incomplete")
	}
	encoded, readErr := ops.readFile(documentPath, sandboxinit.MaxDocumentBytes)
	removeErr := ops.remove(documentPath)
	if readErr != nil {
		return fmt.Errorf("read sandbox command document: %w", readErr)
	}
	if removeErr != nil {
		zero(encoded)
		return fmt.Errorf("remove sandbox command document: %w", removeErr)
	}
	document, err := sandboxinit.Decode(bytes.NewReader(encoded))
	zero(encoded)
	if err != nil {
		return err
	}

	keys := make([]string, 0, len(document.EnvironmentFiles))
	for key := range document.EnvironmentFiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fileBytes := make(map[string][]byte, len(keys))
	fileValues := make(map[string]string, len(keys))
	defer func() {
		for _, value := range fileBytes {
			zero(value)
		}
	}()
	for _, key := range keys {
		value, readErr := ops.readFile(document.EnvironmentFiles[key], sandboxinit.MaxEnvironmentValueBytes)
		fileBytes[key] = value
		if readErr != nil {
			removeEnvironmentFiles(ops, document, keys)
			return fmt.Errorf("read sandbox environment materialization: %w", readErr)
		}
		fileValues[key] = string(value)
	}
	if err := removeEnvironmentFiles(ops, document, keys); err != nil {
		return err
	}
	environment, err := document.ChildEnvironment(fileValues)
	if err != nil {
		return err
	}
	if ops.resolve == nil || ops.chdir == nil || ops.supervise == nil {
		return errors.New("sandbox init execution operations are incomplete")
	}
	executable, err := ops.resolve(document.Command.Executable, document.Command.Directory, environment)
	if err != nil {
		return fmt.Errorf("resolve sandbox executable: %w", err)
	}
	if err := ops.chdir(document.Command.Directory); err != nil {
		return fmt.Errorf("change sandbox command directory: %w", err)
	}
	environmentList, err := sandboxinit.EnvironmentList(environment)
	if err != nil {
		return err
	}
	argv := make([]string, 0, len(document.Command.Args)+1)
	argv = append(argv, document.Command.Executable)
	argv = append(argv, document.Command.Args...)
	status, err := ops.supervise(sandboxinit.SuperviseConfig{
		Executable:  executable,
		Arguments:   argv,
		Environment: environmentList,
		Receipt:     document.ChildStartReceipt.Clone(),
	})
	if err != nil {
		return err
	}
	if status != 0 {
		return &childExitError{Code: status}
	}
	return nil
}

func removeEnvironmentFiles(ops operations, document sandboxinit.Document, keys []string) error {
	var failures []error
	for _, key := range keys {
		if err := ops.remove(document.EnvironmentFiles[key]); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("remove sandbox environment materializations: %w", errors.Join(failures...))
	}
	return nil
}

func realOperations() operations {
	return operations{
		readFile: readBoundedRegularFile,
		writeAll: func(value []byte) error {
			written, err := os.Stdout.Write(value)
			if err != nil || written != len(value) {
				return io.ErrShortWrite
			}
			return nil
		},
		remove:    os.Remove,
		chdir:     os.Chdir,
		resolve:   resolveExecutable,
		supervise: supervise,
	}
}

func supervise(config sandboxinit.SuperviseConfig) (int, error) {
	signals := make(chan os.Signal, 16)
	signal.Notify(signals,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
	)
	defer signal.Stop(signals)
	config.ConfirmExec = confirmChildExec
	config.PublishReceipt = publishChildStartReceipt
	config.Signals = signals
	config.RequireAcknowledgement = true
	return sandboxinit.Supervise(config)
}

func confirmChildExec(pid int) error {
	if pid <= 0 {
		return errors.New("sandbox child PID is invalid")
	}
	// os/exec.Cmd.Start has already synchronously confirmed a successful exec.
	// Do not require the process to remain alive: exact short-lived commands
	// still crossed the declared exec boundary and must retain their exit code.
	return nil
}

func publishChildStartReceipt(receipt executor.ChildStartReceipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil || len(encoded) > sandboxinit.MaxDocumentBytes {
		return errors.New("encode child-start receipt")
	}
	descriptor, err := unix.Open(
		sandboxinit.ChildStartReceiptPath,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return errors.New("create child-start receipt")
	}
	file := os.NewFile(uintptr(descriptor), "child-start-receipt")
	if file == nil {
		_ = unix.Close(descriptor)
		return errors.New("open child-start receipt")
	}
	written, writeErr := file.Write(encoded)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || written != len(encoded) || syncErr != nil || closeErr != nil {
		_ = os.Remove(sandboxinit.ChildStartReceiptPath)
		return errors.New("publish child-start receipt")
	}
	return nil
}

func readBoundedRegularFile(filePath string, limit int64) ([]byte, error) {
	descriptor, err := unix.Open(filePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), "sandbox-material")
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New("open sandbox material file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("sandbox material file is invalid")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(value)) > limit {
		zero(value)
		return nil, errors.New("sandbox material file exceeds limit")
	}
	return value, nil
}

func resolveExecutable(executable, directory string, environment map[string]string) (string, error) {
	pathValue, ok := environment["PATH"]
	if !ok {
		return "", errors.New("sandbox executable PATH is missing")
	}
	for _, pathDirectory := range filepath.SplitList(pathValue) {
		candidate := filepath.Join(pathDirectory, executable)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	_ = directory
	return "", errors.New("sandbox executable is unavailable")
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
