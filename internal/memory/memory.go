package memory

import "context"

// Memory is a piece of historical context returned by a Store.
type Memory struct {
	ID       string
	Content  string
	Metadata map[string]string
}

// Store persists and retrieves agent context without exposing provider details.
type Store interface {
	Search(ctx context.Context, query string, limit int, tags map[string]string) ([]Memory, error)
	Save(ctx context.Context, content string, tags map[string]string) error
}
