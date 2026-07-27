// Package verification defines shell-free verification command contracts.
package verification

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Limits bounds verification work requested by a template.
type Limits struct {
	MaxCommands    int
	MaxArguments   int
	MaxTimeout     time.Duration
	MaxOutputBytes int64
}

// DefaultLimits are the beta bounds applied while decoding code-change input.
var DefaultLimits = Limits{
	MaxCommands:    64,
	MaxArguments:   64,
	MaxTimeout:     30 * time.Minute,
	MaxOutputBytes: 1 << 20,
}

// CommandSpec is the JSON-safe command configuration accepted in template input.
type CommandSpec struct {
	Name       string   `json:"name"`
	Directory  string   `json:"directory"`
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
	Timeout    string   `json:"timeout"`
	Required   bool     `json:"required"`
}

// Command is a fully validated command with a repository-relative directory.
type Command struct {
	Name       string
	Directory  string
	Executable string
	Args       []string
	// EnvironmentKeys is keys-only durable result evidence for the exact
	// command-specific environment declaration confirmed at child start. Nil
	// means absent or unconfirmed; an empty slice is an explicit declaration
	// with no command-specific keys. Values are never durable.
	EnvironmentKeys []string `json:"environment_keys"`
	// Environment contains command-specific exact-environment overrides.
	// It is intentionally separate from Args so callers never need shell syntax
	// for values such as GOWORK=off.
	Environment map[string]string `json:"-"`
	Timeout     time.Duration
	Required    bool
}

// Result captures one bounded verification execution.
type Result struct {
	Command      Command       `json:"command"`
	ExitCode     int           `json:"exit_code"`
	Duration     time.Duration `json:"duration"`
	Output       string        `json:"output"`
	Truncated    bool          `json:"truncated"`
	Passed       bool          `json:"passed"`
	Warning      bool          `json:"warning"`
	FailureClass string        `json:"failure_class,omitempty"`
	CauseCode    string        `json:"cause_code,omitempty"`
}

// Compile validates spec against workspace while preserving a normalized
// repository-relative directory in durable command evidence.
func Compile(spec CommandSpec, workspace string, limits Limits) (Command, error) {
	if err := validateLimits(limits); err != nil {
		return Command{}, err
	}
	if containsNUL(workspace) {
		return Command{}, fmt.Errorf("compile verification command: workspace contains NUL byte")
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return Command{}, fmt.Errorf("compile verification command: workspace is required")
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return Command{}, fmt.Errorf("compile verification command: resolve workspace: %w", err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return Command{}, fmt.Errorf("compile verification command: resolve workspace: %w", err)
	}

	if containsNUL(spec.Name) || strings.TrimSpace(spec.Name) == "" {
		return Command{}, fmt.Errorf("compile verification command: name is required")
	}
	spec.Directory = strings.TrimSpace(spec.Directory)
	if containsNUL(spec.Directory) || filepath.IsAbs(spec.Directory) {
		return Command{}, fmt.Errorf("compile verification command %q: directory must be a relative path", spec.Name)
	}
	if containsParentDirectory(spec.Directory) {
		return Command{}, fmt.Errorf("compile verification command %q: directory must not contain ..", spec.Name)
	}
	relativeDirectory := spec.Directory
	if relativeDirectory == "" {
		relativeDirectory = "."
	}
	relativeDirectory = filepath.Clean(relativeDirectory)
	directory := filepath.Join(workspace, relativeDirectory)
	if err := ensureContainedDirectory(workspace, directory); err != nil {
		return Command{}, fmt.Errorf("compile verification command %q: directory escapes workspace", spec.Name)
	}

	executable := strings.TrimSpace(spec.Executable)
	if executable == "" || containsNUL(executable) || strings.ContainsAny(executable, " \t\r\n") || isShell(executable) {
		return Command{}, fmt.Errorf("compile verification command %q: executable must be one shell-free program", spec.Name)
	}
	if len(spec.Args) > limits.MaxArguments {
		return Command{}, fmt.Errorf("compile verification command %q: too many arguments", spec.Name)
	}
	for _, arg := range spec.Args {
		if containsNUL(arg) {
			return Command{}, fmt.Errorf("compile verification command %q: argument contains NUL byte", spec.Name)
		}
	}
	timeout, err := time.ParseDuration(spec.Timeout)
	if err != nil {
		return Command{}, fmt.Errorf("compile verification command %q: parse timeout: %w", spec.Name, err)
	}
	if timeout < time.Second || timeout > limits.MaxTimeout {
		return Command{}, fmt.Errorf("compile verification command %q: timeout must be between 1s and %s", spec.Name, limits.MaxTimeout)
	}

	return Command{
		Name:       strings.TrimSpace(spec.Name),
		Directory:  filepath.ToSlash(relativeDirectory),
		Executable: executable,
		Args:       append([]string(nil), spec.Args...),
		Timeout:    timeout,
		Required:   spec.Required,
	}, nil
}

func ensureContainedDirectory(workspace, directory string) error {
	realDirectory, err := resolveFromExistingAncestor(directory)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(workspace, realDirectory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("directory escapes workspace")
	}
	return nil
}

func resolveFromExistingAncestor(directory string) (string, error) {
	candidate := filepath.Clean(directory)
	var unresolved []string
	for {
		_, err := os.Lstat(candidate)
		if err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(unresolved) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, unresolved[index])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", err
		}
		unresolved = append(unresolved, filepath.Base(candidate))
		candidate = parent
	}
}

func validateLimits(limits Limits) error {
	if limits.MaxArguments < 0 || limits.MaxTimeout < time.Second {
		return fmt.Errorf("compile verification command: invalid limits")
	}
	return nil
}

func containsNUL(value string) bool { return strings.IndexByte(value, 0) >= 0 }

func containsParentDirectory(path string) bool {
	path = filepath.FromSlash(path)
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return true
		}
	}
	return false
}

func isShell(executable string) bool {
	switch strings.ToLower(filepath.Base(executable)) {
	case "sh", "bash", "zsh", "fish", "dash", "cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return true
	default:
		return false
	}
}
