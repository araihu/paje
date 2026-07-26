// Package filesystem persists provider-neutral control-plane snapshots and
// append-only event segments for the v1 single-replica installation.
package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/controlplane"
)

var rootLocks sync.Map

type Store struct {
	root         string
	records      string
	events       string
	transactions string
	lock         *sync.Mutex
	cursorIndex  map[string]uint64
	fault        func(DurableBoundary) error
}

type DurableBoundary string

const (
	BoundaryTransactionCommitted DurableBoundary = "transaction_committed"
	BoundaryEventRootCommitted   DurableBoundary = "event_root_committed"
	BoundaryEventCommitted       DurableBoundary = "event_committed"
	BoundarySnapshotCommitted    DurableBoundary = "snapshot_committed"
	BoundaryTransactionCleared   DurableBoundary = "transaction_cleared"
)

type Option func(*Store)

func WithFaultInjector(inject func(DurableBoundary) error) Option {
	return func(store *Store) { store.fault = inject }
}

const transactionSchemaVersion = "paje.controlplane.filesystem-transaction/v1"

type transaction struct {
	SchemaVersion   string                `json:"schema_version"`
	ControlRunID    string                `json:"control_run_id"`
	ExpectedVersion uint64                `json:"expected_version"`
	Create          bool                  `json:"create"`
	Next            controlplane.Snapshot `json:"next"`
}

var _ controlplane.Store = (*Store)(nil)

func New(root string) (*Store, error) {
	return NewWithOptions(root)
}

