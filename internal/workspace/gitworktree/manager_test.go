package gitworktree_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/workspace/gitworktree"
)

const (
	orphanHelperEnvironment = "PAJE_GITWORKTREE_ORPHAN_HELPER"
	orphanHelperRoot        = "PAJE_GITWORKTREE_ORPHAN_ROOT"
	orphanHelperSource      = "PAJE_GITWORKTREE_ORPHAN_SOURCE"
	orphanHelperSHA         = "PAJE_GITWORKTREE_ORPHAN_SHA"
	orphanHelperReady       = "PAJE_GITWORKTREE_ORPHAN_READY"
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

func TestManagerReapsWorkspaceAbandonedByKilledProcessAndPreservesActiveLease(t *testing.T) {
	if os.Getenv(orphanHelperEnvironment) == "1" {
		manager, err := gitworktree.New(os.Getenv(orphanHelperRoot))
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := manager.Prepare(
			context.Background(),
			os.Getenv(orphanHelperSource),
			os.Getenv(orphanHelperSHA),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(orphanHelperReady), []byte(prepared.Path()), 0o600); err != nil {
			t.Fatal(err)
		}
		for {
			runtime.KeepAlive(prepared)
			time.Sleep(time.Second)
		}
	}

	requireGit(t)
	root := filepath.Join(t.TempDir(), "manager")
	source := newRepository(t)
	sha := gitOutput(t, source, "rev-parse", "HEAD")
	activeManager, err := gitworktree.New(root)
	if err != nil {
		t.Fatal(err)
	}
	active, err := activeManager.Prepare(context.Background(), source, sha)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = active.Cleanup(context.Background()) })

	ready := filepath.Join(t.TempDir(), "orphan-ready")
	command := exec.Command(os.Args[0], "-test.run=^TestManagerReapsWorkspaceAbandonedByKilledProcessAndPreservesActiveLease$")
	command.Env = append(os.Environ(),
		orphanHelperEnvironment+"=1",
		orphanHelperRoot+"="+root,
		orphanHelperSource+"="+source,
		orphanHelperSHA+"="+sha,
		orphanHelperReady+"="+ready,
	)
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	orphanPath := waitForOrphanWorkspace(t, ready, waited, &childOutput)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err == nil {
		t.Fatal("orphan helper exited cleanly after kill")
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("abandoned workspace missing before recovery: %v", err)
	}

	if _, err := gitworktree.New(root); err != nil {
		t.Fatalf("New(recovery) error = %v", err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned workspace remains after recovery: %v", err)
	}
	assertFileContent(t, filepath.Join(active.Path(), "file.txt"), "original")
}

func TestManagerRecoveryRejectsSymlinkAndPreservesForeignDirectory(t *testing.T) {
	requireGit(t)
	root := filepath.Join(t.TempDir(), "manager")
	if _, err := gitworktree.New(root); err != nil {
		t.Fatal(err)
	}
	worktreesRoot := filepath.Join(root, "worktrees")
	foreign := filepath.Join(worktreesRoot, "foreign")
	if err := os.Mkdir(foreign, 0o750); err != nil {
		t.Fatal(err)
	}
	foreignMarker := filepath.Join(foreign, "must-survive")
	if err := os.WriteFile(foreignMarker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideMarker := filepath.Join(outside, "must-survive")
	if err := os.WriteFile(outsideMarker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worktreesRoot, "paje-123456")); err != nil {
		t.Fatal(err)
	}

	if _, err := gitworktree.New(root); err == nil {
		t.Fatal("New() accepted symlinked managed workspace candidate")
	}
	assertFileContent(t, foreignMarker, "safe")
	assertFileContent(t, outsideMarker, "safe")
}

func waitForOrphanWorkspace(t *testing.T, ready string, waited <-chan error, output *bytes.Buffer) string {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-waited:
			t.Fatalf("orphan helper exited before ready: %v\n%s", err, output.String())
		case <-deadline.C:
			t.Fatalf("orphan helper did not become ready\n%s", output.String())
		case <-ticker.C:
			encoded, err := os.ReadFile(ready)
			if err == nil {
				path := strings.TrimSpace(string(encoded))
				if path == "" {
					t.Fatal("orphan helper wrote empty workspace path")
				}
				return path
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
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

func TestPreparedWorkspaceIsSelfContainedInsideSingleBind(t *testing.T) {
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
	t.Cleanup(func() { _ = prepared.Cleanup(context.Background()) })

	gitMetadata := filepath.Join(prepared.Path(), ".git")
	info, err := os.Lstat(gitMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("workspace .git is not an in-bind directory: mode=%v", info.Mode())
	}
	for _, args := range [][]string{
		{"rev-parse", "--absolute-git-dir"},
		{"rev-parse", "--path-format=absolute", "--git-common-dir"},
		{"rev-parse", "--path-format=absolute", "--git-path", "objects"},
	} {
		resolved := gitOutput(t, prepared.Path(), args...)
		if err := pathWithinForTest(prepared.Path(), resolved); err != nil {
			t.Fatalf("git %s escaped the single workspace bind: %q: %v", strings.Join(args, " "), resolved, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(gitMetadata, "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace uses object alternates: %v", err)
	}
	command := exec.Command("git", "config", "--local", "--get-regexp", `^(remote\.|credential\.)`)
	command.Dir = prepared.Path()
	output, err := command.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("inspect local remotes/credentials: %v\n%s", err, output)
		}
	}
	if len(output) != 0 {
		t.Fatalf("workspace retains live remote or credential config: %s", output)
	}
}

func TestWorkspaceCleanupRefusesReboundDirectoryIdentity(t *testing.T) {
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
	path := prepared.Path()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "replacement-must-remain")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := prepared.Cleanup(context.Background()); err == nil {
		t.Fatal("Cleanup() accepted a rebound workspace directory")
	}
	assertFileContent(t, marker, "safe")
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

func pathWithinForTest(root, candidate string) error {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("path escapes root")
	}
	return nil
}
