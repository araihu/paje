package executor

import (
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/araihu/paje/internal/workerprofile"
)

const (
	SandboxWorkspaceRoot = "/workspace"

	maxArgumentCount   = 256
	maxArgumentBytes   = 1 << 16
	maxEnvironmentKeys = 512
	maxEnvironmentByte = 1 << 20
	maxExecutionTime   = 24 * time.Hour
	maxOutputBytes     = 64 << 20
)

var (
	runIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	stagePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	executablePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

func (id AttemptID) Validate() error {
	if !runIDPattern.MatchString(id.RunID) || !stagePattern.MatchString(id.Stage) || id.Attempt <= 0 ||
		id.StartedAt.IsZero() || id.Sequence < 0 {
		return errors.New("executor attempt identity is incomplete")
	}
	switch id.Purpose {
	case PurposeProbe, PurposeAgent, PurposeVerification:
		return nil
	default:
		return errors.New("executor attempt purpose is unsupported")
	}
}

func (request Request) Validate() error {
	if err := request.Attempt.Validate(); err != nil {
		return err
	}
	canonical, err := workerprofile.Canonicalize(request.Profile)
	if err != nil || request.Profile.Digest == "" || !reflect.DeepEqual(canonical, request.Profile) {
		return errors.New("executor request requires an exact canonical worker profile")
	}
	if err := validateWorkspace(request.Workspace); err != nil {
		return err
	}
	if err := validateCommand(request.Command, request.Workspace.SandboxPath); err != nil {
		return err
	}
	if err := validateEnvironment(request.Environment); err != nil {
		return err
	}
	if environmentBytes(request.Environment)+environmentBytes(request.Command.Environment) > maxEnvironmentByte {
		return errors.New("executor environment is too large")
	}
	for key := range request.Command.Environment {
		if _, collision := request.Environment[key]; collision {
			return errors.New("executor command environment collides with baseline environment")
		}
	}
	if request.Timeout <= 0 || request.Timeout > maxExecutionTime || request.OutputLimit <= 0 || request.OutputLimit > maxOutputBytes {
		return errors.New("executor request limits are invalid")
	}
	return validateSecrets(request)
}

func validateWorkspace(workspace Workspace) error {
	if workspace.HostPath == "" || strings.IndexByte(workspace.HostPath, 0) >= 0 || !filepath.IsAbs(workspace.HostPath) ||
		filepath.Clean(workspace.HostPath) != workspace.HostPath || workspace.HostPath == string(filepath.Separator) {
		return errors.New("executor workspace host path is invalid")
	}
	if workspace.SandboxPath == "" || strings.IndexByte(workspace.SandboxPath, 0) >= 0 ||
		!filepath.IsAbs(workspace.SandboxPath) || filepath.Clean(workspace.SandboxPath) != workspace.SandboxPath ||
		workspace.SandboxPath != SandboxWorkspaceRoot || pathsOverlap(workspace.SandboxPath, "/run/paje/secrets") {
		return errors.New("executor workspace sandbox path is invalid")
	}
	return nil
}

func validateCommand(command Command, workspaceRoot string) error {
	if !executablePattern.MatchString(command.Executable) || isShell(command.Executable) {
		return errors.New("executor command executable is invalid")
	}
	if len(command.Args) > maxArgumentCount {
		return errors.New("executor command has too many arguments")
	}
	for _, argument := range command.Args {
		if len(argument) > maxArgumentBytes || strings.IndexByte(argument, 0) >= 0 {
			return errors.New("executor command argument is invalid")
		}
	}
	if command.Directory == "" || strings.IndexByte(command.Directory, 0) >= 0 || !filepath.IsAbs(command.Directory) ||
		filepath.Clean(command.Directory) != command.Directory || !pathWithin(workspaceRoot, command.Directory) {
		return errors.New("executor command directory escapes workspace")
	}
	if err := validateEnvironment(command.Environment); err != nil {
		return err
	}
	return nil
}

func validateEnvironment(environment map[string]string) error {
	if len(environment) > maxEnvironmentKeys {
		return errors.New("executor environment has too many keys")
	}
	total := 0
	for key, value := range environment {
		if !environmentPattern.MatchString(key) || strings.IndexByte(value, 0) >= 0 {
			return errors.New("executor environment is invalid")
		}
		total += len(key) + len(value)
		if total > maxEnvironmentByte {
			return errors.New("executor environment is too large")
		}
	}
	return nil
}

func environmentBytes(environment map[string]string) int {
	total := 0
	for key, value := range environment {
		total += len(key) + len(value)
	}
	return total
}

func validateSecrets(request Request) error {
	if request.Attempt.Purpose != PurposeAgent {
		if len(request.Secrets) != 0 {
			return errors.New("executor secrets are agent-only")
		}
		return nil
	}
	if len(request.Secrets) != len(request.Profile.Secrets) {
		return errors.New("executor agent secret materializations do not match profile")
	}
	matched := make([]bool, len(request.Profile.Secrets))
	for _, materialization := range request.Secrets {
		if materialization.Delivery() == workerprofile.DeliveryEnvironment {
			if _, collision := request.Environment[materialization.Target()]; collision {
				return errors.New("executor secret environment target collides with baseline environment")
			}
			if _, collision := request.Command.Environment[materialization.Target()]; collision {
				return errors.New("executor secret environment target collides with command environment")
			}
		}
		found := false
		for index, requirement := range request.Profile.Secrets {
			if matched[index] || requirement.Stage != workerprofile.StageAgent ||
				requirement.Delivery != materialization.Delivery() || requirement.Target != materialization.Target() {
				continue
			}
			matched[index] = true
			found = true
			break
		}
		if !found {
			return errors.New("executor secret materialization is not declared by profile")
		}
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func pathsOverlap(first, second string) bool {
	return pathWithin(first, second) || pathWithin(second, first)
}

func isShell(executable string) bool {
	switch strings.ToLower(filepath.Base(executable)) {
	case "sh", "bash", "zsh", "fish", "dash", "cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return true
	default:
		return false
	}
}

func safeIdentifier(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func validErrorClass(class string) bool {
	switch class {
	case "input", "policy", "environment", "agent", "verification", "canceled", "cleanup", "internal":
		return true
	default:
		return false
	}
}
