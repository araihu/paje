// Package gitworktree prepares isolated self-contained Git workspaces.
package gitworktree

import (
	"context"
	"crypto/rand"
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
	"golang.org/x/sys/unix"
)

var (
	immutableSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	refPattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@-]*$`)
)

// Manager maintains cached repository mirrors and ephemeral worktrees.
type Manager struct {
	mu            sync.Mutex
	gitPath       string
	root          string
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
	if err := os.MkdirAll(absoluteRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create git workspace manager: create root: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("create git workspace manager: canonicalize root: %w", err)
	}
	repositories, err := createManagedDirectory(canonicalRoot, "repositories")
	if err != nil {
		return nil, err
	}
	worktreesRoot, err := createManagedDirectory(canonicalRoot, "worktrees")
	if err != nil {
		return nil, err
	}
	return &Manager{
		gitPath:       gitPath,
		root:          canonicalRoot,
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
	if err := m.validateRoots(); err != nil {
		return nil, err
	}

	mirror := filepath.Join(m.repositories, repositoryKey(repoURI)+".git")
	if err := m.validateMirror(mirror); err != nil {
		return nil, err
	}
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
	if err := validateManagedPath(m.worktreesRoot, path, true); err != nil {
		return nil, fmt.Errorf("prepare git workspace: validate worktree path: %w", err)
	}

	if err := m.runGit(ctx, "clone", "--no-local", "--no-checkout", mirror, path); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("prepare git workspace: clone detached workspace: %w", err)
	}
	if err := m.runGit(ctx, "-C", path, "checkout", "--detach", sha); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("prepare git workspace: checkout detached revision: %w", err)
	}
	if err := m.runGit(ctx, "-C", path, "remote", "remove", "origin"); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("prepare git workspace: remove clone remote: %w", err)
	}
	if err := m.validateStandaloneWorkspace(ctx, path); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("prepare git workspace: validate standalone workspace: %w", err)
	}
	identity, err := os.Lstat(path)
	if err != nil || !identity.IsDir() || identity.Mode()&os.ModeSymlink != 0 {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("prepare git workspace: capture workspace identity: %w", err)
	}
	return &gitWorkspace{
		manager:  m,
		path:     path,
		identity: identity,
	}, nil
}

func (m *Manager) validateStandaloneWorkspace(ctx context.Context, workspacePath string) error {
	gitDirectory := filepath.Join(workspacePath, ".git")
	info, err := os.Lstat(gitDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("workspace Git metadata is not an in-workspace directory")
	}
	for _, args := range [][]string{
		{"-C", workspacePath, "rev-parse", "--absolute-git-dir"},
		{"-C", workspacePath, "rev-parse", "--path-format=absolute", "--git-common-dir"},
		{"-C", workspacePath, "rev-parse", "--path-format=absolute", "--git-path", "objects"},
	} {
		output, err := m.gitOutput(ctx, args...)
		if err != nil {
			return err
		}
		resolved := strings.TrimSpace(string(output))
		if resolved == "" {
			return errors.New("workspace Git path is empty")
		}
		canonical, err := filepath.EvalSymlinks(resolved)
		if err != nil {
			return err
		}
		if err := pathWithin(workspacePath, canonical); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(filepath.Join(gitDirectory, "objects", "info", "alternates")); err == nil {
		return errors.New("workspace Git object alternates are forbidden")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	remotes, err := m.gitOutput(ctx, "-C", workspacePath, "remote")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(remotes)) != "" {
		return errors.New("workspace retains a live Git remote")
	}
	command := exec.CommandContext(ctx, m.gitPath, "-C", workspacePath, "config", "--local", "--get-regexp", `^credential\.`)
	output, err := command.CombinedOutput()
	if err == nil || len(output) != 0 {
		return errors.New("workspace retains local credential configuration")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return err
	}
	return nil
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
	if err := m.validateRoots(); err != nil {
		return repository.Revision{}, err
	}

	mirror := filepath.Join(m.repositories, repositoryKey(repoURI)+".git")
	if err := m.validateMirror(mirror); err != nil {
		return repository.Revision{}, err
	}
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
	if err := m.validateMirror(mirror); err != nil {
		return err
	}
	info, err := os.Lstat(mirror)
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
		if validateErr := m.validateMirror(mirror); validateErr == nil {
			_ = os.RemoveAll(mirror)
		}
		return fmt.Errorf("prepare git workspace: clone mirror: %w", err)
	}
	return nil
}

func (m *Manager) validateRoots() error {
	if err := validateManagedDirectory(m.root, m.repositories); err != nil {
		return fmt.Errorf("validate git workspace repositories root: %w", err)
	}
	if err := validateManagedDirectory(m.root, m.worktreesRoot); err != nil {
		return fmt.Errorf("validate git workspace worktrees root: %w", err)
	}
	return nil
}

func (m *Manager) validateMirror(mirror string) error {
	if err := validateManagedPath(m.repositories, mirror, true); err != nil {
		return fmt.Errorf("validate git workspace mirror: %w", err)
	}
	return nil
}

func createManagedDirectory(root, name string) (string, error) {
	directory := filepath.Join(root, name)
	if err := os.Mkdir(directory, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create git workspace manager: create %q: %w", directory, err)
	}
	if err := validateManagedDirectory(root, directory); err != nil {
		return "", fmt.Errorf("create git workspace manager: %w", err)
	}
	return filepath.EvalSymlinks(directory)
}

func validateManagedDirectory(root, directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("must be a non-symlink directory")
	}
	return validateManagedPath(root, directory, false)
}

func validateManagedPath(root, path string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(path)
			resolvedParent, parentErr := filepath.EvalSymlinks(parent)
			if parentErr != nil {
				return parentErr
			}
			return pathWithin(root, resolvedParent)
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("must not be a symlink")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	return pathWithin(root, resolved)
}

func pathWithin(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes workspace root")
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
	mu                    sync.Mutex
	manager               *Manager
	path                  string
	identity              os.FileInfo
	cleaned               bool
	cleanup               *cleanupExchangeState
	beforeCleanupExchange func()
}

type cleanupExchangeState struct {
	quarantineName      string
	quarantineIdentity  os.FileInfo
	placeholderIdentity os.FileInfo
	quarantineRemoved   bool
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
	if err := w.manager.validateRoots(); err != nil {
		return fmt.Errorf("cleanup git workspace: %w", err)
	}
	if err := validateManagedPath(w.manager.worktreesRoot, w.path, true); err != nil {
		return fmt.Errorf("cleanup git workspace: validate worktree path: %w", err)
	}

	parentPath := filepath.Dir(w.path)
	if parentPath != w.manager.worktreesRoot {
		return errors.New("cleanup git workspace: workspace is not a direct managed child")
	}
	parentDescriptor, err := unix.Open(
		parentPath,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("cleanup git workspace: open managed parent: %w", err)
	}
	parent := os.NewFile(uintptr(parentDescriptor), "git-workspace-parent")
	if parent == nil {
		_ = unix.Close(parentDescriptor)
		return errors.New("cleanup git workspace: open managed parent")
	}
	defer parent.Close()
	parentIdentity, err := parent.Stat()
	if err != nil {
		return fmt.Errorf("cleanup git workspace: inspect managed parent: %w", err)
	}
	currentParent, err := os.Lstat(parentPath)
	if err != nil || !os.SameFile(parentIdentity, currentParent) {
		return errors.New("cleanup git workspace: managed parent identity was rebound")
	}

	targetName := filepath.Base(w.path)
	if w.cleanup != nil {
		return w.finishCleanupExchange(ctx, parentDescriptor, targetName)
	}
	info, err := statDirectoryAt(parentDescriptor, targetName)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("cleanup git workspace: workspace identity is missing")
	} else if err != nil {
		return fmt.Errorf("cleanup git workspace: inspect path: %w", err)
	}
	if w.identity == nil || !os.SameFile(w.identity, info) {
		return errors.New("cleanup git workspace: workspace identity was rebound")
	}
	quarantineName, err := createCleanupQuarantine(parentDescriptor)
	if err != nil {
		return fmt.Errorf("cleanup git workspace: create quarantine: %w", err)
	}
	placeholder, err := statDirectoryAt(parentDescriptor, quarantineName)
	if err != nil {
		_ = unix.Unlinkat(parentDescriptor, quarantineName, unix.AT_REMOVEDIR)
		return fmt.Errorf("cleanup git workspace: bind quarantine placeholder: %w", err)
	}
	if w.beforeCleanupExchange != nil {
		w.beforeCleanupExchange()
	}
	if err := exchangeDirectoryEntries(parentDescriptor, targetName, quarantineName); err != nil {
		_ = unix.Unlinkat(parentDescriptor, quarantineName, unix.AT_REMOVEDIR)
		return fmt.Errorf("cleanup git workspace: exchange with quarantine: %w", err)
	}
	quarantineDescriptor, err := unix.Openat(
		parentDescriptor,
		quarantineName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		restoreErr := rollbackCleanupExchange(parentDescriptor, targetName, quarantineName, info, placeholder)
		return errors.Join(fmt.Errorf("cleanup git workspace: open quarantined workspace: %w", err), restoreErr)
	}
	quarantine := os.NewFile(uintptr(quarantineDescriptor), "git-workspace-quarantine")
	if quarantine == nil {
		_ = unix.Close(quarantineDescriptor)
		restoreErr := rollbackCleanupExchange(parentDescriptor, targetName, quarantineName, info, placeholder)
		return errors.Join(errors.New("cleanup git workspace: open quarantined workspace"), restoreErr)
	}
	moved, err := quarantine.Stat()
	if err != nil || !os.SameFile(w.identity, moved) {
		_ = quarantine.Close()
		expectedMoved := info
		if moved != nil {
			expectedMoved = moved
		}
		restoreErr := rollbackCleanupExchange(parentDescriptor, targetName, quarantineName, expectedMoved, placeholder)
		return errors.Join(
			errors.New("cleanup git workspace: exchanged workspace identity was rebound"),
			err,
			restoreErr,
		)
	}
	if err := quarantine.Close(); err != nil {
		restoreErr := rollbackCleanupExchange(parentDescriptor, targetName, quarantineName, moved, placeholder)
		return errors.Join(fmt.Errorf("cleanup git workspace: close quarantined workspace: %w", err), restoreErr)
	}
	w.cleanup = &cleanupExchangeState{
		quarantineName:      quarantineName,
		quarantineIdentity:  moved,
		placeholderIdentity: placeholder,
	}
	currentTarget, targetErr := statDirectoryAt(parentDescriptor, targetName)
	if targetErr != nil || !os.SameFile(placeholder, currentTarget) {
		return errors.Join(errors.New("cleanup git workspace: exchanged placeholder identity was rebound"), targetErr)
	}
	return w.finishCleanupExchange(ctx, parentDescriptor, targetName)
}

func (w *gitWorkspace) finishCleanupExchange(ctx context.Context, parentDescriptor int, targetName string) error {
	state := w.cleanup
	if state == nil {
		return errors.New("cleanup git workspace: cleanup exchange state is missing")
	}
	if !state.quarantineRemoved {
		quarantineDescriptor, err := unix.Openat(
			parentDescriptor,
			state.quarantineName,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return fmt.Errorf("cleanup git workspace: reopen quarantined workspace: %w", err)
		}
		quarantine := os.NewFile(uintptr(quarantineDescriptor), "git-workspace-quarantine")
		if quarantine == nil {
			_ = unix.Close(quarantineDescriptor)
			return errors.New("cleanup git workspace: reopen quarantined workspace")
		}
		current, err := quarantine.Stat()
		if err != nil || !os.SameFile(state.quarantineIdentity, current) {
			_ = quarantine.Close()
			return errors.Join(errors.New("cleanup git workspace: quarantined workspace identity was rebound"), err)
		}
		if err := removeDirectoryContents(ctx, quarantine); err != nil {
			_ = quarantine.Close()
			return fmt.Errorf("cleanup git workspace: remove quarantined workspace contents: %w", err)
		}
		current, err = statDirectoryAt(parentDescriptor, state.quarantineName)
		if err != nil || !os.SameFile(state.quarantineIdentity, current) {
			_ = quarantine.Close()
			return errors.Join(errors.New("cleanup git workspace: quarantined workspace identity was rebound"), err)
		}
		if err := quarantine.Close(); err != nil {
			return fmt.Errorf("cleanup git workspace: close quarantined workspace: %w", err)
		}
		if err := unix.Unlinkat(parentDescriptor, state.quarantineName, unix.AT_REMOVEDIR); err != nil {
			return fmt.Errorf("cleanup git workspace: remove quarantine: %w", err)
		}
		state.quarantineRemoved = true
	}

	currentTarget, err := statDirectoryAt(parentDescriptor, targetName)
	switch {
	case errors.Is(err, os.ErrNotExist):
		w.cleanup = nil
		w.cleaned = true
		return nil
	case err != nil:
		w.cleanup = nil
		w.cleaned = true
		return errors.Join(errors.New("cleanup git workspace: exchanged placeholder identity was rebound"), err)
	case !os.SameFile(state.placeholderIdentity, currentTarget):
		w.cleanup = nil
		w.cleaned = true
		return errors.New("cleanup git workspace: exchanged placeholder identity was rebound")
	}
	if err := unix.Unlinkat(parentDescriptor, targetName, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("cleanup git workspace: remove exchanged placeholder: %w", err)
	}
	w.cleanup = nil
	w.cleaned = true
	return nil
}

func rollbackCleanupExchange(
	parentDescriptor int,
	targetName string,
	quarantineName string,
	movedIdentity os.FileInfo,
	placeholderIdentity os.FileInfo,
) error {
	currentMoved, movedErr := statDirectoryAt(parentDescriptor, quarantineName)
	currentPlaceholder, placeholderErr := statDirectoryAt(parentDescriptor, targetName)
	if movedErr != nil || placeholderErr != nil || movedIdentity == nil || placeholderIdentity == nil ||
		!os.SameFile(movedIdentity, currentMoved) || !os.SameFile(placeholderIdentity, currentPlaceholder) {
		return errors.Join(errors.New("cleanup git workspace: cannot safely roll back cleanup exchange"), movedErr, placeholderErr)
	}
	if err := exchangeDirectoryEntries(parentDescriptor, targetName, quarantineName); err != nil {
		return fmt.Errorf("cleanup git workspace: roll back cleanup exchange: %w", err)
	}
	restored, restoredErr := statDirectoryAt(parentDescriptor, targetName)
	placeholder, quarantineErr := statDirectoryAt(parentDescriptor, quarantineName)
	if restoredErr != nil || quarantineErr != nil || !os.SameFile(movedIdentity, restored) ||
		!os.SameFile(placeholderIdentity, placeholder) {
		reverseErr := exchangeDirectoryEntries(parentDescriptor, targetName, quarantineName)
		return errors.Join(
			errors.New("cleanup git workspace: cleanup exchange rollback identity mismatch"),
			restoredErr,
			quarantineErr,
			reverseErr,
		)
	}
	if err := unix.Unlinkat(parentDescriptor, quarantineName, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("cleanup git workspace: remove rolled-back placeholder: %w", err)
	}
	return nil
}

func removeDirectoryContents(ctx context.Context, directory *os.File) error {
	descriptor, err := unix.Dup(int(directory.Fd()))
	if err != nil {
		return err
	}
	reader := os.NewFile(uintptr(descriptor), "git-workspace-cleanup-reader")
	if reader == nil {
		_ = unix.Close(descriptor)
		return errors.New("open workspace cleanup reader")
	}
	entries, readErr := reader.ReadDir(-1)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		var before unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if before.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
				return err
			}
			continue
		}
		childDescriptor, err := unix.Openat(
			int(directory.Fd()),
			name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return err
		}
		child := os.NewFile(uintptr(childDescriptor), "git-workspace-cleanup-child")
		if child == nil {
			_ = unix.Close(childDescriptor)
			return errors.New("open workspace cleanup child")
		}
		var opened unix.Stat_t
		if err := unix.Fstat(childDescriptor, &opened); err != nil || opened.Dev != before.Dev || opened.Ino != before.Ino {
			_ = child.Close()
			return errors.Join(errors.New("workspace cleanup child identity was rebound"), err)
		}
		if err := removeDirectoryContents(ctx, child); err != nil {
			_ = child.Close()
			return err
		}
		var current unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || current.Dev != opened.Dev || current.Ino != opened.Ino {
			_ = child.Close()
			return errors.Join(errors.New("workspace cleanup child identity was rebound"), err)
		}
		if err := child.Close(); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(directory.Fd()), name, unix.AT_REMOVEDIR); err != nil {
			return err
		}
	}
	return nil
}

func statDirectoryAt(parentDescriptor int, name string) (os.FileInfo, error) {
	descriptor, err := unix.Openat(
		parentDescriptor,
		name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), "git-workspace-entry")
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New("open workspace entry")
	}
	defer file.Close()
	return file.Stat()
}

func createCleanupQuarantine(parentDescriptor int) (string, error) {
	for range 16 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		name := ".paje-cleanup-" + hex.EncodeToString(random[:])
		if err := unix.Mkdirat(parentDescriptor, name, 0o700); err == nil {
			return name, nil
		} else if !errors.Is(err, unix.EEXIST) {
			return "", err
		}
	}
	return "", errors.New("create unique cleanup quarantine")
}