func NewWithOptions(root string, options ...Option) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("create control-plane store: root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("create control-plane store: resolve root: %w", err)
	}
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: store root is a symlink", controlplane.ErrCorruptStore)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("create control-plane store: inspect root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create control-plane store root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("create control-plane store: canonicalize root: %w", err)
	}
	records := filepath.Join(canonical, "records")
	events := filepath.Join(canonical, "events")
	transactions := filepath.Join(canonical, "transactions")
	for _, directory := range []string{records, events, transactions} {
		if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create control-plane store directory: %w", err)
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: store directory is unsafe", controlplane.ErrCorruptStore)
		}
	}
	lockValue, _ := rootLocks.LoadOrStore(canonical, &sync.Mutex{})
	store := &Store{
		root: canonical, records: records, events: events, transactions: transactions,
		lock: lockValue.(*sync.Mutex), cursorIndex: make(map[string]uint64),
	}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	store.lock.Lock()
	defer store.lock.Unlock()
	if err := store.recoverTransactionsLocked(); err != nil {
		return nil, err
	}
	if err := store.auditLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Create(ctx context.Context, snapshot controlplane.Snapshot) (controlplane.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	normalizeCursor(&snapshot)
	if err := controlplane.ValidateSnapshot(snapshot); err != nil {
		return controlplane.Snapshot{}, err
	}
	if !validID(snapshot.Run.ID) {
		return controlplane.Snapshot{}, controlplane.ErrInvalidRecord
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	path := s.recordPath(snapshot.Run.ID)
	if _, err := os.Lstat(path); err == nil {
		return controlplane.Snapshot{}, controlplane.ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return controlplane.Snapshot{}, fmt.Errorf("create control-plane record: %w", err)
	}
	tx := transaction{
		SchemaVersion: transactionSchemaVersion, ControlRunID: snapshot.Run.ID,
		Create: true, Next: controlplane.CloneSnapshot(snapshot),
	}
	if err := s.commitTransactionLocked(ctx, tx); err != nil {
		return controlplane.Snapshot{}, fmt.Errorf("create control-plane record: %w", err)
	}
	return controlplane.CloneSnapshot(snapshot), nil
}

func (s *Store) Load(ctx context.Context, id string) (controlplane.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	if !validID(id) {
		return controlplane.Snapshot{}, controlplane.ErrNotFound
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	return s.loadLocked(id)
}

func (s *Store) Save(
	ctx context.Context,
	next controlplane.Snapshot,
	expectedVersion uint64,
) (controlplane.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	if !validID(next.Run.ID) {
		return controlplane.Snapshot{}, controlplane.ErrNotFound
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, err
	}
	current, err := s.loadLocked(next.Run.ID)
	if err != nil {
		return controlplane.Snapshot{}, err
	}
	if current.Version != expectedVersion {
		return controlplane.Snapshot{}, controlplane.ErrVersionConflict
	}
	if next.Run.ID != current.Run.ID || next.SchemaVersion != current.SchemaVersion ||
		len(next.Events) < len(current.Events) ||
		!reflect.DeepEqual(next.Events[:len(current.Events)], current.Events) {
		return controlplane.Snapshot{}, controlplane.ErrInvalidRecord
	}
	next.Version = current.Version + 1
	normalizeCursor(&next)
	if err := controlplane.ValidateSave(current, next); err != nil {
		return controlplane.Snapshot{}, err
	}
	tx := transaction{
		SchemaVersion: transactionSchemaVersion, ControlRunID: next.Run.ID,
		ExpectedVersion: expectedVersion, Next: controlplane.CloneSnapshot(next),
	}
	if err := s.commitTransactionLocked(ctx, tx); err != nil {
		return controlplane.Snapshot{}, fmt.Errorf("save control-plane record: %w", err)
	}
	return controlplane.CloneSnapshot(next), nil
}

func (s *Store) EventsAfter(
	ctx context.Context,
	id string,
	after uint64,
	limit int,
) ([]controlplane.Event, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, after, err
	}
	if !validID(id) {
		return nil, after, controlplane.ErrNotFound
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	snapshot, err := s.loadLocked(id)
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

func (s *Store) commitTransactionLocked(ctx context.Context, tx transaction) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := s.transactionPath(tx.ControlRunID)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%w: unfinished transaction already exists", controlplane.ErrCorruptStore)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := atomicWriteJSON(ctx, path, tx); err != nil {
		return err
	}
	if err := s.afterBoundary(BoundaryTransactionCommitted); err != nil {
		return err
	}
	if err := s.materializeEventsLocked(ctx, tx.Next, true); err != nil {
		return err
	}
	if err := atomicWriteJSON(ctx, s.recordPath(tx.ControlRunID), tx.Next); err != nil {
		return err
	}
	if err := s.afterBoundary(BoundarySnapshotCommitted); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := syncDirectory(s.transactions); err != nil {
		return err
	}
	s.cursorIndex[tx.ControlRunID] = tx.Next.Run.EventCursor
	return s.afterBoundary(BoundaryTransactionCleared)
}

func (s *Store) recoverTransactionsLocked() error {
	entries, err := os.ReadDir(s.transactions)
	if err != nil {
		return fmt.Errorf("%w: read transactions: %v", controlplane.ErrCorruptStore, err)
	}
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") ||
			!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: unsafe transaction entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) {
			return fmt.Errorf("%w: invalid transaction filename", controlplane.ErrCorruptStore)
		}
		var tx transaction
		path := filepath.Join(s.transactions, entry.Name())
		if err := readStrictJSON(path, &tx); err != nil ||
			tx.SchemaVersion != transactionSchemaVersion || tx.ControlRunID != id ||
			tx.Next.Run.ID != id {
			return fmt.Errorf("%w: invalid transaction %q", controlplane.ErrCorruptStore, id)
		}
		if err := controlplane.ValidateSnapshot(tx.Next); err != nil {
			return fmt.Errorf("%w: invalid transaction snapshot %q: %v", controlplane.ErrCorruptStore, id, err)
		}
		current, exists, err := s.loadRecordIfExistsLocked(id)
		if err != nil {
			return fmt.Errorf("%w: read transaction target %q: %v", controlplane.ErrCorruptStore, id, err)
		}
		snapshotCommitted := false
		if tx.Create {
			if tx.ExpectedVersion != 0 {
				return fmt.Errorf("%w: create transaction has expected version", controlplane.ErrCorruptStore)
			}
			if exists {
				if !reflect.DeepEqual(current, tx.Next) {
					return fmt.Errorf("%w: create transaction target mismatch", controlplane.ErrCorruptStore)
				}
				snapshotCommitted = true
			}
		} else {
			if !exists || tx.ExpectedVersion == 0 || tx.Next.Version != tx.ExpectedVersion+1 {
				return fmt.Errorf("%w: save transaction version is invalid", controlplane.ErrCorruptStore)
			}
			switch current.Version {
			case tx.ExpectedVersion:
				if err := controlplane.ValidateSave(current, tx.Next); err != nil {
					return fmt.Errorf("%w: save transaction transition is invalid: %v", controlplane.ErrCorruptStore, err)
				}
			case tx.Next.Version:
				if !reflect.DeepEqual(current, tx.Next) {
					return fmt.Errorf("%w: committed transaction target mismatch", controlplane.ErrCorruptStore)
				}
				snapshotCommitted = true
			default:
				return fmt.Errorf("%w: save transaction target version mismatch", controlplane.ErrCorruptStore)
			}
		}
		if err := s.materializeEventsLocked(context.Background(), tx.Next, false); err != nil {
			return fmt.Errorf("%w: recover transaction events %q: %v", controlplane.ErrCorruptStore, id, err)
		}
		if !snapshotCommitted {
			if err := atomicWriteJSON(context.Background(), s.recordPath(id), tx.Next); err != nil {
				return fmt.Errorf("%w: recover transaction snapshot %q: %v", controlplane.ErrCorruptStore, id, err)
			}
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("%w: clear recovered transaction %q: %v", controlplane.ErrCorruptStore, id, err)
		}
		if err := syncDirectory(s.transactions); err != nil {
			return fmt.Errorf("%w: sync recovered transactions: %v", controlplane.ErrCorruptStore, err)
		}
		s.cursorIndex[id] = tx.Next.Run.EventCursor
	}
	return nil
}

