// Package mock provides an in-memory implementation of the workspace port.
package mock

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	core "github.com/araihu/paje/internal/workspace"
)

// Preparation records one call to Manager.Prepare.
type Preparation struct {
	RepoURI       string
	Branch        string
	WorkspacePath string
	CleanupCount  int
}

// Manager returns logical, unique workspace paths without touching disk.
type Manager struct {
	mu       sync.Mutex
	root     string
	prepared []Preparation
}

var _ core.Manager = (*Manager)(nil)

// NewManager constructs a mock workspace manager.
func NewManager(root string) *Manager {
	if root == "" {
		root = "/mock-workspaces"
	}
	return &Manager{root: root}
}

// Prepare records the request and returns a logical workspace.
func (m *Manager) Prepare(
	ctx context.Context,
	repoURI string,
	branch string,
) (core.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	index := len(m.prepared)
	path := filepath.Join(m.root, fmt.Sprintf("workspace-%d", index+1))
	m.prepared = append(m.prepared, Preparation{
		RepoURI:       repoURI,
		Branch:        branch,
		WorkspacePath: path,
	})
	return &Workspace{manager: m, index: index, path: path}, nil
}

// Prepared returns a snapshot of all preparation records.
func (m *Manager) Prepared() []Preparation {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]Preparation, len(m.prepared))
	copy(result, m.prepared)
	return result
}

// Workspace is a logical workspace returned by Manager.
type Workspace struct {
	mu      sync.Mutex
	manager *Manager
	index   int
	path    string
	cleaned bool
}

var _ core.Workspace = (*Workspace)(nil)

// Path returns the logical workspace path.
func (w *Workspace) Path() string {
	return w.path
}

// Cleanup records cleanup once.
func (w *Workspace) Cleanup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cleaned {
		return nil
	}

	w.manager.mu.Lock()
	w.manager.prepared[w.index].CleanupCount++
	w.manager.mu.Unlock()
	w.cleaned = true
	return nil
}
