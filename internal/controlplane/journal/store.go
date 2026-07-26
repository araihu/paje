package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store interface {
	Reserve(context.Context, Action) (Action, bool, error)
	Reservation(context.Context, string, string) (Action, error)
	Append(context.Context, string, uint64, Event) (Event, error)
	RunEvents(context.Context, RunCursor, int) ([]Event, RunCursor, error)
	Feed(context.Context, GlobalCursor, int) ([]Event, GlobalCursor, error)
	Checkpoint(context.Context, RunCursor, GlobalCursor, []byte) error
	LoadCheckpoint(context.Context, string) ([]byte, RunCursor, GlobalCursor, error)
	ActiveRuns(context.Context, string, int) ([]string, string, error)
}

type memoryCheckpoint struct {
	bytes  []byte
	run    RunCursor
	global GlobalCursor
}

// MemoryStore is the reference single-replica journal implementation. It is
// concurrency-safe and uses the same validation rules as durable stores.
type MemoryStore struct {
	mu             sync.Mutex
	installationID string
	actions        map[string]Action
	actionRuns     map[string]string
	idempotency    map[string]Action
	events         []Event
	eventByID      map[string]Event
	runEvents      map[string][]Event
	checkpoints    map[string]memoryCheckpoint
	active         map[string]bool
}

var _ Store = (*MemoryStore)(nil)

func NewMemoryStore(installationID string) (*MemoryStore, error) {
	if strings.TrimSpace(installationID) == "" {
		return nil, fmt.Errorf("%w: installation identity is required", ErrInvalidRecord)
	}
	return &MemoryStore{
		installationID: installationID,
		actions:        make(map[string]Action),
		actionRuns:     make(map[string]string),
		idempotency:    make(map[string]Action),
		eventByID:      make(map[string]Event),
		runEvents:      make(map[string][]Event),
		checkpoints:    make(map[string]memoryCheckpoint),
		active:         make(map[string]bool),
	}, nil
}

func (s *MemoryStore) InstallationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installationID
}

func (s *MemoryStore) RunHead(controlRunID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(len(s.runEvents[controlRunID]))
}

func (s *MemoryStore) SetRunActive(controlRunID string, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[controlRunID] = active
}

func (s *MemoryStore) ValidateExactReservation(ctx context.Context, action Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateAction(action); err != nil {
		return err
	}
	existing, err := s.Reservation(ctx, action.ControlRunID, action.ID)
	if err != nil {
		return err
	}
	if existing != action {
		return ErrConflict
	}
	return nil
}