func (s *Store) materializeEventsLocked(ctx context.Context, snapshot controlplane.Snapshot, inject bool) error {
	directory := s.eventDirectory(snapshot.Run.ID)
	if len(snapshot.Events) == 0 {
		if entries, err := os.ReadDir(directory); err == nil && len(entries) != 0 {
			return fmt.Errorf("unexpected event segments")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	createdRoot := false
	if err := os.Mkdir(directory, 0o700); err == nil {
		createdRoot = true
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	if createdRoot {
		if err := syncDirectory(s.events); err != nil {
			return err
		}
		if inject {
			if err := s.afterBoundary(BoundaryEventRootCommitted); err != nil {
				return err
			}
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) > len(snapshot.Events) {
		return fmt.Errorf("event segment count exceeds transaction")
	}
	for index, event := range snapshot.Events {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(directory, eventFilename(uint64(index+1)))
		var existing controlplane.Event
		if err := readStrictJSON(path, &existing); err == nil {
			if existing != event {
				return fmt.Errorf("event segment mismatch")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := createJSONExclusive(ctx, path, event); err != nil {
			return err
		}
		if inject {
			if err := s.afterBoundary(BoundaryEventCommitted); err != nil {
				return err
			}
		}
	}
	return syncDirectory(directory)
}

func (s *Store) loadRecordIfExistsLocked(id string) (controlplane.Snapshot, bool, error) {
	encoded, err := os.ReadFile(s.recordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return controlplane.Snapshot{}, false, nil
	}
	if err != nil {
		return controlplane.Snapshot{}, false, err
	}
	snapshot, err := controlplane.DecodeSnapshot(encoded)
	if err != nil {
		return controlplane.Snapshot{}, false, err
	}
	return snapshot, true, nil
}

func (s *Store) afterBoundary(boundary DurableBoundary) error {
	if s.fault == nil {
		return nil
	}
	return s.fault(boundary)
}

func (s *Store) auditLocked() error {
	rootEntries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("%w: read root: %v", controlplane.ErrCorruptStore, err)
	}
	for _, entry := range rootEntries {
		if entry.Name() != "records" && entry.Name() != "events" && entry.Name() != "transactions" {
			return fmt.Errorf("%w: unexpected root entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
	}
	transactionEntries, err := os.ReadDir(s.transactions)
	if err != nil || len(transactionEntries) != 0 {
		return fmt.Errorf("%w: unfinished transaction audit", controlplane.ErrCorruptStore)
	}
	recordEntries, err := os.ReadDir(s.records)
	if err != nil {
		return fmt.Errorf("%w: read records: %v", controlplane.ErrCorruptStore, err)
	}
	records := make(map[string]controlplane.Snapshot, len(recordEntries))
	for _, entry := range recordEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("%w: unexpected record entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: unsafe record entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) {
			return fmt.Errorf("%w: invalid record filename", controlplane.ErrCorruptStore)
		}
		snapshot, err := s.loadLocked(id)
		if err != nil {
			return fmt.Errorf("%w: record %q: %v", controlplane.ErrCorruptStore, id, err)
		}
		records[id] = snapshot
		s.cursorIndex[id] = snapshot.Run.EventCursor
	}
	eventRoots, err := os.ReadDir(s.events)
	if err != nil {
		return fmt.Errorf("%w: read events: %v", controlplane.ErrCorruptStore, err)
	}
	for _, entry := range eventRoots {
		info, err := entry.Info()
		if err != nil || !entry.IsDir() || info.Mode()&os.ModeSymlink != 0 || !validID(entry.Name()) {
			return fmt.Errorf("%w: unsafe event root %q", controlplane.ErrCorruptStore, entry.Name())
		}
		snapshot, ok := records[entry.Name()]
		if !ok {
			return fmt.Errorf("%w: orphan event root %q", controlplane.ErrCorruptStore, entry.Name())
		}
		if err := s.auditEventsLocked(snapshot); err != nil {
			return err
		}
		delete(records, entry.Name())
	}
	for id, snapshot := range records {
		if len(snapshot.Events) != 0 {
			return fmt.Errorf("%w: missing event root for %q", controlplane.ErrCorruptStore, id)
		}
	}
	return nil
}

func (s *Store) auditEventsLocked(snapshot controlplane.Snapshot) error {
	directory := s.eventDirectory(snapshot.Run.ID)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("%w: read event stream: %v", controlplane.ErrCorruptStore, err)
	}
	if len(entries) != len(snapshot.Events) {
		return fmt.Errorf("%w: event segment count mismatch", controlplane.ErrCorruptStore)
	}
	for index, entry := range entries {
		info, err := entry.Info()
		wantName := eventFilename(uint64(index + 1))
		if err != nil || entry.Name() != wantName || !info.Mode().IsRegular() ||
			info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: unsafe event segment %q", controlplane.ErrCorruptStore, entry.Name())
		}
		var event controlplane.Event
		if err := readStrictJSON(filepath.Join(directory, entry.Name()), &event); err != nil ||
			event != snapshot.Events[index] {
			return fmt.Errorf("%w: event segment mismatch", controlplane.ErrCorruptStore)
		}
	}
	return nil
}

func (s *Store) loadLocked(id string) (controlplane.Snapshot, error) {
	encoded, err := os.ReadFile(s.recordPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return controlplane.Snapshot{}, controlplane.ErrNotFound
		}
		return controlplane.Snapshot{}, err
	}
	snapshot, err := controlplane.DecodeSnapshot(encoded)
	if err != nil {
		return controlplane.Snapshot{}, err
	}
	if snapshot.Run.ID != id {
		return controlplane.Snapshot{}, controlplane.ErrInvalidRecord
	}
	return snapshot, nil
}

func (s *Store) writeNewEventsLocked(
	ctx context.Context,
	id string,
	existing []controlplane.Event,
	added []controlplane.Event,
) error {
	if len(added) == 0 {
		return nil
	}
	directory := s.eventDirectory(id)
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create event directory: %w", err)
	}
	for index, event := range added {
		if err := ctx.Err(); err != nil {
			s.removeEventsLocked(id, added[:index])
			return err
		}
		wantCursor := uint64(len(existing) + index + 1)
		if event.Cursor != wantCursor {
			s.removeEventsLocked(id, added[:index])
			return controlplane.ErrInvalidRecord
		}
		path := filepath.Join(directory, eventFilename(event.Cursor))
		if err := createJSONExclusive(ctx, path, event); err != nil {
			s.removeEventsLocked(id, added[:index])
			return fmt.Errorf("append event segment: %w", err)
		}
	}
	return syncDirectory(directory)
}

func (s *Store) removeEventsLocked(id string, events []controlplane.Event) {
	for _, event := range events {
		_ = os.Remove(filepath.Join(s.eventDirectory(id), eventFilename(event.Cursor)))
	}
	directory := s.eventDirectory(id)
	if entries, err := os.ReadDir(directory); err == nil && len(entries) == 0 {
		_ = os.Remove(directory)
	}
}

func (s *Store) recordPath(id string) string {
	return filepath.Join(s.records, id+".json")
}

func (s *Store) eventDirectory(id string) string {
	return filepath.Join(s.events, id)
}

func (s *Store) transactionPath(id string) string {
	return filepath.Join(s.transactions, id+".json")
}

func eventFilename(cursor uint64) string {
	return fmt.Sprintf("%020d.json", cursor)
}

func normalizeCursor(snapshot *controlplane.Snapshot) {
	for index := range snapshot.Events {
		snapshot.Events[index].Cursor = uint64(index + 1)
		snapshot.Events[index].ControlRunID = snapshot.Run.ID
	}
	snapshot.Run.EventCursor = uint64(len(snapshot.Events))
}

func atomicWriteJSON(ctx context.Context, target string, value any) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(target)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	open := true
	defer func() {
		if open {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	open = false
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func createJSONExclusive(ctx context.Context, target string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(target)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func readStrictJSON(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for index, character := range id {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_') {
			continue
		}
		return false
	}
	return true
}
