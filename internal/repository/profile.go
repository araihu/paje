package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/araihu/paje/internal/verification"
)

const profileCommandTimeout = time.Minute

// ProfileRequest is the bounded input to repository inspection.
type ProfileRequest struct {
	Workspace        string
	Environment      map[string]string
	Checks           []verification.CommandSpec
	ModuleExclusions []ModuleExclusion
}

// ProfileResult contains serializable preflight evidence and ready-to-run checks.
type ProfileResult struct {
	Facts    map[string]string
	Warnings []string
	Modules  []string
	Commands []verification.Command
}

// EnvironmentError identifies a missing or unusable local tool as an
// environment failure, matching verification.Executor classification semantics.
type EnvironmentError struct {
	Operation string
}

func (e *EnvironmentError) Error() string {
	return fmt.Sprintf("repository profile environment failure: %s", e.Operation)
}

// GenericProfile records Git preflight facts and compiles explicit checks.
type GenericProfile struct {
	limits   verification.Limits
	executor verification.Runner
}

// GoProfile adds tracked Go module discovery and GOWORK-off commands.
type GoProfile struct {
	generic *GenericProfile
}

// NewGenericProfile constructs a generic profile using Task 4's bounded executor.
func NewGenericProfile(limits verification.Limits) (*GenericProfile, error) {
	if limits.MaxCommands <= 0 {
		return nil, fmt.Errorf("create generic repository profile: command limit must be positive")
	}
	executor, err := verification.NewExecutor(limits)
	if err != nil {
		return nil, fmt.Errorf("create generic repository profile: %w", err)
	}
	return &GenericProfile{limits: limits, executor: executor}, nil
}

// NewGoProfile constructs a Go profile using Task 4's bounded executor.
func NewGoProfile(limits verification.Limits) (*GoProfile, error) {
	generic, err := NewGenericProfile(limits)
	if err != nil {
		return nil, fmt.Errorf("create Go repository profile: %w", err)
	}
	return &GoProfile{generic: generic}, nil
}

func (p *GenericProfile) Name() string { return "generic" }

// Inspect records generic Git facts and compiles only explicitly supplied checks.
func (p *GenericProfile) Inspect(ctx context.Context, request ProfileRequest) (ProfileResult, error) {
	workspace, err := validateWorkspace(request.Workspace)
	if err != nil {
		return ProfileResult{}, err
	}
	if len(request.Checks) == 0 {
		return ProfileResult{}, fmt.Errorf("inspect generic repository profile: at least one check is required")
	}
	if len(request.Checks) > p.limits.MaxCommands {
		return ProfileResult{}, fmt.Errorf("inspect generic repository profile: too many checks")
	}
	result, err := p.inspectGit(ctx, workspace, request.Environment)
	if err != nil {
		return ProfileResult{}, err
	}
	result.Commands, err = compileChecks(request.Checks, workspace, p.limits)
	if err != nil {
		return ProfileResult{}, err
	}
	for _, command := range result.Commands {
		result.Facts["tool:"+command.Executable] = toolAvailability(command, request.Environment)
	}
	return result, nil
}

func (p *GoProfile) Name() string { return "go" }

