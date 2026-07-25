package gitcapture

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
)

func TestCleanupTreeChecksContextDuringTraversal(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a")
	second := filepath.Join(root, "b")
	if err := os.WriteFile(first, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ops := defaultCleanupOps
	ops.remove = func(path string) error {
		err := os.Remove(path)
		if path == first {
			cancel()
		}
		return err
	}

	err := cleanupTree(ctx, root, ops)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanupTree() error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("cleanup continued after cancellation: %v", err)
	}
}

func TestCleanupTreeJoinsIndependentRemovalFailures(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a")
	second := filepath.Join(root, "b")
	if err := os.WriteFile(first, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstFailure := errors.New("remove first")
	secondFailure := errors.New("remove second")
	ops := defaultCleanupOps
	ops.remove = func(path string) error {
		switch path {
		case first:
			return firstFailure
		case second:
			return secondFailure
		default:
			return os.Remove(path)
		}
	}

	err := cleanupTree(context.Background(), root, ops)
	if !errors.Is(err, firstFailure) || !errors.Is(err, secondFailure) {
		t.Fatalf("cleanupTree() error = %v, want both injected failures", err)
	}
}

func TestCrossValidateRejectsMalformedAndMismatchedObjectIDs(t *testing.T) {
	zeros := strings.Repeat("0", 40)
	ones := strings.Repeat("1", 40)
	twos := strings.Repeat("2", 40)
	change := []artifact.Change{{Path: "a", Status: "A", OldMode: "000000", NewMode: "100644"}}
	nameStatus := []byte("A\x00a\x00")
	for _, tc := range []struct {
		name, rawObject, stagedObject string
	}{
		{name: "mismatch", rawObject: ones, stagedObject: twos},
		{name: "malformed matching values", rawObject: "not-an-object", stagedObject: "not-an-object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(":000000 100644 " + zeros + " " + tc.rawObject + " A\x00a\x00")
			stages := []byte("100644 " + tc.stagedObject + " 0\ta\x00")
			err := crossValidateChanges(change, raw, nameStatus, stages)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("crossValidateChanges() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestCrossValidateAcceptsSHA256DeletionWithoutAStagedEntry(t *testing.T) {
	zeros := strings.Repeat("0", 64)
	ones := strings.Repeat("1", 64)
	changes := []artifact.Change{{Path: "gone", Status: "D", OldMode: "100644", NewMode: "000000"}}
	raw := []byte(":100644 000000 " + ones + " " + zeros + " D\x00gone\x00")
	if err := crossValidateChanges(changes, raw, []byte("D\x00gone\x00"), nil); err != nil {
		t.Fatalf("crossValidateChanges() error = %v, want SHA-256 deletion accepted", err)
	}
}

func TestValidatePatchPathsChecksEveryHeaderAndIgnoresHunkContent(t *testing.T) {
	patch := "" +
		"diff --git \"a/old\\tname\" \"b/new\\nname\"\n" +
		"similarity index 90%\n" +
		"rename from \"old\\tname\"\n" +
		"rename to \"new\\nname\"\n" +
		"--- \"a/old\\tname\"\n" +
		"+++ \"b/new\\nname\"\n" +
		"@@ -1 +1,3 @@\n" +
		"-old\n" +
		"+new\n" +
		"+++ b/../hunk-content-is-not-a-header\n" +
		"+rename from ../also-content\n" +
		"diff --git a/source b/copied\n" +
		"similarity index 100%\n" +
		"copy from source\n" +
		"copy to copied\n" +
		"diff --git a/added b/added\n" +
		"new file mode 100644\n" +
		"--- /dev/null\n" +
		"+++ b/added\n" +
		"@@ -0,0 +1 @@\n" +
		"+added\n" +
		"diff --git a/deleted b/deleted\n" +
		"deleted file mode 100644\n" +
		"--- a/deleted\n" +
		"+++ /dev/null\n" +
		"@@ -1 +0,0 @@\n" +
		"-deleted\n"
	if err := validatePatchPaths([]byte(patch)); err != nil {
		t.Fatalf("validatePatchPaths() error = %v, want valid stateful patch", err)
	}
}

func TestValidatePatchPathsRejectsUnsafeOrInconsistentEffectivePaths(t *testing.T) {
	tests := map[string]string{
		"diff old":      "diff --git a/../escape b/safe\n",
		"diff new":      "diff --git a/safe b/../escape\n",
		"old marker":    "diff --git a/safe b/safe\n--- a/../escape\n+++ b/safe\n",
		"new marker":    "diff --git a/safe b/safe\n--- a/safe\n+++ b/../escape\n",
		"rename from":   "diff --git a/safe b/new\nrename from ../escape\nrename to new\n",
		"rename to":     "diff --git a/old b/safe\nrename from old\nrename to ../escape\n",
		"copy from":     "diff --git a/safe b/new\ncopy from ../escape\ncopy to new\n",
		"copy to":       "diff --git a/old b/safe\ncopy from old\ncopy to ../escape\n",
		"quoted escape": "diff --git \"a/\\056\\056/escape\" b/safe\n",
		"both dev null": "diff --git a/safe b/safe\n--- /dev/null\n+++ /dev/null\n",
		"one marker":    "diff --git a/safe b/safe\n--- a/safe\n",
		"mismatch":      "diff --git a/old b/new\nrename from other\nrename to new\n",
	}
	for name, patch := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validatePatchPaths([]byte(patch)); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("validatePatchPaths() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestApplyTimeoutDuringRealMutationRemovesOperationLockAndRestoresTarget(t *testing.T) {
	repo := t.TempDir()
	testGit(t, repo, "init")
	testGit(t, repo, "config", "user.name", "Paje Test")
	testGit(t, repo, "config", "user.email", "paje@example.test")
	if err := os.WriteFile(filepath.Join(repo, "text.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", "-A")
	testGit(t, repo, "commit", "-m", "base")
	base := testGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "text.txt"), []byte("captured\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	capturer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	result, err := capturer.Capture(context.Background(), Request{Workspace: repo, BaseSHA: base, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	testGit(t, repo, "worktree", "add", "--detach", target, base)
	t.Cleanup(func() { testGit(t, repo, "worktree", "remove", "--force", target) })
	beforeStages := testGitBytes(t, target, "ls-files", "--stage", "-z")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	mutated := filepath.Join(t.TempDir(), "mutated")
	fake := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = apply ] && [ \"$2\" = --index ]; then\n" +
		"  printf 'partial mutation\\n' > \"$PWD/text.txt\"\n" +
		"  : > \"$GIT_INDEX_FILE.lock\"\n" +
		"  /bin/chmod 600 \"$GIT_INDEX_FILE.lock\"\n" +
		"  : > " + testShellQuote(mutated) + "\n" +
		"  exec /bin/sleep 30\n" +
		"fi\n" +
		"exec " + testShellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))
	capturer, err = New()
	if err != nil {
		t.Fatal(err)
	}
	capturer.applyTimeout = 150 * time.Millisecond

	err = capturer.Apply(context.Background(), ApplyRequest{
		Workspace:       target,
		BaseSHA:         base,
		Patch:           result.Patch,
		ExpectedTreeSHA: result.TreeSHA,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply() error = %v, want context.DeadlineExceeded joined with restoration", err)
	}
	if _, err := os.Stat(mutated); err != nil {
		t.Fatalf("timeout fired before real mutation harness: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(target, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "base\n" {
		t.Fatalf("timed-out Apply left filesystem mutation: %q", contents)
	}
	if status := testGit(t, target, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching"); status != "" {
		t.Fatalf("timed-out Apply left target state: %q", status)
	}
	if stages := testGitBytes(t, target, "ls-files", "--stage", "-z"); !bytes.Equal(stages, beforeStages) {
		t.Fatalf("timed-out Apply left different index\nbefore: %q\nafter:  %q", beforeStages, stages)
	}
	index := testGit(t, target, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(index) {
		index = filepath.Join(target, index)
	}
	if _, err := os.Lstat(index + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("timed-out Apply left index lock: %v", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(resolvedTarget), ".paje-git-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("timed-out Apply left private temp trees: %v, %v", matches, err)
	}
}

func TestRemoveOperationIndexLockNeverRemovesUnsafeEntry(t *testing.T) {
	for _, kind := range []string{"symlink", "writable"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			index := filepath.Join(directory, "index")
			state, err := prepareIndexLock(index)
			if err != nil {
				t.Fatal(err)
			}
			lock := index + ".lock"
			switch kind {
			case "symlink":
				if err := os.WriteFile(filepath.Join(directory, "target"), []byte("target"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("target", lock); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.WriteFile(lock, []byte("unsafe"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(lock, 0o666); err != nil {
					t.Fatal(err)
				}
			}
			if err := removeOperationIndexLock(state); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("removeOperationIndexLock() error = %v, want ErrInvalidRequest", err)
			}
			if _, err := os.Lstat(lock); err != nil {
				t.Fatalf("unsafe lock was removed: %v", err)
			}
		})
	}
}

func TestRestoreVerificationReportsIgnoredResidue(t *testing.T) {
	repo := t.TempDir()
	testGit(t, repo, "init")
	testGit(t, repo, "config", "user.name", "Paje Test")
	testGit(t, repo, "config", "user.email", "paje@example.test")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "text.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", "-A")
	testGit(t, repo, "commit", "-m", "base")
	base := testGit(t, repo, "rev-parse", "HEAD")
	ignored := filepath.Join(repo, "ignored.cache")
	if err := os.WriteFile(ignored, []byte("residue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := realIndexPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := prepareIndexLock(index)
	if err != nil {
		t.Fatal(err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = clean ]; then exit 0; fi\n" +
		"exec " + testShellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))
	capturer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	primary := errors.New("injected transaction failure")
	err = capturer.restoreTarget(context.Background(), repo, index, base, lock, primary)
	if !errors.Is(err, primary) || !strings.Contains(err.Error(), "target remains dirty") {
		t.Fatalf("restoreTarget() error = %v, want primary plus ignored-residue verification", err)
	}
	if _, err := os.Stat(ignored); err != nil {
		t.Fatalf("fake clean unexpectedly removed ignored residue: %v", err)
	}
}

func testGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(testGitBytes(t, directory, args...)))
}

func testGitBytes(t *testing.T, directory string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return output
}

func testShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
