// Package mock provides a concurrency-safe in-memory control-plane store with
// deterministic failure injection for recovery tests.
package mock

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/araihu/paje/internal/controlplane"
	"github.com/araihu/paje/internal/controlplane/journal"
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
	journal     *journal.MemoryStore
}

var _ controlplane.Store = (*Store)(nil)
var installationCounter atomic.Uint64

func NewStore(config ...Config) *Store {
	installationID := fmt.Sprintf("mock-installation-%d", installationCounter.Add(1))
	journalStore, err := journal.NewMemoryStore(installationID)
	if err != nil {
		panic(err)
	}
	store := &Store{
		records:    make(map[string]controlplane.Snapshot),
		failAtSave: make(map[int]error),
		journal:    journalStore,
	}
	if len(config) > 0 {
		store.createError = config[0].CreateError
		store.loadError = config[0].LoadError
		store.saveError = config[0].SaveError
	}
	return store
}

func (s *Store) InstallationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.InstallationID()
}

func (s *Store) Reserve(ctx context.Context, action journal.Action) (journal.Action, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.Reserve(ctx, action)
}

func (s *Store) Reservation(
	ctx context.Context,
	controlRunID, actionID string,
) (journal.Action, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.Reservation(ctx, controlRunID, actionID)
}

func (s *Store) Append(ctx context.Context, controlRunID string, expected uint64, event journal.Event) (journal.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.Append(ctx, controlRunID, expected, event)
}

func (s *Store) RunEvents(ctx context.Context, cursor journal.RunCursor, limit int) ([]journal.Event, journal.RunCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.RunEvents(ctx, cursor, limit)
}

func (s *Store) Feed(ctx context.Context, cursor journal.GlobalCursor, limit int) ([]journal.Event, journal.GlobalCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.Feed(ctx, cursor, limit)
}

func (s *Store) Checkpoint(ctx context.Context, run journal.RunCursor, global journal.GlobalCursor, projection []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.Checkpoint(ctx, run, global, projection)
}

func (s *Store) LoadCheckpoint(ctx context.Context, controlRunID string) ([]byte, journal.RunCursor, journal.GlobalCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.LoadCheckpoint(ctx, controlRunID)
}

func (s *Store) ActiveRuns(ctx context.Context, cursor string, limit int) ([]string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.ActiveRuns(ctx, cursor, limit)
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
	if err := s.appendProjectionLocked(ctx, controlplane.Snapshot{}, snapshot, true); err != nil {
		delete(s.records, snapshot.Run.ID)
		return controlplane.Snapshot{}, err
	}
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

func (s *Store) ReserveAction(
	ctx context.Context,
	next controlplane.Snapshot,
	expectedVersion uint64,
	action journal.Action,
) (controlplane.Snapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCount++
	if err := s.failAtSave[s.saveCount]; err != nil {
		return controlplane.Snapshot{}, false, err
	}
	if s.saveError != nil {
		return controlplane.Snapshot{}, false, s.saveError
	}
	current, ok := s.records[next.Run.ID]
	if !ok {
		return controlplane.Snapshot{}, false, controlplane.ErrNotFound
	}
	next.Version = expectedVersion + 1
	if current.Version != expectedVersion {
		if current.Version == next.Version && reflect.DeepEqual(current, next) {
			if err := s.journal.ValidateExactReservation(ctx, action); err != nil {
				return controlplane.Snapshot{}, false, err
			}
			return controlplane.CloneSnapshot(current), false, nil
		}
		return controlplane.Snapshot{}, false, controlplane.ErrVersionConflict
	}
	if err := controlplane.ValidateActionReservation(current, next, action); err != nil {
		return controlplane.Snapshot{}, false, err
	}
	payloadDigest, err := journal.Digest(next)
	if err != nil {
		return controlplane.Snapshot{}, false, err
	}
	occurred := next.Run.UpdatedAt
	if occurred.IsZero() {
		occurred = time.Unix(0, 0).UTC()
	}
	_, created, _, err := s.journal.ReserveAndAppend(ctx, action, journal.Event{
		ID:           fmt.Sprintf("snapshot_%s_%020d", next.Run.ID, next.Version),
		ControlRunID: next.Run.ID, Kind: journal.EventProjectionUpdated,
		PayloadDigest: payloadDigest, OccurredAt: occurred,
	})
	if err != nil {
		return controlplane.Snapshot{}, false, err
	}
	s.records[next.Run.ID] = controlplane.CloneSnapshot(next)
	return controlplane.CloneSnapshot(next), created, nil
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
	_, actionID := outcomeBinding(current, next)
	if actionID != "" {
		reservation, err := s.journal.Reservation(ctx, next.Run.ID, actionID)
		if err != nil {
			return controlplane.Snapshot{}, err
		}
		if err := controlplane.ValidateOutcomeReservation(next, expectedVersion, actionID, reservation); err != nil {
			return controlplane.Snapshot{}, err
		}
	}
	s.saveCount++
	if err := s.failAtSave[s.saveCount]; err != nil {
		return controlplane.Snapshot{}, err
	}
	if s.saveError != nil {
		return controlplane.Snapshot{}, s.saveError
	}
	if err := s.appendProjectionLocked(ctx, current, next, false); err != nil {
		return controlplane.Snapshot{}, err
	}
	s.records[next.Run.ID] = controlplane.CloneSnapshot(next)
	return controlplane.CloneSnapshot(next), nil
}

func (s *Store) appendProjectionLocked(
	ctx context.Context,
	current, next controlplane.Snapshot,
	create bool,
) error {
	kind := journal.EventProjectionUpdated
	actionID := ""
	if !create {
		kind, actionID = outcomeBinding(current, next)
	}
	payloadDigest, err := journal.Digest(next)
	if err != nil {
		return err
	}
	occurred := next.Run.UpdatedAt
	if occurred.IsZero() {
		occurred = time.Unix(0, 0).UTC()
	}
	_, err = s.journal.Append(ctx, next.Run.ID, s.journal.RunHead(next.Run.ID), journal.Event{
		ID:           fmt.Sprintf("snapshot_%s_%020d", next.Run.ID, next.Version),
		ControlRunID: next.Run.ID, ActionID: actionID, Kind: kind,
		PayloadDigest: payloadDigest, OccurredAt: occurred,
	})
	if err == nil && next.Run.Status == controlplane.StatusClosed {
		s.journal.SetRunActive(next.Run.ID, false)
	}
	return err
}

func outcomeBinding(
	current, next controlplane.Snapshot,
) (journal.EventKind, string) {
	kind := journal.EventProjectionUpdated
	actionID := ""
	for id, nextAction := range next.Actions {
		previous, ok := current.Actions[id]
		switch {
		case ok && !previous.Completed && nextAction.Completed:
			kind, actionID = journal.EventActionResult, id
		case ok && !previous.Ambiguous && nextAction.Ambiguous:
			kind, actionID = journal.EventActionAmbiguous, id
		}
	}
	return kind, actionID
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
