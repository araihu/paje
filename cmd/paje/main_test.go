package main

import (
	"os"
	"testing"

	"github.com/araihu/paje/internal/config"
	"github.com/araihu/paje/internal/memory/mem0"
	memorymock "github.com/araihu/paje/internal/memory/mock"
	codexrunner "github.com/araihu/paje/internal/runner/codex"
	"github.com/araihu/paje/internal/runner/local"
	runnermock "github.com/araihu/paje/internal/runner/mock"
	"github.com/araihu/paje/internal/workspace/gitworktree"
	workspacemock "github.com/araihu/paje/internal/workspace/mock"
)

func TestBuildDependenciesUsesMocksByDefault(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		MemoryAdapter:    "mock",
		WorkspaceAdapter: "mock",
		RunnerAdapter:    "mock",
		WorkspaceRoot:    t.TempDir(),
	}

	dependencies, err := buildDependencies(cfg)
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	if _, ok := dependencies.memory.(*memorymock.Store); !ok {
		t.Errorf("memory dependency = %T, want *memory/mock.Store", dependencies.memory)
	}
	if _, ok := dependencies.workspaces.(*workspacemock.Manager); !ok {
		t.Errorf("workspace dependency = %T, want *workspace/mock.Manager", dependencies.workspaces)
	}
	if _, ok := dependencies.runner.(*runnermock.Runner); !ok {
		t.Errorf("runner dependency = %T, want *runner/mock.Runner", dependencies.runner)
	}
}

func TestBuildDependenciesUsesConfiguredRealAdapters(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		MemoryAdapter:    "mem0",
		WorkspaceAdapter: "git",
		RunnerAdapter:    "local",
		Mem0APIKey:       "secret",
		Mem0BaseURL:      "https://mem0.example.test",
		WorkspaceRoot:    t.TempDir(),
		RunnerCommand:    os.Args[0],
		RunnerArgs:       []string{"-test.run=TestNonexistent"},
	}

	dependencies, err := buildDependencies(cfg)
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	if _, ok := dependencies.memory.(*mem0.Store); !ok {
		t.Errorf("memory dependency = %T, want *mem0.Store", dependencies.memory)
	}
	if _, ok := dependencies.workspaces.(*gitworktree.Manager); !ok {
		t.Errorf("workspace dependency = %T, want *gitworktree.Manager", dependencies.workspaces)
	}
	if _, ok := dependencies.runner.(*local.Runner); !ok {
		t.Errorf("runner dependency = %T, want *local.Runner", dependencies.runner)
	}
}

func TestBuildDependenciesUsesCodexRunner(t *testing.T) {
	t.Parallel()

	dependencies, err := buildDependencies(config.Config{
		MemoryAdapter:    "mock",
		WorkspaceAdapter: "mock",
		RunnerAdapter:    "codex",
		WorkspaceRoot:    t.TempDir(),
		RunnerCommand:    os.Args[0],
	})
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	if _, ok := dependencies.runner.(*codexrunner.Runner); !ok {
		t.Errorf("runner dependency = %T, want *codex.Runner", dependencies.runner)
	}
}

func TestBuildDependenciesRejectsUnknownSelection(t *testing.T) {
	t.Parallel()

	if _, err := buildDependencies(config.Config{
		MemoryAdapter:    "unknown",
		WorkspaceAdapter: "mock",
		RunnerAdapter:    "mock",
		WorkspaceRoot:    t.TempDir(),
	}); err == nil {
		t.Fatal("buildDependencies() error = nil, want unknown adapter error")
	}
}

func TestBuildDependenciesRejectsUnknownRunnerSelection(t *testing.T) {
	t.Parallel()

	if _, err := buildDependencies(config.Config{
		MemoryAdapter:    "mock",
		WorkspaceAdapter: "mock",
		RunnerAdapter:    "unknown",
		WorkspaceRoot:    t.TempDir(),
	}); err == nil {
		t.Fatal("buildDependencies() error = nil, want unknown adapter error")
	}
}