// Inspect discovers tracked modules, resolves each outside ambient workspaces,
// and compiles selected checks with a GOWORK=off command override.
func (p *GoProfile) Inspect(ctx context.Context, request ProfileRequest) (ProfileResult, error) {
	workspace, err := validateWorkspace(request.Workspace)
	if err != nil {
		return ProfileResult{}, err
	}
	result, err := p.generic.inspectGit(ctx, workspace, request.Environment)
	if err != nil {
		return ProfileResult{}, err
	}
	goWork, err := p.generic.profileOutput(ctx, workspace, request.Environment, "go env GOWORK", "go", "env", "GOWORK")
	if err != nil {
		return ProfileResult{}, err
	}
	result.Facts["go_work"] = strings.TrimSpace(goWork)
	result.Facts["go_available"] = "available"

	modules, err := p.generic.discoverModules(ctx, workspace, request.Environment)
	if err != nil {
		return ProfileResult{}, err
	}
	selected, warnings, err := applyExclusions(modules, request.ModuleExclusions)
	if err != nil {
		return ProfileResult{}, err
	}
	result.Modules = selected
	result.Warnings = warnings

	goEnvironment := copyEnvironment(request.Environment)
	goEnvironment["GOWORK"] = "off"
	for _, module := range selected {
		moduleDirectory, err := containedModuleDirectory(workspace, module)
		if err != nil {
			return ProfileResult{}, err
		}
		output, err := p.generic.profileOutput(ctx, moduleDirectory, goEnvironment, "go list -m -json", "go", "list", "-m", "-json")
		if err != nil {
			return ProfileResult{}, err
		}
		result.Facts["go_module:"+module] = strings.TrimSpace(output)
	}

	checksPerModule := len(request.Checks)
	if checksPerModule == 0 {
		checksPerModule = 1
	}
	if len(selected) > p.generic.limits.MaxCommands/checksPerModule {
		return ProfileResult{}, fmt.Errorf("inspect Go repository profile: too many generated checks")
	}
	for _, module := range selected {
		moduleDirectory, err := containedModuleDirectory(workspace, module)
		if err != nil {
			return ProfileResult{}, err
		}
		checks := request.Checks
		if len(checks) == 0 {
			checks = []verification.CommandSpec{{
				Name:       "go test " + module,
				Directory:  ".",
				Executable: "go",
				Args:       []string{"test", "./..."},
				Timeout:    "10m",
				Required:   true,
			}}
		}
		commands, err := compileChecks(checks, moduleDirectory, p.generic.limits)
		if err != nil {
			return ProfileResult{}, err
		}
		for index := range commands {
			commands[index].Environment = map[string]string{"GOWORK": "off"}
		}
		result.Commands = append(result.Commands, commands...)
	}
	return result, nil
}

func (p *GenericProfile) inspectGit(ctx context.Context, workspace string, environment map[string]string) (ProfileResult, error) {
	baseSHA, err := p.profileOutput(ctx, workspace, environment, "git rev-parse HEAD", "git", "rev-parse", "HEAD")
	if err != nil {
		return ProfileResult{}, err
	}
	status, err := p.profileOutput(ctx, workspace, environment, "git status", "git", "status", "--porcelain=v1")
	if err != nil {
		return ProfileResult{}, err
	}
	return ProfileResult{Facts: map[string]string{
		"base_sha":      strings.TrimSpace(baseSHA),
		"git_status":    strings.TrimSpace(status),
		"git_available": "available",
	}}, nil
}

func (p *GenericProfile) discoverModules(ctx context.Context, workspace string, environment map[string]string) ([]string, error) {
	output, err := p.profileOutput(ctx, workspace, environment, "git ls-files go.mod", "git", "ls-files", "-z", "--", "go.mod", "**/go.mod")
	if err != nil {
		return nil, err
	}
	modules := make([]string, 0)
	seen := make(map[string]struct{})
	for _, path := range strings.Split(output, "\x00") {
		if path == "" {
			continue
		}
		path = filepath.ToSlash(path)
		if path == "go.mod" {
			path = "."
		} else {
			path = strings.TrimSuffix(path, "/go.mod")
		}
		if path == "" || path == "." || strings.HasPrefix(path, "../") || filepath.IsAbs(path) {
			if path != "." {
				return nil, fmt.Errorf("inspect Go repository profile: invalid tracked go.mod path %q", path)
			}
		}
		if _, ok := seen[path]; ok {
			continue
		}
		if _, err := containedModuleDirectory(workspace, path); err != nil {
			return nil, err
		}
		seen[path] = struct{}{}
		modules = append(modules, path)
	}
	sort.Strings(modules)
	if len(modules) == 0 {
		return nil, fmt.Errorf("inspect Go repository profile: no tracked go.mod files")
	}
	return modules, nil
}

