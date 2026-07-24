package gitcapture_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/policy"
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
	writeFile(t, filepath.Join(repo, "unchanged-source.txt"), []byte("copy-only-content\n"), 0o644)
	oddOld := "odd\tcafé\nold.txt"
	oddNew := "odd\tcafé\nnew.txt"
	writeFile(t, filepath.Join(repo, oddOld), []byte("odd path\n"), 0o644)
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
	if err := os.Rename(filepath.Join(source, oddOld), filepath.Join(source, oddNew)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "copied-unchanged.txt"), []byte("copy-only-content\n"), 0o644)
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
	wantChanges := []artifact.Change{
		{Path: "binary.bin", Status: "M", OldMode: "100644", NewMode: "100644"},
		{Path: "copied-unchanged.txt", OldPath: "unchanged-source.txt", Status: "C", OldMode: "100644", NewMode: "100644"},
		{Path: "delete.txt", Status: "D", OldMode: "100644", NewMode: "000000"},
		{Path: "new.txt", Status: "A", OldMode: "000000", NewMode: "100644"},
		{Path: oddNew, OldPath: oddOld, Status: "R", OldMode: "100644", NewMode: "100644"},
		{Path: "relative-link", Status: "A", OldMode: "000000", NewMode: "120000"},
		{Path: "renamed.txt", OldPath: "rename.txt", Status: "R", OldMode: "100644", NewMode: "100644"},
		{Path: "script.sh", Status: "M", OldMode: "100644", NewMode: "100755"},
		{Path: "text.txt", Status: "M", OldMode: "100644", NewMode: "100644"},
	}
	if !reflect.DeepEqual(result.Changes, wantChanges) {
		t.Fatalf("Capture() changes = %#v, want %#v", result.Changes, wantChanges)
	}
	if !bytes.Contains(result.Patch, []byte("copy from unchanged-source.txt\ncopy to copied-unchanged.txt\n")) {
		t.Fatalf("Capture() patch does not retain unchanged-source copy metadata:\n%s", result.Patch)
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

func TestApplyRejectsUntrackedTargetWithoutMutatingIt(t *testing.T) {
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
	target := filepath.Join(t.TempDir(), "target")
	git(t, repo, "worktree", "add", "--detach", target, base)
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", target) })
	writeFile(t, filepath.Join(target, "untracked.txt"), []byte("must block apply\n"), 0o644)
	beforeIndex := readIndex(t, target)
	beforeText, err := os.ReadFile(filepath.Join(target, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	err = capturer.Apply(context.Background(), gitcapture.ApplyRequest{Workspace: target, BaseSHA: base, Patch: result.Patch, ExpectedTreeSHA: result.TreeSHA})
	if !errors.Is(err, gitcapture.ErrDirtyIndex) {
		t.Fatalf("Apply() error = %v, want ErrDirtyIndex", err)
	}
	afterText, err := os.ReadFile(filepath.Join(target, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readIndex(t, target), beforeIndex) || !bytes.Equal(afterText, beforeText) {
		t.Fatal("Apply() mutated a target rejected for untracked state")
	}
}

func TestApplyTreeMismatchLeavesWorktreeAndIndexUnchanged(t *testing.T) {
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
	target := filepath.Join(t.TempDir(), "target")
	git(t, repo, "worktree", "add", "--detach", target, base)
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", target) })
	beforeIndex := readIndex(t, target)
	beforeText, err := os.ReadFile(filepath.Join(target, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	err = capturer.Apply(context.Background(), gitcapture.ApplyRequest{Workspace: target, BaseSHA: base, Patch: result.Patch, ExpectedTreeSHA: "0000000000000000000000000000000000000000"})
	if !errors.Is(err, gitcapture.ErrTreeMismatch) {
		t.Fatalf("Apply() error = %v, want ErrTreeMismatch", err)
	}
	afterText, err := os.ReadFile(filepath.Join(target, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readIndex(t, target), beforeIndex) || !bytes.Equal(afterText, beforeText) {
		t.Fatal("tree mismatch mutated target before proof completed")
	}
}

func TestApplyCancellationAfterRealApplyRestoresPristineTarget(t *testing.T) {
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
	target := filepath.Join(t.TempDir(), "target")
	git(t, repo, "worktree", "add", "--detach", target, base)
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", target) })
	beforeStages := gitBytes(t, target, nil, "ls-files", "--stage", "-z")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	control := t.TempDir()
	applied := filepath.Join(control, "applied")
	release := filepath.Join(control, "release")
	fake := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = apply ] && [ \"$2\" = --index ]; then\n" +
		"  " + shellQuote(realGit) + " \"$@\"\n" +
		"  status=$?\n" +
		"  : > " + shellQuote(applied) + "\n" +
		"  while [ ! -f " + shellQuote(release) + " ]; do /bin/sleep 0.01; done\n" +
		"  exit $status\n" +
		"fi\n" +
		"exec " + shellQuote(realGit) + " \"$@\"\n"
	writeFile(t, fake, []byte(script), 0o755)
	t.Setenv("PATH", filepath.Dir(fake))
	capturer, err = gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	applyResult := make(chan error, 1)
	go func() {
		applyResult <- capturer.Apply(ctx, gitcapture.ApplyRequest{
			Workspace:       target,
			BaseSHA:         base,
			Patch:           result.Patch,
			ExpectedTreeSHA: result.TreeSHA,
		})
	}()
	waitForFile(t, applied, applyResult)
	contents, err := os.ReadFile(filepath.Join(target, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "changed\n" {
		t.Fatalf("barrier fired before real apply mutation: text.txt = %q", contents)
	}
	cancel()
	writeFile(t, release, nil, 0o600)
	err = <-applyResult
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply() error = %v, want context.Canceled joined after rollback", err)
	}
	contents, err = os.ReadFile(filepath.Join(target, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "base\n" {
		t.Fatalf("canceled Apply left target mutation: text.txt = %q", contents)
	}
	if status := git(t, target, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("canceled Apply left dirty target: %q", status)
	}
	if stages := gitBytes(t, target, nil, "ls-files", "--stage", "-z"); !bytes.Equal(stages, beforeStages) {
		t.Fatalf("canceled Apply left different index\nbefore: %q\nafter:  %q", beforeStages, stages)
	}
	index := git(t, target, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(index) {
		index = filepath.Join(target, index)
	}
	if _, err := os.Stat(index + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled Apply left index lock: %v", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(resolvedTarget), ".paje-git-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("canceled Apply left private temp trees: %v, %v", matches, err)
	}
}

func TestApplyRestoresTargetAfterApplyOrPostVerifyFailure(t *testing.T) {
	for _, failurePoint := range []string{"apply", "post-verify"} {
		t.Run(failurePoint, func(t *testing.T) {
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
			target := filepath.Join(t.TempDir(), "target")
			git(t, repo, "worktree", "add", "--detach", target, base)
			t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", target) })
			beforeStages := gitBytes(t, target, nil, "ls-files", "--stage", "-z")

			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			control := t.TempDir()
			mutated := filepath.Join(control, "mutated")
			failed := filepath.Join(control, "failed")
			fake := filepath.Join(t.TempDir(), "git")
			script := "#!/bin/sh\n" +
				"if [ \"$1\" = apply ] && [ \"$2\" = --index ]; then\n" +
				"  " + shellQuote(realGit) + " \"$@\"\n" +
				"  status=$?\n" +
				"  : > " + shellQuote(mutated) + "\n"
			if failurePoint == "apply" {
				script += "  exit 85\n"
			} else {
				script += "  exit $status\n"
			}
			if failurePoint == "post-verify" {
				script += "fi\n" +
					"if [ \"$1\" = write-tree ] && [ -f " + shellQuote(mutated) + " ] && [ ! -f " + shellQuote(failed) + " ]; then\n" +
					"  : > " + shellQuote(failed) + "\n" +
					"  exit 86\n"
			}
			script += "fi\nexec " + shellQuote(realGit) + " \"$@\"\n"
			writeFile(t, fake, []byte(script), 0o755)
			t.Setenv("PATH", filepath.Dir(fake))
			capturer, err = gitcapture.New()
			if err != nil {
				t.Fatal(err)
			}
			err = capturer.Apply(context.Background(), gitcapture.ApplyRequest{
				Workspace:       target,
				BaseSHA:         base,
				Patch:           result.Patch,
				ExpectedTreeSHA: result.TreeSHA,
			})
			if err == nil {
				t.Fatal("Apply() error = nil, want injected failure")
			}
			if _, err := os.Stat(mutated); err != nil {
				t.Fatalf("failure occurred before real apply mutation: %v", err)
			}
			contents, err := os.ReadFile(filepath.Join(target, "text.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != "base\n" {
				t.Fatalf("failed Apply left target mutation: text.txt = %q", contents)
			}
			if status := git(t, target, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
				t.Fatalf("failed Apply left dirty target: %q", status)
			}
			if stages := gitBytes(t, target, nil, "ls-files", "--stage", "-z"); !bytes.Equal(stages, beforeStages) {
				t.Fatalf("failed Apply left different index\nbefore: %q\nafter:  %q", beforeStages, stages)
			}
		})
	}
}

func TestCaptureRejectsGitAndIndexSymlinks(t *testing.T) {
	for _, kind := range []string{"git", "index"} {
		t.Run(kind, func(t *testing.T) {
			repo := initializedRepository(t)
			base := git(t, repo, "rev-parse", "HEAD")
			if kind == "git" {
				if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, "git-data")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("git-data", filepath.Join(repo, ".git")); err != nil {
					t.Fatal(err)
				}
			} else {
				index := filepath.Join(repo, ".git", "index")
				if err := os.Rename(index, index+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("index.real", index); err != nil {
					t.Fatal(err)
				}
			}
			capturer, err := gitcapture.New()
			if err != nil {
				t.Fatal(err)
			}
			_, err = capturer.Capture(context.Background(), gitcapture.Request{Workspace: repo, BaseSHA: base, MaxBytes: 1 << 20})
			if !errors.Is(err, gitcapture.ErrInvalidRequest) {
				t.Fatalf("Capture() error = %v, want ErrInvalidRequest for malicious %s link", err, kind)
			}
		})
	}
}

func TestCapturePreservesGitlinkForPolicyDenial(t *testing.T) {
	repo := initializedRepository(t)
	base := git(t, repo, "rev-parse", "HEAD")
	module := filepath.Join(repo, "module")
	if err := os.Mkdir(module, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, module, "init")
	git(t, module, "config", "user.name", "Paje Test")
	git(t, module, "config", "user.email", "paje@example.test")
	writeFile(t, filepath.Join(module, "module.txt"), []byte("module\n"), 0o644)
	git(t, module, "add", "-A")
	git(t, module, "commit", "-m", "module")
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	result, err := capturer.Capture(context.Background(), gitcapture.Request{Workspace: repo, BaseSHA: base, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 || result.Changes[0] != (artifact.Change{Path: "module", Status: "A", OldMode: "000000", NewMode: "160000"}) {
		t.Fatalf("Capture() changes = %#v, want preserved gitlink", result.Changes)
	}
	evaluator, err := policy.NewChangePolicy(policy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	decision := evaluator.Evaluate(context.Background(), result)
	if decision.Allowed || len(decision.Findings) != 1 || decision.Findings[0].RuleID != "mode-gitlink" || decision.Findings[0].Path != "module" {
		t.Fatalf("policy decision = %#v, want gitlink denial", decision)
	}
}

func TestApplyRejectsExactFortyOneCharacterSHAWithoutMutation(t *testing.T) {
	repo := initializedRepository(t)
	base := git(t, repo, "rev-parse", "HEAD")
	beforeIndex := readIndex(t, repo)
	beforeText, err := os.ReadFile(filepath.Join(repo, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	err = capturer.Apply(context.Background(), gitcapture.ApplyRequest{Workspace: repo, BaseSHA: base + "0", Patch: []byte("diff --git a/text.txt b/text.txt\n"), ExpectedTreeSHA: base})
	if !errors.Is(err, gitcapture.ErrInvalidRequest) {
		t.Fatalf("Apply() error = %v, want pre-mutation SHA validation error", err)
	}
	afterText, err := os.ReadFile(filepath.Join(repo, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeIndex, readIndex(t, repo)) || !bytes.Equal(beforeText, afterText) {
		t.Fatal("invalid 41-character SHA mutated worktree or index")
	}
}

func TestApplyValidatesEveryPatchPathBeforeInvokingGit(t *testing.T) {
	workspace := fakeWorkspace(t)
	invoked := filepath.Join(t.TempDir(), "git-invoked")
	fake := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n: > " + shellQuote(invoked) + "\nexit 97\n"
	writeFile(t, fake, []byte(script), 0o755)
	t.Setenv("PATH", filepath.Dir(fake))
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	base := strings.Repeat("a", 40)
	patch := []byte("diff --git a/safe b/new\nrename from ../escape\nrename to new\n")
	err = capturer.Apply(context.Background(), gitcapture.ApplyRequest{
		Workspace:       workspace,
		BaseSHA:         base,
		Patch:           patch,
		ExpectedTreeSHA: base,
	})
	if !errors.Is(err, gitcapture.ErrInvalidRequest) {
		t.Fatalf("Apply() error = %v, want ErrInvalidRequest", err)
	}
	if _, err := os.Stat(invoked); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git was invoked before patch validation: %v", err)
	}
}

func TestCaptureRejectsNameStatusAndStageCrossValidationMismatch(t *testing.T) {
	workspace := fakeWorkspace(t)
	base := strings.Repeat("a", 40)
	fake := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"*--show-toplevel*) printf '%s\\n' '" + workspace + "' ;;\n" +
		"*'rev-parse HEAD'*) printf '%s\\n' '" + base + "' ;;\n" +
		"*--verify*) printf '%s\\n' '" + base + "' ;;\n" +
		"*'diff --quiet'*) exit 0 ;;\n" +
		"*'diff --cached --binary'*) printf 'diff --git a/a b/a\\n' ;;\n" +
		"*'diff --cached --raw'*) printf ':000000 100644 0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 A\\000a\\000' ;;\n" +
		"*'diff --cached --name-status'*) printf 'A\\000b\\000' ;;\n" +
		"*'ls-files --stage'*) printf '100644 1111111111111111111111111111111111111111 0\\ta\\000' ;;\n" +
		"*write-tree*) printf '%s\\n' '" + base + "' ;;\n" +
		"*) : > \"$GIT_INDEX_FILE\" ;;\n" +
		"esac\n"
	writeFile(t, fake, []byte(script), 0o755)
	t.Setenv("PATH", filepath.Dir(fake))
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	_, err = capturer.Capture(context.Background(), gitcapture.Request{Workspace: workspace, BaseSHA: base, MaxBytes: 1 << 20})
	if !errors.Is(err, gitcapture.ErrInvalidRequest) {
		t.Fatalf("Capture() error = %v, want cross-validation rejection", err)
	}
}

func TestCaptureCancelsGitDescendants(t *testing.T) {
	workspace := fakeWorkspace(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	fake := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n/bin/sleep 30 &\necho $! > '" + pidFile + "'\nwait\n"
	writeFile(t, fake, []byte(script), 0o755)
	t.Setenv("PATH", filepath.Dir(fake))
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := capturer.Capture(ctx, gitcapture.Request{Workspace: workspace, BaseSHA: strings.Repeat("a", 40), MaxBytes: 1 << 20})
		result <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(pidFile); statErr == nil {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("Capture() returned before starting its descendant: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			err := <-result
			t.Fatalf("fake Git did not start its descendant; Capture() error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err = <-result
	if err == nil {
		t.Fatal("Capture() error = nil, want canceled process")
	}
	pidData, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	deadline = time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("Git descendant %d survived cancellation", pid)
	}
}

func TestCaptureUsesTempOutsideWorkspaceAndCleansIt(t *testing.T) {
	repo := initializedRepository(t)
	base := git(t, repo, "rev-parse", "HEAD")
	t.Setenv("TMPDIR", repo)
	writeFile(t, filepath.Join(repo, "text.txt"), []byte("changed\n"), 0o644)
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capturer.Capture(context.Background(), gitcapture.Request{Workspace: repo, BaseSHA: base, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(repo, ".paje-git-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary Git directories remain in workspace: %v, %v", matches, err)
	}
}

func TestCaptureTempTreeIsAnObservablePrivateSiblingAndIsRemoved(t *testing.T) {
	repo := initializedRepository(t)
	base := git(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "text.txt"), []byte("changed\n"), 0o644)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	control := t.TempDir()
	observed := filepath.Join(control, "index-path")
	release := filepath.Join(control, "release")
	fake := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"case \"$GIT_INDEX_FILE\" in\n" +
		"*/.paje-git-*/index)\n" +
		"  if [ ! -f " + shellQuote(release) + " ]; then\n" +
		"    printf '%s' \"$GIT_INDEX_FILE\" > " + shellQuote(observed) + "\n" +
		"    while [ ! -f " + shellQuote(release) + " ]; do /bin/sleep 0.01; done\n" +
		"  fi\n" +
		"  ;;\n" +
		"esac\n" +
		"exec " + shellQuote(realGit) + " \"$@\"\n"
	writeFile(t, fake, []byte(script), 0o755)
	t.Setenv("PATH", filepath.Dir(fake))
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := capturer.Capture(context.Background(), gitcapture.Request{Workspace: repo, BaseSHA: base, MaxBytes: 1 << 20})
		result <- err
	}()
	indexPath := waitForFileContents(t, observed, result)
	tempTree := filepath.Dir(indexPath)
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(tempTree) != filepath.Dir(resolvedRepo) || tempTree == resolvedRepo {
		t.Fatalf("temporary tree = %q, want sibling of %q", tempTree, repo)
	}
	info, err := os.Stat(tempTree)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("temporary tree mode = %o, want 0700", info.Mode().Perm())
	}
	indexInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if indexInfo.Mode().Perm() != 0o600 {
		t.Fatalf("temporary index mode = %o, want 0600", indexInfo.Mode().Perm())
	}
	writeFile(t, release, nil, 0o600)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tempTree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary sibling remains after Capture: %v", err)
	}
}

func fakeWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, ".git", "index"), nil, 0o600)
	return workspace
}

func waitForFileContents(t *testing.T, path string, result <-chan error) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil && len(contents) != 0 {
			return string(contents)
		}
		select {
		case err := <-result:
			t.Fatalf("operation returned before observation: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

func waitForFile(t *testing.T, path string, result <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-result:
			t.Fatalf("operation returned before barrier: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
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
