package workspace

import "context"

// Workspace is an isolated environment prepared for one agent execution.
type Workspace interface {
	Path() string
	Cleanup(ctx context.Context) error
}

// Manager prepares isolated workspaces from repositories.
type Manager interface {
	Prepare(ctx context.Context, repoURI string, branch string) (Workspace, error)
}
