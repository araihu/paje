package repository_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/verification"
)

func TestGenericProfileCompilesOnlyConfiguredCommands(t *testing.T) {
	workspace := newModuleRepository(t)
	profile, err := repository.NewGenericProfile(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	result, err := profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace:   workspace,
		Environment: goEnvironment(workspace),
		Checks: []verification.CommandSpec{{
			Name: "configured", Directory: "site", Executable: "go", Args: []string{"test", "./..."}, Timeout: "1m", Required: true,
		}},
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Commands) != 1 || result.Commands[0].Directory != filepath.Join(resolvedWorkspace, "site") {
		t.Fatalf("Commands = %#v", result.Commands)
	}
	for _, key := range []string{"base_sha", "git_status", "git_available", "tool:go"} {
		if _, ok := result.Facts[key]; !ok {
			t.Errorf("Facts is missing %q: %#v", key, result.Facts)
		}
	}
}

func TestGoProfileDiscoversModulesAndBuildsExactCommands(t *testing.T) {
	workspace := newModuleRepository(t)
	profile, err := repository.NewGoProfile(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	result, err := profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace: workspace, Environment: goEnvironment(workspace),
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	wantModules := []string{".", "site", "tools"}
	if !reflect.DeepEqual(result.Modules, wantModules) {
		t.Fatalf("Modules = %#v, want %#v", result.Modules, wantModules)
	}
	if len(result.Commands) != len(wantModules) {
		t.Fatalf("Commands = %#v", result.Commands)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for index, command := range result.Commands {
		if command.Name != "go test "+wantModules[index] || command.Executable != "go" || !reflect.DeepEqual(command.Args, []string{"test", "./..."}) || command.Directory != filepath.Join(resolvedWorkspace, wantModules[index]) || command.Environment["GOWORK"] != "off" || !command.Required {
			t.Errorf("Commands[%d] = %#v", index, command)
		}
	}
	if got, want := result.Facts["go_work"], filepath.Join(workspace, "go.work"); got != want {
		t.Errorf("Facts[go_work] = %q, want ambient %q", got, want)
	}
	if result.Facts["go_module:."] == "" || result.Facts["go_module:site"] == "" || result.Facts["go_module:tools"] == "" {
		t.Fatalf("Facts does not record GOWORK=off module resolution: %#v", result.Facts)
	}
}

func TestGoProfileExcludesOnlyDeclaredModuleAndPreservesReason(t *testing.T) {
	workspace := newModuleRepository(t)
	profile, err := repository.NewGoProfile(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	result, err := profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace: workspace, Environment: goEnvironment(workspace),
		ModuleExclusions: []repository.ModuleExclusion{{Path: "tools", Reason: "generator-only module"}},
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !reflect.DeepEqual(result.Modules, []string{".", "site"}) || len(result.Commands) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(result.Warnings, []string{"excluded module tools: generator-only module"}) {
		t.Fatalf("Warnings = %#v", result.Warnings)
	}
	if _, err := profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace: workspace, Environment: goEnvironment(workspace),
		ModuleExclusions: []repository.ModuleExclusion{{Path: "missing", Reason: "not present"}},
	}); err == nil {
		t.Fatal("Inspect() accepted exclusion for undiscovered module")
	}
}

func TestGoProfileAppliesConfiguredChecksInsideEveryModule(t *testing.T) {
	workspace := newModuleRepository(t)
	profile, err := repository.NewGoProfile(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	result, err := profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace: workspace, Environment: goEnvironment(workspace),
		Checks: []verification.CommandSpec{{Name: "custom", Directory: ".", Executable: "go", Args: []string{"vet", "./..."}, Timeout: "1m", Required: true}},
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if len(result.Commands) != 3 {
		t.Fatalf("Commands = %#v", result.Commands)
	}
	for _, command := range result.Commands {
		if command.Name != "custom" || command.Executable != "go" || command.Environment["GOWORK"] != "off" || !reflect.DeepEqual(command.Args, []string{"vet", "./..."}) {
			t.Errorf("command = %#v", command)
		}
	}
	if _, err := profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace: workspace, Environment: goEnvironment(workspace),
		Checks: []verification.CommandSpec{{Name: "escape", Directory: "../outside", Executable: "go", Timeout: "1m"}},
	}); err == nil {
		t.Fatal("Inspect() accepted a check directory escaping its module")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "site", "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace: workspace, Environment: goEnvironment(workspace),
		Checks: []verification.CommandSpec{{Name: "symlink escape", Directory: "escape", Executable: "go", Timeout: "1m"}},
	}); err == nil {
		t.Fatal("Inspect() accepted a check directory escaping its module through a symlink")
	}
}

func TestGoProfileClassifiesMissingGoAsEnvironmentFailure(t *testing.T) {
	workspace := newModuleRepository(t)
	profile, err := repository.NewGoProfile(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	_, err = profile.Inspect(context.Background(), repository.ProfileRequest{
		Workspace:   workspace,
		Environment: map[string]string{"PATH": t.TempDir(), "GOWORK": filepath.Join(workspace, "go.work")},
	})
	var environmentErr *repository.EnvironmentError
	if !errors.As(err, &environmentErr) {
		t.Fatalf("Inspect() error = %v, want environment failure", err)
	}
}

func newModuleRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	workspace := t.TempDir()
	run(t, workspace, "git", "init", "-b", "main")
	run(t, workspace, "git", "config", "user.name", "Paje Test")
	run(t, workspace, "git", "config", "user.email", "paje@example.test")
	for path, content := range map[string]string{
		"go.mod":        "module example.test/root\n\ngo 1.26\n",
		"root.go":       "package root\n",
		"site/go.mod":   "module example.test/site\n\ngo 1.26\n",
		"site/site.go":  "package site\n",
		"tools/go.mod":  "module example.test/tools\n\ngo 1.26\n",
		"tools/tool.go": "package tools\n",
		"go.work":       "go 1.26\n\nuse (\n\t.\n\t./site\n\t./tools\n)\n",
	} {
		full := filepath.Join(workspace, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, workspace, "git", "add", ".")
	run(t, workspace, "git", "commit", "-m", "fixture")
	return workspace
}

func goEnvironment(workspace string) map[string]string {
	return map[string]string{"PATH": os.Getenv("PATH"), "GOWORK": filepath.Join(workspace, "go.work")}
}

func run(t *testing.T, directory, executable string, args ...string) {
	t.Helper()
	command := exec.Command(executable, args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", executable, strings.Join(args, " "), err, output)
	}
}
