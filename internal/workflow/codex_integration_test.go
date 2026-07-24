package workflow_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/memory"
	memorymock "github.com/araihu/paje/internal/memory/mock"
	"github.com/araihu/paje/internal/runner/codex"
	"github.com/araihu/paje/internal/workflow"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestCodexOrchestrationIntegration(t *testing.T) {
	if os.Getenv("PAJE_CODEX_INTEGRATION") != "1" {
		t.Skip("set PAJE_CODEX_INTEGRATION=1 to run the authenticated Codex integration")
	}

	repository := newIntegrationRepository(t)
	workspaceRoot := t.TempDir()
	store := memorymock.NewStore([]memory.Memory{{
		ID:      "seed-1",
		Content: "Pajé orchestration memory: read the README before responding.",
	}})
	workspaces, err := gitworktree.New(workspaceRoot)
	if err != nil {
		t.Fatalf("gitworktree.New() error = %v", err)
	}
	executor, err := codex.New("codex")
	if err != nil {
		t.Fatalf("codex.New() error = %v", err)
	}
	orchestrator, err := workflow.New(store, workspaces, executor)
	if err != nil {
		t.Fatalf("workflow.New() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	output, err := orchestrator.Run(ctx, workflow.RunInput{
		TaskDescription: "Read README.md in the workspace and the relevant memory included below. Then reply with exactly PAJE_CODEX_ORCHESTRATED_OK, with no other text. Do not modify any files.",
		RepositoryURI:   repository,
		Branch:          "main",
		MemoryQuery:     "Pajé orchestration memory",
		MemoryLimit:     1,
		Env:             codexIntegrationEnvironment(t),
	})
	if err != nil {
		t.Fatalf("orchestrator.Run() error = %v", err)
	}
	if output.Output != "PAJE_CODEX_ORCHESTRATED_OK" {
		t.Errorf("terminal response = %q, want PAJE_CODEX_ORCHESTRATED_OK", output.Output)
	}

	memories := store.Memories()
	if len(memories) != 2 {
		t.Fatalf("saved memories = %d, want seed plus outcome", len(memories))
	}
	if !strings.Contains(memories[1].Content, "PAJE_CODEX_ORCHESTRATED_OK") {
		t.Errorf("outcome memory = %q, want terminal response", memories[1].Content)
	}

	entries, err := os.ReadDir(filepath.Join(workspaceRoot, "worktrees"))
	if err != nil {
		t.Fatalf("read worktree root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("worktree root entries = %v, want empty after cleanup", entries)
	}
}

func codexIntegrationEnvironment(t *testing.T) map[string]string {
	t.Helper()

	environment := make(map[string]string)
	for _, key := range []string{"PATH", "HOME", "CODEX_HOME", "CODEX_API_KEY"} {
		if value := os.Getenv(key); value != "" {
			environment[key] = value
		}
	}
	if environment["PATH"] == "" || environment["HOME"] == "" {
		t.Fatal("PAJE_CODEX_INTEGRATION=1 requires PATH and HOME for authenticated Codex execution")
	}
	return environment
}

func newIntegrationRepository(t *testing.T) string {
	t.Helper()

	repository := t.TempDir()
	runIntegrationGit(t, repository, "init", "-b", "main")
	runIntegrationGit(t, repository, "config", "user.name", "Pajé integration")
	runIntegrationGit(t, repository, "config", "user.email", "paje@example.test")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("# Pajé integration fixture\n"), 0o600); err != nil {
		t.Fatalf("write README fixture: %v", err)
	}
	runIntegrationGit(t, repository, "add", "README.md")
	runIntegrationGit(t, repository, "commit", "-m", "seed fixture")
	return repository
}

func runIntegrationGit(t *testing.T, directory string, args ...string) {
	t.Helper()

	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
}
