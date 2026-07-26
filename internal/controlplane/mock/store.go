// Package mock provides a concurrency-safe in-memory control-plane store with
// deterministic failure injection for recovery tests.
package mock

import (
	"context"
	"sync"

	"github.com/araihu/paje/internal/controlplane"
)

type Config struct {
	CreateError error
	LoadError   error
	SaveError   error
}

type Store struct {
	mu          sync.Mutex
	records     map[string]controlplane.Snapshot
	createError error
	loadError   error
	saveError   error
	saveCount   int
	failAtSave  map[int]error
}

var _ controlplane.Store = (*Store)(nil)

func NewStore(config ...Config) *Store {
	store := &Store{
		records:    make(map[string]controlplane.Snapshot),
		failAtSave: make(map[int]error),
	}
	if len(config) > 0 {
		store.createError = config[0].CreateError
		store.loadError = config[0].LoadError
		store.saveError = config[0].SaveError
	}
	return store
}

func (s *Store) FailSave(number int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAtSave[number] = err
}

func (s *Store) FailNextSave(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAtSave[s.saveCount+1] = err
}

func (s *Store) Create(ctx context.Context, snapshot controlplane.Snapshot) (controlplane.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	if err := controlplane.ValidateSnapshot(snapshot); err != nil {
		return controlplane.Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createError != nil {
		return controlplane.Snapshot{}, s.createError
	}
	if _, exists := s.records[snapshot.Run.ID]; exists {
		return controlplane.Snapshot{}, controlplane.ErrAlreadyExists
	}
	s.records[snapshot.Run.ID] = controlplane.CloneSnapshot(snapshot)
	return controlplane.CloneSnapshot(snapshot), nil
}

func (s *Store) Load(ctx context.Context, id string) (controlplane.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadError != nil {
		return controlplane.Snapshot{}, s.loadError
	}
	snapshot, ok := s.records[id]
	if !ok {
		return controlplane.Snapshot{}, controlplane.ErrNotFound
	}
	return controlplane.CloneSnapshot(snapshot), nil
}

func (s *Store) Save(
	ctx context.Context,
	next controlplane.Snapshot,
	expectedVersion uint64,
) (controlplane.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCount++
	if err := s.failAtSave[s.saveCount]; err != nil {
		return controlplane.Snapshot{}, err
	}
	if s.saveError != nil {
		return controlplane.Snapshot{}, s.saveError
	}
	current, ok := s.records[next.Run.ID]
	if !ok {
		return controlplane.Snapshot{}, controlplane.ErrNotFound
	}
	if current.Version != expectedVersion {
		return controlplane.Snapshot{}, controlplane.ErrVersionConflict
	}
	next.Version = current.Version + 1
	if err := controlplane.ValidateSave(current, next); err != nil {
		return controlplane.Snapshot{}, err
	}
	s.records[next.Run.ID] = controlplane.CloneSnapshot(next)
	return controlplane.CloneSnapshot(next), nil
}

func (s *Store) EventsAfter(
	ctx context.Context,
	id string,
	after uint64,
	limit int,
) ([]controlplane.Event, uint64, error) {
	snapshot, err := s.Load(ctx, id)
	if err != nil {
		return nil, after, err
	}
	if limit <= 0 {
		limit = 100
	}
	events := make([]controlplane.Event, 0, min(limit, len(snapshot.Events)))
	cursor := after
	for _, event := range snapshot.Events {
		if event.Cursor <= after {
			continue
		}
		events = append(events, event)
		cursor = event.Cursor
		if len(events) == limit {
			break
		}
	}
	return events, cursor, nil
}
