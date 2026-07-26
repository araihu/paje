// paje-sandbox-init consumes one private executor-created command document and
// replaces itself with the exact declared child process without a shell.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/araihu/paje/internal/sandboxinit"
	"golang.org/x/sys/unix"
)

type operations struct {
	readFile func(string, int64) ([]byte, error)
	remove   func(string) error
	chdir    func(string) error
	resolve  func(string, string, map[string]string) (string, error)
	exec     func(string, []string, []string) error
}

func main() {
	if err := run(os.Args[1:], os.Stdin, realOperations()); err != nil {
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
	case len(arguments) == 1 && filepath.IsAbs(arguments[0]):
		return runDocument(arguments[0], ops)
	default:
		return errors.New("sandbox init invocation is invalid")
	}
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
	if ops.resolve == nil || ops.chdir == nil || ops.exec == nil {
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
	return ops.exec(executable, argv, environmentList)
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
		remove:   os.Remove,
		chdir:    os.Chdir,
		resolve:  resolveExecutable,
		exec:     syscall.Exec,
	}
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
