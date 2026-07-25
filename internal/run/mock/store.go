// Package runmock provides a concurrency-safe in-memory run store.
package runmock

import (
	"context"
	"sync"

	"github.com/araihu/paje/internal/run"
)

type Config struct {
	ReserveError error
	LoadError    error
	SaveError    error
}

type Store struct {
	mu           sync.Mutex
	records      map[string]run.Record
	idempotency  map[string]binding
	reserveError error
	loadError    error
	saveError    error
}

type binding struct {
	runID     string
	inputHash string
}

var _ run.Store = (*Store)(nil)

func NewStore(config ...Config) *Store {
	store := &Store{
		records: make(map[string]run.Record), idempotency: make(map[string]binding),
	}
	if len(config) != 0 {
		store.reserveError = config[0].ReserveError
		store.loadError = config[0].LoadError
		store.saveError = config[0].SaveError
	}
	return store
}

func (s *Store) SetReserveError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveError = err
}

func (s *Store) SetLoadError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadError = err
}

func (s *Store) SetSaveError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveError = err
}

func (s *Store) Reserve(ctx context.Context, reservation run.Reservation) (run.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return run.Record{}, false, err
	}
	if err := run.ValidateReservation(reservation); err != nil {
		return run.Record{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return run.Record{}, false, err
	}
	if s.reserveError != nil {
		return run.Record{}, false, s.reserveError
	}

	key := ""
	if reservation.IdempotencyKey != "" {
		key = reservation.Template.String() + "\x00" + reservation.IdempotencyKey
		if existing, ok := s.idempotency[key]; ok {
			if existing.inputHash != reservation.InputHash {
				return run.Record{}, false, run.ErrIdempotencyConflict
			}
			record, ok := s.records[existing.runID]
			if !ok {
				return run.Record{}, false, run.ErrNotFound
			}
			return run.CloneRecord(record), false, nil
		}
	}
	if _, exists := s.records[reservation.NewRunID]; exists {
		return run.Record{}, false, run.ErrAlreadyExists
	}
	record, err := run.NewRecord(reservation)
	if err != nil {
		return run.Record{}, false, err
	}
	s.records[record.ID] = run.CloneRecord(record)
	if key != "" {
		s.idempotency[key] = binding{runID: record.ID, inputHash: record.InputHash}
	}
	return run.CloneRecord(record), true, nil
}

func (s *Store) Load(ctx context.Context, id string) (run.Record, error) {
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}
	if s.loadError != nil {
		return run.Record{}, s.loadError
	}
	record, ok := s.records[id]
	if !ok {
		return run.Record{}, run.ErrNotFound
	}
	return run.CloneRecord(record), nil
}

func (s *Store) Save(ctx context.Context, next run.Record, expected uint64) (run.Record, error) {
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}
	if s.saveError != nil {
		return run.Record{}, s.saveError
	}
	current, ok := s.records[next.ID]
	if !ok {
		return run.Record{}, run.ErrNotFound
	}
	if current.Version != expected {
		return run.Record{}, run.ErrVersionConflict
	}
	saved, err := run.PrepareSave(current, next)
	if err != nil {
		return run.Record{}, err
	}
	s.records[next.ID] = run.CloneRecord(saved)
	return run.CloneRecord(saved), nil
}
