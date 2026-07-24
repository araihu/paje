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
	"regexp"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/workspace"
)

var (
	immutableSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	refPattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@-]*$`)
)

// Manager maintains cached repository mirrors and ephemeral worktrees.
type Manager struct {
	mu            sync.Mutex
	gitPath       string
	repositories  string
	worktreesRoot string
}

var _ workspace.Manager = (*Manager)(nil)
var _ repository.Resolver = (*Manager)(nil)

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

// Prepare creates an isolated detached worktree at an already-resolved SHA.
func (m *Manager) Prepare(
	ctx context.Context,
	repoURI string,
	sha string,
) (workspace.Workspace, error) {
	repoURI = strings.TrimSpace(repoURI)
	sha = strings.TrimSpace(sha)
	if !validRepositoryURI(repoURI) {
		return nil, fmt.Errorf("prepare git workspace: repository URI is required")
	}
	if !immutableSHAPattern.MatchString(sha) {
		return nil, fmt.Errorf("prepare git workspace: immutable SHA is required")
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
		"worktree", "add", "--detach", path, sha,
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

// Resolve refreshes the repository mirror under the manager lock and resolves
// a requested ref to a lowercase forty-character commit SHA.
func (m *Manager) Resolve(ctx context.Context, repoURI, ref string) (repository.Revision, error) {
	repoURI = strings.TrimSpace(repoURI)
	ref = strings.TrimSpace(ref)
	if !validRepositoryURI(repoURI) {
		return repository.Revision{}, fmt.Errorf("resolve git revision: repository URI is required")
	}
	if !validRef(ref) {
		return repository.Revision{}, fmt.Errorf("resolve git revision: invalid ref")
	}
	if err := ctx.Err(); err != nil {
		return repository.Revision{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	mirror := filepath.Join(m.repositories, repositoryKey(repoURI)+".git")
	if err := m.updateMirror(ctx, repoURI, mirror); err != nil {
		return repository.Revision{}, err
	}
	output, err := m.gitOutput(ctx, "--git-dir", mirror, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return repository.Revision{}, fmt.Errorf("resolve git revision: %w", err)
	}
	sha := strings.TrimSpace(string(output))
	if !immutableSHAPattern.MatchString(sha) {
		return repository.Revision{}, fmt.Errorf("resolve git revision: resolved SHA is not immutable")
	}
	dirty, err := m.localSourceDirty(ctx, repoURI)
	if err != nil {
		return repository.Revision{}, err
	}
	return repository.Revision{RepositoryURI: repoURI, Ref: ref, SHA: sha, SourceDirty: dirty}, nil
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
	_, err := m.gitOutput(ctx, args...)
	return err
}

func (m *Manager) gitOutput(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, m.gitPath, args...)
	output, err := command.CombinedOutput()
	if err == nil {
		return output, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	const diagnosticLimit = 4096
	if len(output) > diagnosticLimit {
		output = output[:diagnosticLimit]
	}
	return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
}

func (m *Manager) localSourceDirty(ctx context.Context, repoURI string) (bool, error) {
	path, ok := localPath(repoURI)
	if !ok {
		return false, nil
	}
	bare, err := m.gitOutput(ctx, "-C", path, "rev-parse", "--is-bare-repository")
	if err != nil {
		return false, fmt.Errorf("resolve git revision: inspect local source: %w", err)
	}
	if strings.TrimSpace(string(bare)) == "true" {
		return false, nil
	}
	output, err := m.gitOutput(ctx, "-C", path, "status", "--porcelain=v1", "-z")
	if err != nil {
		return false, fmt.Errorf("resolve git revision: inspect local source status: %w", err)
	}
	return len(output) != 0, nil
}

func localPath(repoURI string) (string, bool) {
	if strings.Contains(repoURI, "://") || strings.HasPrefix(repoURI, "git@") {
		return "", false
	}
	path, err := filepath.Abs(repoURI)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return path, true
}

func validRef(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.IndexByte(ref, 0) >= 0 || !refPattern.MatchString(ref) {
		return false
	}
	return !strings.Contains(ref, "..") && !strings.Contains(ref, "//") && !strings.HasSuffix(ref, ".") && !strings.HasSuffix(ref, "/")
}

func validRepositoryURI(repoURI string) bool {
	return repoURI != "" && !strings.HasPrefix(repoURI, "-") && !strings.ContainsAny(repoURI, "\x00\r\n")
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
