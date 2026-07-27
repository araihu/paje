// Package sandboxinit defines the private command document consumed inside a
// workload sandbox immediately before execve.
package sandboxinit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/araihu/paje/internal/executor"
)

const (
	DocumentPath             = "/run/paje/command.json"
	ChildStartReceiptPath    = "/run/paje/child-start.json"
	SecretRoot               = "/run/paje/secrets"
	MaxDocumentBytes         = 1 << 20
	MaxEnvironmentBytes      = 1 << 20
	MaxEnvironmentValueBytes = MaxEnvironmentBytes
	maxArguments             = 256
	maxArgumentBytes         = 1 << 16
	maxEnvironmentKeys       = 512
)

var (
	executablePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

type Document struct {
	WorkspaceRoot     string                     `json:"workspace_root"`
	Command           executor.Command           `json:"command"`
	Environment       map[string]string          `json:"environment"`
	EnvironmentFiles  map[string]string          `json:"environment_files,omitempty"`
	ChildStartReceipt executor.ChildStartReceipt `json:"child_start_receipt"`
}

// BindChildStartReceipt freezes the exact command/environment declaration and
// private challenge that sandbox-init must acknowledge after OS exec success.
func (document *Document) BindChildStartReceipt(attempt executor.AttemptID, challenge string) error {
	if document == nil {
		return errors.New("sandbox command document is nil")
	}
	if document.Environment["PATH"] != executor.CanonicalSandboxPATH {
		return errors.New("sandbox command exact canonical PATH is required")
	}
	receipt, err := executor.NewChildStartReceipt(
		attempt,
		document.Command,
		document.Environment,
		document.EnvironmentFiles,
		challenge,
	)
	if err != nil {
		return err
	}
	document.ChildStartReceipt = receipt
	return nil
}

func Decode(reader io.Reader) (Document, error) {
	limited := &io.LimitedReader{R: reader, N: MaxDocumentBytes + 1}
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return Document{}, errors.New("read sandbox command document")
	}
	if len(encoded) > MaxDocumentBytes {
		return Document{}, errors.New("sandbox command document exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, errors.New("decode sandbox command document")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Document{}, errors.New("sandbox command document has trailing content")
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	return document, nil
}

func (document Document) Validate() error {
	if document.WorkspaceRoot != executor.SandboxWorkspaceRoot {
		return errors.New("sandbox command workspace root is not fixed")
	}
	if err := validateCommand(document.Command, document.WorkspaceRoot); err != nil {
		return err
	}
	if err := validateEnvironment(document.Environment); err != nil {
		return err
	}
	if err := validateEnvironment(document.Command.Environment); err != nil {
		return err
	}
	for key := range document.Command.Environment {
		if _, exists := document.Environment[key]; exists {
			return errors.New("sandbox command environment key collision")
		}
	}
	if len(document.EnvironmentFiles)+len(document.Environment)+len(document.Command.Environment) > maxEnvironmentKeys {
		return errors.New("sandbox command environment has too many keys")
	}
	seenPaths := make(map[string]struct{}, len(document.EnvironmentFiles))
	for key, file := range document.EnvironmentFiles {
		if !environmentPattern.MatchString(key) || file == "" || strings.IndexByte(file, 0) >= 0 ||
			!strings.HasPrefix(file, "/") || path.Clean(file) != file ||
			!pathWithin(SecretRoot, file) || file == SecretRoot {
			return errors.New("sandbox environment file declaration is invalid")
		}
		if _, exists := document.Environment[key]; exists {
			return errors.New("sandbox environment file key collision")
		}
		if _, exists := document.Command.Environment[key]; exists {
			return errors.New("sandbox command environment file key collision")
		}
		if _, duplicate := seenPaths[file]; duplicate {
			return errors.New("sandbox environment file path is duplicated")
		}
		seenPaths[file] = struct{}{}
	}
	if environmentBytes(document.Environment)+environmentBytes(document.Command.Environment)+
		environmentFileDeclarationBytes(document.EnvironmentFiles) > MaxEnvironmentBytes {
		return errors.New("sandbox command environment is too large")
	}
	pathValue, ok := document.Environment["PATH"]
	if !ok || pathValue != executor.CanonicalSandboxPATH || !validPath(pathValue) {
		return errors.New("sandbox command exact canonical PATH is required")
	}
	expected, err := executor.NewChildStartReceipt(
		document.ChildStartReceipt.Attempt,
		document.Command,
		document.Environment,
		document.EnvironmentFiles,
		document.ChildStartReceipt.Challenge,
	)
	if err != nil || !document.ChildStartReceipt.Matches(expected) {
		return errors.New("sandbox command child-start receipt binding is invalid")
	}
	return nil
}

// ChildEnvironment merges validated non-secret values with environment-file
// values read after sandbox start. The caller owns the returned map.
func (document Document) ChildEnvironment(fileValues map[string]string) (map[string]string, error) {
	if err := document.Validate(); err != nil {
		return nil, err
	}
	if len(fileValues) != len(document.EnvironmentFiles) {
		return nil, errors.New("sandbox environment materialization set is incomplete")
	}
	for key, value := range fileValues {
		if _, expected := document.EnvironmentFiles[key]; !expected || len(value) > MaxEnvironmentValueBytes || strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("sandbox environment materialization is invalid")
		}
	}
	if environmentBytes(document.Environment)+environmentBytes(document.Command.Environment)+environmentBytes(fileValues) > MaxEnvironmentBytes {
		return nil, errors.New("sandbox child environment is too large")
	}
	result := make(map[string]string, len(document.Environment)+len(document.Command.Environment)+len(fileValues))
	for key, value := range document.Environment {
		result[key] = value
	}
	for key, value := range document.Command.Environment {
		result[key] = value
	}
	for key, value := range fileValues {
		result[key] = value
	}
	return result, nil
}

func EnvironmentList(values map[string]string) ([]string, error) {
	if err := validateEnvironment(values); err != nil {
		return nil, err
	}
	if environmentBytes(values) > MaxEnvironmentBytes {
		return nil, errors.New("sandbox child environment is too large")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

func validateCommand(command executor.Command, workspaceRoot string) error {
	if !executablePattern.MatchString(command.Executable) || isShell(command.Executable) || len(command.Args) > maxArguments {
		return errors.New("sandbox command executable or argument count is invalid")
	}
	for _, argument := range command.Args {
		if len(argument) > maxArgumentBytes || strings.IndexByte(argument, 0) >= 0 {
			return errors.New("sandbox command argument is invalid")
		}
	}
	if command.Directory == "" || strings.IndexByte(command.Directory, 0) >= 0 || !strings.HasPrefix(command.Directory, "/") ||
		path.Clean(command.Directory) != command.Directory ||
		!pathWithin(workspaceRoot, command.Directory) {
		return errors.New("sandbox command directory escapes workspace")
	}
	return nil
}

func environmentBytes(values map[string]string) int {
	total := 0
	for key, value := range values {
		total += len(key) + len(value)
	}
	return total
}

func environmentFileDeclarationBytes(values map[string]string) int {
	return environmentBytes(values)
}

func validateEnvironment(values map[string]string) error {
	if len(values) > maxEnvironmentKeys {
		return errors.New("sandbox command environment has too many keys")
	}
	for key, value := range values {
		if !environmentPattern.MatchString(key) || len(value) > MaxEnvironmentValueBytes || strings.IndexByte(value, 0) >= 0 {
			return errors.New("sandbox command environment is invalid")
		}
	}
	return nil
}

func validPath(value string) bool {
	if value == "" || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	for _, directory := range strings.Split(value, ":") {
		if directory == "" || !strings.HasPrefix(directory, "/") || path.Clean(directory) != directory {
			return false
		}
	}
	return true
}

func pathWithin(root, candidate string) bool {
	if root == candidate {
		return true
	}
	return strings.HasPrefix(candidate, root+"/")
}

func isShell(executable string) bool {
	switch strings.ToLower(path.Base(executable)) {
	case "sh", "bash", "zsh", "fish", "dash", "cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return true
	default:
		return false
	}
}
