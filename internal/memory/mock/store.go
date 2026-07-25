// Package mock provides an in-memory implementation of the memory port.
package mock

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/memory"
)

// Store is a concurrency-safe, in-memory memory store.
type Store struct {
	mu       sync.RWMutex
	memories []memory.Memory
	nextID   int
}

var _ memory.Store = (*Store)(nil)

// NewStore constructs a Store seeded with defensive copies of initial.
func NewStore(initial []memory.Memory) *Store {
	memories := cloneMemories(initial)
	return &Store{
		memories: memories,
		nextID:   len(memories) + 1,
	}
}

// Search returns memories whose content contains query and whose metadata
// contains every requested tag.
func (s *Store) Search(
	ctx context.Context,
	query string,
	limit int,
	tags map[string]string,
) ([]memory.Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 0 {
		return nil, fmt.Errorf("search memory: limit must be non-negative")
	}
	if limit == 0 {
		return []memory.Memory{}, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	normalizedQuery := strings.ToLower(query)
	result := make([]memory.Memory, 0, min(limit, len(s.memories)))
	for _, candidate := range s.memories {
		if !strings.Contains(strings.ToLower(candidate.Content), normalizedQuery) {
			continue
		}
		if !containsTags(candidate.Metadata, tags) {
			continue
		}
		result = append(result, cloneMemory(candidate))
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

// Save appends a memory with a deterministic mock identifier.
func (s *Store) Save(ctx context.Context, content string, tags map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.memories = append(s.memories, memory.Memory{
		ID:       fmt.Sprintf("mock-%d", s.nextID),
		Content:  content,
		Metadata: cloneMap(tags),
	})
	s.nextID++
	return nil
}

// Memories returns a defensive snapshot of all stored memories.
func (s *Store) Memories() []memory.Memory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMemories(s.memories)
}

func containsTags(metadata, tags map[string]string) bool {
	for key, want := range tags {
		if metadata[key] != want {
			return false
		}
	}
	return true
}

func cloneMemories(source []memory.Memory) []memory.Memory {
	if source == nil {
		return nil
	}
	result := make([]memory.Memory, len(source))
	for i, item := range source {
		result[i] = cloneMemory(item)
	}
	return result
}

func cloneMemory(source memory.Memory) memory.Memory {
	source.Metadata = cloneMap(source.Metadata)
	return source
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
