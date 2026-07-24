package gitworktree_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	revision, err := manager.Resolve(context.Background(), source, "refs/heads/main")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	first, err := manager.Prepare(context.Background(), source, revision.SHA)
	if err != nil {
		t.Fatalf("Prepare(first) error = %v", err)
	}
	second, err := manager.Prepare(context.Background(), source, revision.SHA)
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
		sha     string
	}{
		{name: "missing repository", sha: strings.Repeat("a", 40)},
		{name: "missing SHA", repoURI: "repository"},
		{name: "non immutable SHA", repoURI: "repository", sha: "main"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, err := manager.Prepare(context.Background(), testCase.repoURI, testCase.sha); err == nil {
				t.Fatal("Prepare() error = nil, want validation error")
			}
		})
	}
}

func TestManagerResolvesImmutableRemoteRevisionAndPreparesIt(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	source := newRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "clone", "--bare", source, remote)
	manager, err := gitworktree.New(filepath.Join(t.TempDir(), "paje-workspaces"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	revision, err := manager.Resolve(ctx, remote, "refs/heads/main")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if revision.RepositoryURI != remote || revision.Ref != "refs/heads/main" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(revision.SHA) || revision.SourceDirty {
		t.Fatalf("revision = %#v", revision)
	}
	prepared, err := manager.Prepare(ctx, remote, revision.SHA)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	t.Cleanup(func() { _ = prepared.Cleanup(context.Background()) })
	assertFileContent(t, filepath.Join(prepared.Path(), "file.txt"), "original")
}

func TestManagerResolveDetectsDirtyLocalSourceWithoutChangingHEAD(t *testing.T) {
	requireGit(t)
	source := newRepository(t)
	before := gitOutput(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	manager, err := gitworktree.New(filepath.Join(t.TempDir(), "paje-workspaces"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	revision, err := manager.Resolve(context.Background(), source, "HEAD")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !revision.SourceDirty || revision.SHA != before {
		t.Fatalf("revision = %#v, want dirty source at %q", revision, before)
	}
	if after := gitOutput(t, source, "rev-parse", "HEAD"); after != before {
		t.Fatalf("Resolve() changed source HEAD from %q to %q", before, after)
	}
}

func TestManagerResolveRejectsUnsafeInputsBeforeGitArguments(t *testing.T) {
	requireGit(t)
	manager, err := gitworktree.New(filepath.Join(t.TempDir(), "paje-workspaces"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	source := newRepository(t)
	for _, testCase := range []struct{ uri, ref string }{
		{uri: "", ref: "HEAD"},
		{uri: "-repository", ref: "HEAD"},
		{uri: source, ref: ""},
		{uri: source, ref: "-c"},
		{uri: source, ref: "does-not-exist"},
	} {
		if _, err := manager.Resolve(context.Background(), testCase.uri, testCase.ref); err == nil {
			t.Fatalf("Resolve(%q, %q) error = nil", testCase.uri, testCase.ref)
		}
	}
}

func TestManagerRejectsSymlinkedManagedRootsAndMirrorBeforeGitOperations(t *testing.T) {
	requireGit(t)
	source := newRepository(t)
	root := filepath.Join(t.TempDir(), "paje-workspaces")
	manager, err := gitworktree.New(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for _, directory := range []string{"repositories", "worktrees"} {
		managed := filepath.Join(root, directory)
		if err := os.Remove(managed); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, managed); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Resolve(context.Background(), source, "HEAD"); err == nil {
			t.Fatalf("Resolve() accepted symlinked %s root", directory)
		}
		if err := os.Remove(managed); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(managed, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	key := repositoryKeyForTest(source)
	mirror := filepath.Join(root, "repositories", key+".git")
	if err := os.Symlink(outside, mirror); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(context.Background(), source, "HEAD"); err == nil {
		t.Fatal("Resolve() accepted a symlinked mirror")
	}
}

func TestWorkspaceCleanupRejectsSymlinkEscapingManagedRoot(t *testing.T) {
	requireGit(t)
	source := newRepository(t)
	manager, err := gitworktree.New(filepath.Join(t.TempDir(), "paje-workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	revision, err := manager.Resolve(context.Background(), source, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := manager.Prepare(context.Background(), source, revision.SHA)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "must-remain")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(prepared.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, prepared.Path()); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Cleanup(context.Background()); err == nil {
		t.Fatal("Cleanup() accepted a symlinked worktree path")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cleanup touched outside root: %v", err)
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

func gitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s error = %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
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

func repositoryKeyForTest(repoURI string) string {
	sum := sha256.Sum256([]byte(repoURI))
	return hex.EncodeToString(sum[:])
}
