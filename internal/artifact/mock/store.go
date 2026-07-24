// Package artifactmock provides a concurrency-safe in-memory artifact store.
package artifactmock

import (
	"context"
	"sync"

	"github.com/araihu/paje/internal/artifact"
)

// Config configures deterministic mock failures.
type Config struct {
	SaveError error
	LoadError error
}

// Snapshot is an independent view of calls and bundles retained by Store.
type Snapshot struct {
	Saves   []artifact.Reference
	Loads   []artifact.Reference
	Bundles map[string]artifact.Bundle
}

// Store is a test double sharing the production canonical reference encoder.
type Store struct {
	mu      sync.Mutex
	bundles map[string]artifact.Bundle
	saves   []artifact.Reference
	loads   []artifact.Reference
	saveErr error
	loadErr error
}

var _ artifact.Store = (*Store)(nil)

// NewStore creates a mock store. An optional Config supplies deterministic errors.
func NewStore(config ...Config) *Store {
	store := &Store{bundles: make(map[string]artifact.Bundle)}
	if len(config) != 0 {
		store.saveErr = config[0].SaveError
		store.loadErr = config[0].LoadError
	}
	return store
}
func (s *Store) SetSaveError(err error) { s.mu.Lock(); defer s.mu.Unlock(); s.saveErr = err }
func (s *Store) SetLoadError(err error) { s.mu.Lock(); defer s.mu.Unlock(); s.loadErr = err }
func (s *Store) Save(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return artifact.Reference{}, s.saveErr
	}
	normalized, _, ref, err := artifact.Canonicalize(bundle)
	if err != nil {
		return artifact.Reference{}, err
	}
	s.bundles[ref.Digest] = artifact.CloneBundle(normalized)
	s.saves = append(s.saves, ref)
	return ref, nil
}
func (s *Store) Load(ctx context.Context, ref artifact.Reference) (artifact.Bundle, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Bundle{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return artifact.Bundle{}, s.loadErr
	}
	s.loads = append(s.loads, ref)
	bundle, ok := s.bundles[ref.Digest]
	if !ok || bundle.Manifest.RunID != ref.RunID {
		return artifact.Bundle{}, artifact.ErrDigestMismatch
	}
	expected, err := artifact.ReferenceFor(bundle)
	if err != nil || expected != ref {
		return artifact.Bundle{}, artifact.ErrDigestMismatch
	}
	return artifact.CloneBundle(bundle), nil
}
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := Snapshot{Saves: append([]artifact.Reference(nil), s.saves...), Loads: append([]artifact.Reference(nil), s.loads...), Bundles: make(map[string]artifact.Bundle, len(s.bundles))}
	for digest, bundle := range s.bundles {
		snapshot.Bundles[digest] = artifact.CloneBundle(bundle)
	}
	return snapshot
}
