package gitcapture_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/araihu/paje/internal/artifact/gitcapture"
)

func TestCaptureAndApplyReproduceEveryGitChangeWithoutMutatingSourceIndex(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Paje Test")
	git(t, repo, "config", "user.email", "paje@example.test")
	writeFile(t, filepath.Join(repo, "text.txt"), []byte("before\n"), 0o644)
	writeFile(t, filepath.Join(repo, "delete.txt"), []byte("delete me\n"), 0o644)
	writeFile(t, filepath.Join(repo, "rename.txt"), []byte("rename me\n"), 0o644)
	writeFile(t, filepath.Join(repo, "script.sh"), []byte("#!/bin/sh\necho before\n"), 0o644)
	writeFile(t, filepath.Join(repo, "binary.bin"), []byte{0, 1, 2, 3}, 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "base")
	base := git(t, repo, "rev-parse", "HEAD")

	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	git(t, repo, "worktree", "add", "--detach", source, base)
	git(t, repo, "worktree", "add", "--detach", target, base)
	t.Cleanup(func() {
		git(t, repo, "worktree", "remove", "--force", source)
		git(t, repo, "worktree", "remove", "--force", target)
	})

	writeFile(t, filepath.Join(source, "text.txt"), []byte("after\n"), 0o644)
	if err := os.Remove(filepath.Join(source, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(source, "rename.txt"), filepath.Join(source, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "new.txt"), []byte("untracked\n"), 0o644)
	if err := os.Chmod(filepath.Join(source, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("text.txt", filepath.Join(source, "relative-link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "binary.bin"), []byte{0, 9, 8, 0, 7}, 0o644)

	sourceIndex := readIndex(t, source)
	targetIndex := readIndex(t, target)
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	result, err := capturer.Capture(context.Background(), gitcapture.Request{
		Workspace: source,
		BaseSHA:   base,
		MaxBytes:  1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Patch) == 0 || result.TreeSHA == "" {
		t.Fatalf("Capture() returned incomplete result: %#v", result)
	}
	if got := readIndex(t, source); !bytes.Equal(got, sourceIndex) {
		t.Fatal("Capture() mutated the source worktree index")
	}

	if err := capturer.Apply(context.Background(), gitcapture.ApplyRequest{
		Workspace:       target,
		BaseSHA:         base,
		Patch:           result.Patch,
		ExpectedTreeSHA: result.TreeSHA,
	}); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(readIndex(t, target), targetIndex) {
		t.Fatal("Apply() did not intentionally update the reproduction index")
	}

	sourceTree, sourceStages := stagedFilesystem(t, source, base)
	targetTree, targetStages := stagedFilesystem(t, target, base)
	if sourceTree != result.TreeSHA {
		t.Fatalf("captured tree = %s, source filesystem tree = %s", result.TreeSHA, sourceTree)
	}
	if targetTree != sourceTree {
		t.Fatalf("reproduced tree = %s, source tree = %s", targetTree, sourceTree)
	}
	if !bytes.Equal(targetStages, sourceStages) {
		t.Fatalf("reproduced staged entries differ\nsource: %q\ntarget: %q", sourceStages, targetStages)
	}
}

func TestCaptureRejectsPatchOverConfiguredLimit(t *testing.T) {
	repo := initializedRepository(t)
	base := git(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "text.txt"), bytes.Repeat([]byte("x"), 4096), 0o644)
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	_, err = capturer.Capture(context.Background(), gitcapture.Request{Workspace: repo, BaseSHA: base, MaxBytes: 16})
	if !errors.Is(err, gitcapture.ErrPatchTooLarge) {
		t.Fatalf("Capture() error = %v, want ErrPatchTooLarge", err)
	}
}

func TestApplyRejectsTreeOtherThanCapturedTree(t *testing.T) {
	repo := initializedRepository(t)
	base := git(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "text.txt"), []byte("changed\n"), 0o644)
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	result, err := capturer.Capture(context.Background(), gitcapture.Request{Workspace: repo, BaseSHA: base, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "reproduction")
	git(t, repo, "worktree", "add", "--detach", worktree, base)
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", worktree) })
	err = capturer.Apply(context.Background(), gitcapture.ApplyRequest{
		Workspace:       worktree,
		BaseSHA:         base,
		Patch:           result.Patch,
		ExpectedTreeSHA: "0000000000000000000000000000000000000000",
	})
	if !errors.Is(err, gitcapture.ErrTreeMismatch) {
		t.Fatalf("Apply() error = %v, want ErrTreeMismatch", err)
	}
}

func initializedRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Paje Test")
	git(t, repo, "config", "user.email", "paje@example.test")
	writeFile(t, filepath.Join(repo, "text.txt"), []byte("base\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "base")
	return repo
}

func stagedFilesystem(t *testing.T, workspace, base string) (string, []byte) {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	gitWithIndex(t, workspace, index, "read-tree", base)
	gitWithIndex(t, workspace, index, "add", "-A", "--", ".")
	return gitWithIndex(t, workspace, index, "write-tree"), gitWithIndexBytes(t, workspace, index, "ls-files", "--stage", "-z")
}

func readIndex(t *testing.T, workspace string) []byte {
	t.Helper()
	path := git(t, workspace, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func writeFile(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return string(bytes.TrimSpace(gitBytes(t, dir, nil, args...)))
}

func gitWithIndex(t *testing.T, dir, index string, args ...string) string {
	t.Helper()
	return string(bytes.TrimSpace(gitBytes(t, dir, []string{"GIT_INDEX_FILE=" + index}, args...)))
}

func gitWithIndexBytes(t *testing.T, dir, index string, args ...string) []byte {
	t.Helper()
	return gitBytes(t, dir, []string{"GIT_INDEX_FILE=" + index}, args...)
}

func gitBytes(t *testing.T, dir string, extraEnv []string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), extraEnv...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return output
}
