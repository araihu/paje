package gitworktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCleanupConcurrentRenameRebindPreservesReplacement(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "manager"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.worktreesRoot, "attempt")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(manager.worktreesRoot, "replacement")
	if err := os.Mkdir(replacement, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(replacement, "must-survive")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(manager.worktreesRoot, "displaced-original")
	workspace := &gitWorkspace{manager: manager, path: path, identity: identity}
	workspace.beforeCleanupExchange = func() {
		if err := os.Rename(path, displaced); err != nil {
			t.Errorf("displace original: %v", err)
			return
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Errorf("install replacement: %v", err)
		}
	}

	if err := workspace.Cleanup(context.Background()); err == nil {
		t.Fatal("Cleanup accepted concurrent directory rebind")
	}
	content, err := os.ReadFile(filepath.Join(path, "must-survive"))
	if err != nil || string(content) != "safe" {
		t.Fatalf("replacement content = %q, %v", content, err)
	}
	if _, err := os.Stat(displaced); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("displaced original inspection: %v", err)
	}
}

func TestCleanupRemovesNonemptyWorkspaceAndQuarantine(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "manager"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.worktreesRoot, "attempt")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "artifact"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	workspace := &gitWorkspace{manager: manager, path: path, identity: identity}

	if err := workspace.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(manager.worktreesRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for index := range entries {
			names[index] = entries[index].Name()
		}
		t.Fatalf("worktree entries = %v, want empty", names)
	}
}

func TestCleanupCancellationAfterExchangeCanRetryWithoutLeak(t *testing.T) {
	manager, workspace, sensitive := cleanupCancellationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	workspace.beforeCleanupExchange = cancel

	if err := workspace.Cleanup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Cleanup() = %v, want cancellation after exchange", err)
	}
	workspace.beforeCleanupExchange = nil
	if err := workspace.Cleanup(context.Background()); err != nil {
		t.Fatalf("retry Cleanup() = %v", err)
	}
	assertCleanupRoot(t, manager.worktreesRoot, nil)
	if _, err := os.Stat(sensitive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sensitive workspace material remains: %v", err)
	}
}

func TestCleanupCancellationRetryPreservesReplacementAndRemovesQuarantine(t *testing.T) {
	manager, workspace, sensitive := cleanupCancellationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	workspace.beforeCleanupExchange = cancel

	if err := workspace.Cleanup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Cleanup() = %v, want cancellation after exchange", err)
	}
	if err := os.Remove(workspace.path); err != nil {
		t.Fatalf("remove exchanged placeholder: %v", err)
	}
	if err := os.Mkdir(workspace.path, 0o750); err != nil {
		t.Fatalf("install replacement: %v", err)
	}
	replacementMarker := filepath.Join(workspace.path, "must-survive")
	if err := os.WriteFile(replacementMarker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace.beforeCleanupExchange = nil

	if err := workspace.Cleanup(context.Background()); err == nil {
		t.Fatal("retry Cleanup() accepted rebound replacement")
	}
	content, err := os.ReadFile(replacementMarker)
	if err != nil || string(content) != "safe" {
		t.Fatalf("replacement content = %q, %v", content, err)
	}
	assertCleanupRoot(t, manager.worktreesRoot, []string{"attempt"})
	if _, err := os.Stat(sensitive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sensitive workspace material remains: %v", err)
	}
	if err := workspace.Cleanup(context.Background()); err != nil {
		t.Fatalf("idempotent Cleanup() = %v", err)
	}
}

func cleanupCancellationFixture(t *testing.T) (*Manager, *gitWorkspace, string) {
	t.Helper()
	manager, err := New(filepath.Join(t.TempDir(), "manager"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.worktreesRoot, "attempt")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	sensitive := filepath.Join(path, "sensitive-material")
	if err := os.WriteFile(sensitive, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return manager, &gitWorkspace{manager: manager, path: path, identity: identity}, sensitive
}

func assertCleanupRoot(t *testing.T, root string, expected []string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for index := range entries {
		names[index] = entries[index].Name()
	}
	if !slices.Equal(names, expected) {
		t.Fatalf("cleanup root entries = %v, want %v", names, expected)
	}
}
