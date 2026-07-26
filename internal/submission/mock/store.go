// Package mock provides deterministic submission-domain test doubles.
package mock

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/araihu/paje/internal/submission"
)

// Store is a concurrency-safe in-memory implementation of submission.Store.
type Store struct {
	mu             sync.Mutex
	records        map[string]submission.Record
	keys           map[string]string
	bindTriggerErr error
}

// NewStore returns an empty deterministic store.
func NewStore() *Store {
	return &Store{
		records: make(map[string]submission.Record),
		keys:    make(map[string]string),
	}
}

func (s *Store) Reserve(
	ctx context.Context,
	reservation submission.Reservation,
) (submission.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := reservation.Record.CredentialID + "\x00" + reservation.IdempotencyKey
	if runID, exists := s.keys[key]; exists {
		current := s.records[runID]
		if !sameBinding(current, reservation.Record) {
			return cloneRecord(current), false, submission.ErrIdempotencyConflict
		}
		return cloneRecord(current), false, nil
	}
	if _, exists := s.records[reservation.Record.RunID]; exists {
		return submission.Record{}, false, submission.ErrIdempotencyConflict
	}
	record := cloneRecord(reservation.Record)
	s.records[record.RunID] = record
	s.keys[key] = record.RunID
	return cloneRecord(record), true, nil
}

func (s *Store) BindTrigger(
	ctx context.Context,
	runID string,
	reference submission.TriggerReference,
) (submission.Record, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindTriggerErr != nil {
		return submission.Record{}, s.bindTriggerErr
	}
	record, exists := s.records[runID]
	if !exists {
		return submission.Record{}, submission.ErrNotFound
	}
	if record.Trigger != nil {
		if *record.Trigger != reference {
			return cloneRecord(record), submission.ErrIdempotencyConflict
		}
		return cloneRecord(record), nil
	}
	record.Trigger = &submission.TriggerReference{
		Provider:      reference.Provider,
		ExternalRunID: reference.ExternalRunID,
	}
	s.records[runID] = cloneRecord(record)
	return cloneRecord(record), nil
}

func (s *Store) Load(ctx context.Context, runID string) (submission.Record, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[runID]
	if !exists {
		return submission.Record{}, submission.ErrNotFound
	}
	return cloneRecord(record), nil
}

func (s *Store) LoadByKey(
	ctx context.Context,
	credentialID string,
	key string,
) (submission.Record, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runID, exists := s.keys[credentialID+"\x00"+key]
	if !exists {
		return submission.Record{}, submission.ErrNotFound
	}
	return cloneRecord(s.records[runID]), nil
}

func (s *Store) MarkCancellationRequested(
	ctx context.Context,
	runID string,
	at time.Time,
) (submission.Record, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[runID]
	if !exists {
		return submission.Record{}, submission.ErrNotFound
	}
	if record.CancellationRequested == nil {
		value := at
		record.CancellationRequested = &value
		record.UpdatedAt = at
		s.records[runID] = cloneRecord(record)
	}
	return cloneRecord(record), nil
}

// SetRecord replaces one stored record without changing its key index. It is
// intended for corruption and recovery fixtures.
func (s *Store) SetRecord(record submission.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[record.RunID] = cloneRecord(record)
}

// SetBindTriggerError configures the durable binding failure fixture.
func (s *Store) SetBindTriggerError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindTriggerErr = err
}

func sameBinding(left, right submission.Record) bool {
	return left.RunID == right.RunID &&
		left.CredentialID == right.CredentialID &&
		left.RequestDigest == right.RequestDigest &&
		left.IdempotencyKeyDigest == right.IdempotencyKeyDigest &&
		left.Template == right.Template &&
		bytes.Equal(left.CanonicalInput, right.CanonicalInput) &&
		left.Origin == right.Origin &&
		left.RootRunID == right.RootRunID &&
		left.Depth == right.Depth
}

func cloneRecord(source submission.Record) submission.Record {
	cloned := source
	cloned.CanonicalInput = append([]byte(nil), source.CanonicalInput...)
	if source.Trigger != nil {
		value := *source.Trigger
		cloned.Trigger = &value
	}
	if source.CancellationRequested != nil {
		value := *source.CancellationRequested
		cloned.CancellationRequested = &value
	}
	return cloned
}

var _ submission.Store = (*Store)(nil)
