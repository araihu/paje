package gitworktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestManagerPreparesIsolatedWorktreesAndCleansThem(t *testing.T) {
	t.Parallel()
	requireGit(t)

	source := newRepository(t)
	manager, err := gitworktree.New(filepath.Join(t.TempDir(), "paje-workspaces"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first, err := manager.Prepare(context.Background(), source, "main")
	if err != nil {
		t.Fatalf("Prepare(first) error = %v", err)
	}
	second, err := manager.Prepare(context.Background(), source, "main")
	if err != nil {
		t.Fatalf("Prepare(second) error = %v", err)
	}
	t.Cleanup(func() {
		_ = first.Cleanup(context.Background())
		_ = second.Cleanup(context.Background())
	})

	if first.Path() == second.Path() {
		t.Fatalf("Prepare() returned duplicate path %q", first.Path())
	}
	assertFileContent(t, filepath.Join(first.Path(), "file.txt"), "original")
	assertFileContent(t, filepath.Join(second.Path(), "file.txt"), "original")

	if err := os.WriteFile(filepath.Join(first.Path(), "file.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile(first worktree) error = %v", err)
	}
	assertFileContent(t, filepath.Join(second.Path(), "file.txt"), "original")
	assertFileContent(t, filepath.Join(source, "file.txt"), "original")

	firstPath := first.Path()
	if err := first.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup(first) error = %v", err)
	}
	if err := first.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup(first, again) error = %v", err)
	}
	if _, err := os.Stat(firstPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(cleaned workspace) error = %v, want not exist", err)
	}
}

func TestManagerValidatesInputs(t *testing.T) {
	t.Parallel()
	requireGit(t)

	if _, err := gitworktree.New(""); err == nil {
		t.Fatal("New(\"\") error = nil, want validation error")
	}
	manager, err := gitworktree.New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	testCases := []struct {
		name    string
		repoURI string
		branch  string
	}{
		{name: "missing repository", branch: "main"},
		{name: "missing branch", repoURI: "repository"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, err := manager.Prepare(context.Background(), testCase.repoURI, testCase.branch); err == nil {
				t.Fatal("Prepare() error = nil, want validation error")
			}
		})
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable is not installed")
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	runGit(t, "", "init", "-b", "main", path)
	runGit(t, path, "config", "user.name", "Paje Test")
	runGit(t, path, "config", "user.email", "paje@example.test")
	if err := os.WriteFile(filepath.Join(path, "file.txt"), []byte("original"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runGit(t, path, "add", "file.txt")
	runGit(t, path, "commit", "-m", "initial")
	return path
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s error = %v\n%s", strings.Join(args, " "), err, output)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != want {
		t.Errorf("ReadFile(%q) = %q, want %q", path, content, want)
	}
}
