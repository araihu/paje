// Package gitworktree prepares isolated agent workspaces using Git worktrees.
package gitworktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/workspace"
)

// Manager maintains cached repository mirrors and ephemeral worktrees.
type Manager struct {
	mu            sync.Mutex
	gitPath       string
	repositories  string
	worktreesRoot string
}

var _ workspace.Manager = (*Manager)(nil)

// New constructs a Git worktree manager rooted at root.
func New(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("create git workspace manager: root is required")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("create git workspace manager: find git: %w", err)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("create git workspace manager: resolve root: %w", err)
	}
	repositories := filepath.Join(absoluteRoot, "repositories")
	worktreesRoot := filepath.Join(absoluteRoot, "worktrees")
	for _, directory := range []string{repositories, worktreesRoot} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("create git workspace manager: create %q: %w", directory, err)
		}
	}
	return &Manager{
		gitPath:       gitPath,
		repositories:  repositories,
		worktreesRoot: worktreesRoot,
	}, nil
}

// Prepare creates an isolated detached worktree at branch.
func (m *Manager) Prepare(
	ctx context.Context,
	repoURI string,
	branch string,
) (workspace.Workspace, error) {
	repoURI = strings.TrimSpace(repoURI)
	branch = strings.TrimSpace(branch)
	if repoURI == "" {
		return nil, fmt.Errorf("prepare git workspace: repository URI is required")
	}
	if branch == "" {
		return nil, fmt.Errorf("prepare git workspace: branch is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	mirror := filepath.Join(m.repositories, repositoryKey(repoURI)+".git")
	if err := m.updateMirror(ctx, repoURI, mirror); err != nil {
		return nil, err
	}

	path, err := os.MkdirTemp(m.worktreesRoot, "paje-")
	if err != nil {
		return nil, fmt.Errorf("prepare git workspace: allocate worktree path: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("prepare git workspace: release worktree path: %w", err)
	}

	if err := m.runGit(
		ctx,
		"--git-dir", mirror,
		"worktree", "add", "--detach", path, branch,
	); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("prepare git workspace: add worktree: %w", err)
	}
	return &gitWorkspace{
		manager: m,
		mirror:  mirror,
		path:    path,
	}, nil
}

func (m *Manager) updateMirror(ctx context.Context, repoURI, mirror string) error {
	info, err := os.Stat(mirror)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("prepare git workspace: mirror path %q is not a directory", mirror)
		}
		if err := m.runGit(ctx, "--git-dir", mirror, "fetch", "--prune", "origin"); err != nil {
			return fmt.Errorf("prepare git workspace: refresh mirror: %w", err)
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("prepare git workspace: inspect mirror: %w", err)
	}

	if err := m.runGit(ctx, "clone", "--mirror", repoURI, mirror); err != nil {
		_ = os.RemoveAll(mirror)
		return fmt.Errorf("prepare git workspace: clone mirror: %w", err)
	}
	return nil
}

func (m *Manager) runGit(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, m.gitPath, args...)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	const diagnosticLimit = 4096
	if len(output) > diagnosticLimit {
		output = output[:diagnosticLimit]
	}
	return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
}

func repositoryKey(repoURI string) string {
	sum := sha256.Sum256([]byte(repoURI))
	return hex.EncodeToString(sum[:])
}

type gitWorkspace struct {
	mu      sync.Mutex
	manager *Manager
	mirror  string
	path    string
	cleaned bool
}

var _ workspace.Workspace = (*gitWorkspace)(nil)

func (w *gitWorkspace) Path() string {
	return w.path
}

func (w *gitWorkspace) Cleanup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cleaned {
		return nil
	}

	w.manager.mu.Lock()
	defer w.manager.mu.Unlock()

	if _, err := os.Stat(w.path); errors.Is(err, os.ErrNotExist) {
		if pruneErr := w.manager.runGit(ctx, "--git-dir", w.mirror, "worktree", "prune"); pruneErr != nil {
			return fmt.Errorf("cleanup git workspace: prune missing worktree: %w", pruneErr)
		}
		w.cleaned = true
		return nil
	} else if err != nil {
		return fmt.Errorf("cleanup git workspace: inspect path: %w", err)
	}

	if err := w.manager.runGit(
		ctx,
		"--git-dir", w.mirror,
		"worktree", "remove", "--force", w.path,
	); err != nil {
		return fmt.Errorf("cleanup git workspace: %w", err)
	}
	w.cleaned = true
	return nil
}
