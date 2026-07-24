package gitcapture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