func (s *MemoryStore) Reservation(
	ctx context.Context,
	controlRunID, actionID string,
) (Action, error) {
	if err := ctx.Err(); err != nil {
		return Action{}, err
	}
	if strings.TrimSpace(controlRunID) == "" || strings.TrimSpace(actionID) == "" {
		return Action{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.actions[actionID]
	if !ok || existing.ControlRunID != controlRunID || s.actionRuns[actionID] != controlRunID ||
		s.idempotency[existing.IdempotencyKey] != existing {
		return Action{}, ErrNotFound
	}
	digest, err := Digest(existing)
	if err != nil {
		return Action{}, err
	}
	reservations := 0
	for _, event := range s.runEvents[controlRunID] {
		if event.ActionID != actionID ||
			(event.Kind != EventActionReserved && event.Kind != EventMigrationAction) {
			continue
		}
		if event.Kind == EventActionReserved && event.PayloadDigest != digest {
			return Action{}, ErrConflict
		}
		reservations++
	}
	if reservations != 1 {
		return Action{}, ErrConflict
	}
	return existing, nil
}

func (s *MemoryStore) Reserve(ctx context.Context, action Action) (Action, bool, error) {
	if err := ctx.Err(); err != nil {
		return Action{}, false, err
	}
	if err := ValidateAction(action); err != nil {
		return Action{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Action{}, false, err
	}
	return s.reserveLocked(action)
}

func (s *MemoryStore) reserveLocked(action Action) (Action, bool, error) {
	if existing, ok := s.idempotency[action.IdempotencyKey]; ok {
		if !reflect.DeepEqual(existing, action) {
			return Action{}, false, ErrConflict
		}
		return existing, false, nil
	}
	if existing, ok := s.actions[action.ID]; ok {
		if !reflect.DeepEqual(existing, action) {
			return Action{}, false, ErrConflict
		}
		return existing, false, nil
	}
	if run := s.actionRuns[action.ID]; run != "" && run != action.ControlRunID {
		return Action{}, false, ErrConflict
	}
	s.actions[action.ID] = action
	s.actionRuns[action.ID] = action.ControlRunID
	s.idempotency[action.IdempotencyKey] = action
	payloadDigest, err := Digest(action)
	if err != nil {
		return Action{}, false, err
	}
	reservation := Event{
		ID:            stableID("reservation", action.ControlRunID, action.ID),
		ControlRunID:  action.ControlRunID,
		ActionID:      action.ID,
		Kind:          EventActionReserved,
		PayloadDigest: payloadDigest,
		OccurredAt:    time.Unix(0, 0).UTC(),
	}
	if _, err := s.appendLocked(action.ControlRunID, uint64(len(s.runEvents[action.ControlRunID])), reservation); err != nil {
		delete(s.actions, action.ID)
		delete(s.actionRuns, action.ID)
		delete(s.idempotency, action.IdempotencyKey)
		return Action{}, false, err
	}
	return action, true, nil
}

// ReserveAndAppend atomically creates or reconciles one exact reservation and
// appends its derived projection event. It is the in-memory transaction used
// by the control-plane mock Store's ReserveAction boundary.
func (s *MemoryStore) ReserveAndAppend(
	ctx context.Context,
	action Action,
	event Event,
) (Action, bool, Event, error) {
	if err := ctx.Err(); err != nil {
		return Action{}, false, Event{}, err
	}
	if err := ValidateAction(action); err != nil {
		return Action{}, false, Event{}, err
	}
	if event.ControlRunID != action.ControlRunID || event.ActionID != "" || event.Kind != EventProjectionUpdated {
		return Action{}, false, Event{}, ErrConflict
	}
	if err := ValidateEvent(event, false); err != nil {
		return Action{}, false, Event{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Action{}, false, Event{}, err
	}
	if existing, ok := s.eventByID[event.ID]; ok {
		if _, actionExists := s.actions[action.ID]; !actionExists {
			return Action{}, false, Event{}, ErrConflict
		}
		candidate := existing
		candidate.RunSequence = 0
		candidate.JournalPosition = 0
		if candidate != event {
			return Action{}, false, Event{}, ErrConflict
		}
	}
	reserved, created, err := s.reserveLocked(action)
	if err != nil {
		return Action{}, false, Event{}, err
	}
	appended, err := s.appendLocked(
		action.ControlRunID, uint64(len(s.runEvents[action.ControlRunID])), event,
	)
	if err != nil {
		return Action{}, false, Event{}, err
	}
	return reserved, created, appended, nil
}

func (s *MemoryStore) Append(
	ctx context.Context,
	controlRunID string,
	expectedRunSequence uint64,
	event Event,
) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	return s.appendLocked(controlRunID, expectedRunSequence, event)
}

func (s *MemoryStore) appendLocked(controlRunID string, expectedRunSequence uint64, event Event) (Event, error) {
	if event.ControlRunID != controlRunID {
		return Event{}, ErrConflict
	}
	if existing, ok := s.eventByID[event.ID]; ok {
		candidate := existing
		candidate.RunSequence = 0
		candidate.JournalPosition = 0
		if candidate != event || existing.RunSequence == 0 {
			return Event{}, ErrConflict
		}
		return existing, nil
	}
	if err := ValidateEvent(event, false); err != nil {
		return Event{}, err
	}
	currentRunSequence := uint64(len(s.runEvents[controlRunID]))
	if expectedRunSequence != currentRunSequence {
		return Event{}, ErrConflict
	}
	if event.ActionID != "" {
		action, ok := s.actions[event.ActionID]
		if !ok || action.ControlRunID != controlRunID || s.actionRuns[event.ActionID] != controlRunID {
			return Event{}, ErrConflict
		}
		if err := ValidateOutcomeTransition(s.events, event); err != nil {
			return Event{}, err
		}
	}
	event.RunSequence = currentRunSequence + 1
	event.JournalPosition = JournalPosition(len(s.events) + 1)
	if err := ValidateEvent(event, true); err != nil {
		return Event{}, err
	}
	s.events = append(s.events, event)
	s.runEvents[controlRunID] = append(s.runEvents[controlRunID], event)
	s.eventByID[event.ID] = event
	s.active[controlRunID] = true
	return event, nil
}

func (s *MemoryStore) RunEvents(
	ctx context.Context,
	cursor RunCursor,
	limit int,
) ([]Event, RunCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, cursor, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor.InstallationID == "" && cursor.SchemaVersion == 0 && cursor.RunSequence == 0 {
		cursor.InstallationID = s.installationID
		cursor.SchemaVersion = SchemaVersion
	}
	events, ok := s.runEvents[cursor.ControlRunID]
	if err := s.validateRunCursorLocked(cursor, uint64(len(events)), ok); err != nil {
		return nil, cursor, err
	}
	limit = pageLimit(limit)
	start := int(cursor.RunSequence)
	end := min(start+limit, len(events))
	result := append([]Event(nil), events[start:end]...)
	next := cursor
	if len(result) > 0 {
		next.RunSequence = result[len(result)-1].RunSequence
	}
	return result, next, nil
}

func (s *MemoryStore) Feed(
	ctx context.Context,
	cursor GlobalCursor,
	limit int,
) ([]Event, GlobalCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, cursor, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor == (GlobalCursor{}) {
		cursor = NewGlobalCursor(s.installationID)
	}
	if err := s.validateGlobalCursorLocked(cursor); err != nil {
		return nil, cursor, err
	}
	limit = pageLimit(limit)
	start := int(cursor.JournalPosition)
	end := min(start+limit, len(s.events))
	result := append([]Event(nil), s.events[start:end]...)
	next := cursor
	if len(result) > 0 {
		next.JournalPosition = result[len(result)-1].JournalPosition
	}
	return result, next, nil
}

func (s *MemoryStore) Checkpoint(
	ctx context.Context,
	runCursor RunCursor,
	globalCursor GlobalCursor,
	projection []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events, exists := s.runEvents[runCursor.ControlRunID]
	if err := s.validateRunCursorLocked(runCursor, uint64(len(events)), exists); err != nil {
		return err
	}
	if err := s.validateGlobalCursorLocked(globalCursor); err != nil {
		return err
	}
	if runCursor.InstallationID != globalCursor.InstallationID {
		return ErrCursor
	}
	if runCursor.RunSequence != uint64(len(events)) ||
		globalCursor.JournalPosition != JournalPosition(len(s.events)) {
		return ErrCursor
	}
	if previous, ok := s.checkpoints[runCursor.ControlRunID]; ok {
		if runCursor.RunSequence < previous.run.RunSequence ||
			globalCursor.JournalPosition < previous.global.JournalPosition {
			return ErrCursor
		}
	}
	s.checkpoints[runCursor.ControlRunID] = memoryCheckpoint{
		bytes:  append([]byte(nil), projection...),
		run:    runCursor,
		global: globalCursor,
	}
	return nil
}

func (s *MemoryStore) LoadCheckpoint(
	ctx context.Context,
	controlRunID string,
) ([]byte, RunCursor, GlobalCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, RunCursor{}, GlobalCursor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, ok := s.checkpoints[controlRunID]
	if !ok {
		return nil, RunCursor{}, GlobalCursor{}, ErrNotFound
	}
	return append([]byte(nil), checkpoint.bytes...), checkpoint.run, checkpoint.global, nil
}

func (s *MemoryStore) ActiveRuns(
	ctx context.Context,
	after string,
	limit int,
) ([]string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.active))
	for id, active := range s.active {
		if active {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	start := 0
	if after != "" {
		prefix := s.installationID + ":"
		if !strings.HasPrefix(after, prefix) {
			return nil, "", ErrCursor
		}
		last := strings.TrimPrefix(after, prefix)
		if strings.TrimSpace(last) == "" {
			return nil, "", ErrCursor
		}
		index := sort.SearchStrings(ids, last)
		start = index
		if index < len(ids) && ids[index] == last {
			start++
		}
	}
	limit = pageLimit(limit)
	end := min(start+limit, len(ids))
	result := append([]string(nil), ids[start:end]...)
	if end < len(ids) && len(result) > 0 {
		return result, s.installationID + ":" + result[len(result)-1], nil
	}
	return result, "", nil
}

func (s *MemoryStore) validateRunCursorLocked(cursor RunCursor, head uint64, exists bool) error {
	if cursor.InstallationID != s.installationID || cursor.SchemaVersion != SchemaVersion ||
		strings.TrimSpace(cursor.ControlRunID) == "" || cursor.RunSequence > head {
		return ErrCursor
	}
	if !exists && cursor.RunSequence != 0 {
		return ErrCursor
	}
	return nil
}

func (s *MemoryStore) validateGlobalCursorLocked(cursor GlobalCursor) error {
	if cursor.InstallationID != s.installationID || cursor.SchemaVersion != SchemaVersion ||
		uint64(cursor.JournalPosition) > uint64(len(s.events)) {
		return ErrCursor
	}
	return nil
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func stableID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{prefix}, values...), "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}