func (p *GenericProfile) profileOutput(ctx context.Context, directory string, environment map[string]string, operation, executable string, args ...string) (string, error) {
	result := p.executor.Run(ctx, verification.Command{
		Name:       operation,
		Directory:  directory,
		Executable: executable,
		Args:       args,
		Timeout:    profileCommandTimeout,
		Required:   true,
	}, environment)
	if result.Passed {
		return result.Output, nil
	}
	if result.FailureClass == "environment" {
		return "", &EnvironmentError{Operation: operation}
	}
	return "", fmt.Errorf("inspect repository profile: %s failed (%s): %s", operation, result.CauseCode, strings.TrimSpace(result.Output))
}

func compileChecks(checks []verification.CommandSpec, workspace string, limits verification.Limits) ([]verification.Command, error) {
	commands := make([]verification.Command, 0, len(checks))
	for index, check := range checks {
		command, err := verification.Compile(check, workspace, limits)
		if err != nil {
			return nil, fmt.Errorf("compile repository profile check %d: %w", index, err)
		}
		if err := ensureDirectoryContained(workspace, command.Directory); err != nil {
			return nil, fmt.Errorf("compile repository profile check %d: %w", index, err)
		}
		commands = append(commands, command)
	}
	return commands, nil
}

func applyExclusions(modules []string, exclusions []ModuleExclusion) ([]string, []string, error) {
	byPath := make(map[string]string, len(exclusions))
	for _, exclusion := range exclusions {
		path := strings.TrimSpace(filepath.ToSlash(exclusion.Path))
		reason := strings.TrimSpace(exclusion.Reason)
		if path == "" || reason == "" || filepath.IsAbs(path) || containsParent(path) {
			return nil, nil, fmt.Errorf("inspect Go repository profile: invalid module exclusion")
		}
		if _, exists := byPath[path]; exists {
			return nil, nil, fmt.Errorf("inspect Go repository profile: duplicate module exclusion %q", path)
		}
		byPath[path] = reason
	}
	selected := make([]string, 0, len(modules))
	warnings := make([]string, 0, len(exclusions))
	for _, module := range modules {
		if reason, excluded := byPath[module]; excluded {
			warnings = append(warnings, "excluded module "+module+": "+reason)
			delete(byPath, module)
			continue
		}
		selected = append(selected, module)
	}
	if len(byPath) != 0 {
		paths := make([]string, 0, len(byPath))
		for path := range byPath {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		return nil, nil, fmt.Errorf("inspect Go repository profile: exclusion %q is not a discovered module", paths[0])
	}
	return selected, warnings, nil
}

func validateWorkspace(workspace string) (string, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || strings.IndexByte(workspace, 0) >= 0 {
		return "", fmt.Errorf("inspect repository profile: workspace is required")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("inspect repository profile: resolve workspace: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect repository profile: stat workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("inspect repository profile: workspace is not a directory")
	}
	return filepath.EvalSymlinks(abs)
}

func containedModuleDirectory(workspace, module string) (string, error) {
	if module != "." && (filepath.IsAbs(module) || containsParent(module)) {
		return "", fmt.Errorf("inspect Go repository profile: module directory escapes workspace")
	}
	directory := filepath.Join(workspace, filepath.FromSlash(module))
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", fmt.Errorf("inspect Go repository profile: resolve module directory %q: %w", module, err)
	}
	relative, err := filepath.Rel(workspace, realDirectory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("inspect Go repository profile: module directory escapes workspace")
	}
	return realDirectory, nil
}

func ensureDirectoryContained(root, directory string) error {
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("resolve check directory: %w", err)
	}
	relative, err := filepath.Rel(root, realDirectory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("directory escapes workspace")
	}
	return nil
}

func containsParent(path string) bool {
	for _, component := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if component == ".." {
			return true
		}
	}
	return false
}

func copyEnvironment(values map[string]string) map[string]string {
	copy := make(map[string]string, len(values)+1)
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func toolAvailability(command verification.Command, environment map[string]string) string {
	if _, err := verification.ResolveExecutable(command.Executable, command.Directory, environment); err == nil {
		return "available"
	}
	return "unavailable"
}
