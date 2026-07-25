// Package verification defines shell-free verification command contracts.
package verification

import (
	"fmt"
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

// Command is a fully validated command ready for execution.
type Command struct {
	Name       string
	Directory  string
	Executable string
	Args       []string
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

// Compile validates spec and resolves its relative directory under workspace.
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
	directory := spec.Directory
	if directory == "" {
		directory = "."
	}
	directory, err = filepath.Abs(filepath.Join(workspace, directory))
	if err != nil {
		return Command{}, fmt.Errorf("compile verification command %q: resolve directory: %w", spec.Name, err)
	}
	relative, err := filepath.Rel(workspace, directory)
	if err != nil {
		return Command{}, fmt.Errorf("compile verification command %q: check directory: %w", spec.Name, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
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
		Directory:  directory,
		Executable: executable,
		Args:       append([]string(nil), spec.Args...),
		Timeout:    timeout,
		Required:   spec.Required,
	}, nil
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
