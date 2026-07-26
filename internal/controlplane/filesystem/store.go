// Package filesystem persists provider-neutral control-plane snapshots and
// append-only event segments for the v1 single-replica installation.
package filesystem

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/araihu/paje/internal/controlplane"
	"github.com/araihu/paje/internal/controlplane/journal"
	"github.com/araihu/paje/internal/controlplane/projection"
)

var rootLocks sync.Map

type Store struct {
	root            string
	records         string
	events          string
	transactions    string
	journalRoot     string
	journalEvents   string
	journalPayloads string
	journalCommits  string
	commitStaging   string
	commitsIdentity os.FileInfo
	stagingIdentity os.FileInfo
	runs            string
	active          string
	installationID  string
	lock            *sync.Mutex
	cursorIndex     map[string]uint64
	fault           func(DurableBoundary) error
}

type DurableBoundary string

const (
	BoundaryTransactionCommitted        DurableBoundary = "transaction_committed"
	BoundaryEventRootCommitted          DurableBoundary = "event_root_committed"
	BoundaryEventCommitted              DurableBoundary = "event_committed"
	BoundarySnapshotCommitted           DurableBoundary = "snapshot_committed"
	BoundaryTransactionCleared          DurableBoundary = "transaction_cleared"
	BoundaryBeforeReservation           DurableBoundary = "before_reservation"
	BoundaryReservationPersisted        DurableBoundary = "reservation_persisted"
	BoundaryAfterReservation            DurableBoundary = "after_reservation"
	BoundaryGlobalPosition              DurableBoundary = "global_position_selected"
	BoundaryCanonicalVisible            DurableBoundary = "canonical_event_visible"
	BoundaryRunIndexRepaired            DurableBoundary = "run_index_repaired"
	BoundaryResultAppended              DurableBoundary = "result_appended"
	BoundaryProjectionRebuilt           DurableBoundary = "projection_rebuilt"
	BoundaryCheckpointWritten           DurableBoundary = "checkpoint_written"
	BoundaryActiveIndexUpdated          DurableBoundary = "active_index_updated"
	BoundaryResponse                    DurableBoundary = "response"
	BoundaryBeforeAuthoritativeCommit   DurableBoundary = "before_authoritative_commit"
	BoundaryAuthoritativeCommitPrepared DurableBoundary = "authoritative_commit_prepared"
	BoundaryAuthoritativeCommitVisible  DurableBoundary = "authoritative_commit_visible"
	BoundaryAuthoritativeCommitResponse DurableBoundary = "authoritative_commit_response"
)

type Option func(*Store)

func WithFaultInjector(inject func(DurableBoundary) error) Option {
	return func(store *Store) { store.fault = inject }
}

const transactionSchemaVersion = "paje.controlplane.filesystem-transaction/v1"
const manifestSchemaVersion = "paje.controlplane.journal-manifest/v1"
const migrationSchemaVersion = "paje.controlplane.journal-migration/v1"
const checkpointSchemaVersion = "paje.controlplane.journal-checkpoint/v1"
const authoritativeCommitSchemaVersion = "paje.controlplane.authoritative-commit/v1"
const maxAuthoritativeCommitRecordBytes = 2*(4*((journal.MaxPayloadBytes+2)/3)) + (64 << 10)

type manifest struct {
	ManifestSchema string `json:"manifest_schema"`
	InstallationID string `json:"installation_id"`
	SchemaVersion  uint32 `json:"schema_version"`
}

type migrationMarker struct {
	SchemaVersion   string                  `json:"schema_version"`
	JournalPosition journal.JournalPosition `json:"journal_position"`
	SnapshotDigest  string                  `json:"snapshot_digest"`
}

type migrationCompletion struct {
	Version uint64 `json:"version"`
}

type migrationReceiptRun struct {
	started   bool
	snapshot  controlplane.Snapshot
	digest    string
	completed bool
}

type journalCheckpoint struct {
	SchemaVersion string               `json:"schema_version"`
	RunCursor     journal.RunCursor    `json:"run_cursor"`
	GlobalCursor  journal.GlobalCursor `json:"global_cursor"`
	Projection    []byte               `json:"projection"`
}

type activeRun struct {
	ControlRunID    string                  `json:"control_run_id"`
	RunSequence     uint64                  `json:"run_sequence"`
	JournalPosition journal.JournalPosition `json:"journal_position"`
}

type authoritativeCommit struct {
	SchemaVersion  string         `json:"schema_version"`
	Action         journal.Action `json:"action"`
	Reservation    journal.Event  `json:"reservation"`
	Outcome        journal.Event  `json:"outcome"`
	RequestPayload []byte         `json:"request_payload"`
	OutcomePayload []byte         `json:"outcome_payload"`
}

type transaction struct {
	SchemaVersion   string                `json:"schema_version"`
	ControlRunID    string                `json:"control_run_id"`
	ExpectedVersion uint64                `json:"expected_version"`
	Create          bool                  `json:"create"`
	Action          *journal.Action       `json:"action,omitempty"`
	EventKind       journal.EventKind     `json:"event_kind"`
	EventActionID   string                `json:"event_action_id,omitempty"`
	Next            controlplane.Snapshot `json:"next"`
}

var _ controlplane.Store = (*Store)(nil)
var _ journal.AuthoritativeStore = (*Store)(nil)

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
	journalRoot := filepath.Join(canonical, "journal")
	journalEvents := filepath.Join(journalRoot, "events")
	journalPayloads := filepath.Join(journalRoot, "payloads")
	journalCommits := filepath.Join(journalRoot, "commits")
	commitStaging := filepath.Join(journalRoot, "commit-staging")
	runs := filepath.Join(canonical, "runs")
	active := filepath.Join(canonical, "active")
	for _, directory := range []string{
		records, events, transactions, journalRoot, journalEvents, journalPayloads,
		journalCommits, commitStaging, runs, active,
	} {
		if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create control-plane store directory: %w", err)
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: store directory is unsafe", controlplane.ErrCorruptStore)
		}
	}
	commitsIdentity, err := os.Lstat(journalCommits)
	if err != nil {
		return nil, fmt.Errorf("inspect authoritative commits identity: %w", err)
	}
	stagingIdentity, err := os.Lstat(commitStaging)
	if err != nil {
		return nil, fmt.Errorf("inspect authoritative commit staging identity: %w", err)
	}
	lockValue, _ := rootLocks.LoadOrStore(canonical, &sync.Mutex{})
	store := &Store{
		root: canonical, records: records, events: events, transactions: transactions,
		journalRoot: journalRoot, journalEvents: journalEvents,
		journalPayloads: journalPayloads, journalCommits: journalCommits,
		commitStaging: commitStaging, commitsIdentity: commitsIdentity,
		stagingIdentity: stagingIdentity, runs: runs, active: active,
		lock: lockValue.(*sync.Mutex), cursorIndex: make(map[string]uint64),
	}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	store.lock.Lock()
	defer store.lock.Unlock()
	if err := store.ensureManifestLocked(); err != nil {
		return nil, err
	}
	if err := store.recoverCommitStagingLocked(); err != nil {
		return nil, err
	}
	if err := store.auditJournalLocked(); err != nil {
		return nil, err
	}
	if _, err := store.validateExistingMigrationMarkerLocked(); err != nil {
		return nil, err
	}
	if err := store.repairSnapshotCheckpointsLocked(); err != nil {
		return nil, err
	}
	if err := store.recoverTransactionsLocked(); err != nil {
		return nil, err
	}
	if err := store.auditLocked(); err != nil {
		return nil, err
	}
	if err := store.migrateSnapshotsLocked(); err != nil {
		return nil, err
	}
	if err := store.repairSnapshotCheckpointsLocked(); err != nil {
		return nil, err
	}
	if err := store.repairJournalDerivedLocked(); err != nil {
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
		Create: true, EventKind: journal.EventProjectionUpdated,
		Next: controlplane.CloneSnapshot(snapshot),
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
	snapshot, err := s.loadLocked(id)
	if err == nil {
		return snapshot, nil
	}
	if info, statErr := os.Lstat(s.recordPath(id)); statErr == nil &&
		(info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return controlplane.Snapshot{}, fmt.Errorf("%w: unsafe derived snapshot checkpoint", controlplane.ErrCorruptStore)
	}
	if !errors.Is(err, controlplane.ErrNotFound) && !errors.Is(err, controlplane.ErrInvalidRecord) {
		return controlplane.Snapshot{}, err
	}
	if repairErr := s.repairSnapshotCheckpointsLocked(); repairErr != nil {
		return controlplane.Snapshot{}, repairErr
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
	eventKind, eventActionID, err := snapshotEventBinding(current, next)
	if err != nil {
		return controlplane.Snapshot{}, err
	}
	tx := transaction{
		SchemaVersion: transactionSchemaVersion, ControlRunID: next.Run.ID,
		ExpectedVersion: expectedVersion, EventKind: eventKind, EventActionID: eventActionID,
		Next: controlplane.CloneSnapshot(next),
	}
	if err := s.commitTransactionLocked(ctx, tx); err != nil {
		return controlplane.Snapshot{}, fmt.Errorf("save control-plane record: %w", err)
	}
	return controlplane.CloneSnapshot(next), nil
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
	if !validID(next.Run.ID) || !validID(action.ID) || !validID(action.ControlRunID) {
		return controlplane.Snapshot{}, false, controlplane.ErrNotFound
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return controlplane.Snapshot{}, false, err
	}
	current, err := s.loadLocked(next.Run.ID)
	if err != nil {
		return controlplane.Snapshot{}, false, err
	}
	next.Version = expectedVersion + 1
	normalizeCursor(&next)
	if current.Version != expectedVersion {
		if current.Version == next.Version && reflect.DeepEqual(current, next) {
			actions, readErr := s.readJournalActionsLocked()
			if readErr != nil {
				return controlplane.Snapshot{}, false, readErr
			}
			existing, exists, conflictErr := exactReservation(actions, action)
			if conflictErr != nil || !exists || existing != action {
				if conflictErr != nil {
					return controlplane.Snapshot{}, false, conflictErr
				}
				return controlplane.Snapshot{}, false, journal.ErrConflict
			}
			events, readErr := s.readJournalEventsLocked()
			if readErr != nil {
				return controlplane.Snapshot{}, false, readErr
			}
			if !hasPriorReservation(events, action.ControlRunID, action.ID) {
				return controlplane.Snapshot{}, false, journal.ErrConflict
			}
			payloadDigest, digestErr := journal.Digest(current)
			if digestErr != nil {
				return controlplane.Snapshot{}, false, digestErr
			}
			snapshotEventID := stableJournalID("snapshot", current.Run.ID, fmt.Sprintf("%d", current.Version))
			projectionBound := false
			for _, event := range events {
				if event.ID == snapshotEventID && event.ControlRunID == current.Run.ID &&
					event.Kind == journal.EventProjectionUpdated && event.ActionID == "" &&
					event.PayloadDigest == payloadDigest {
					projectionBound = true
					break
				}
			}
			if !projectionBound {
				return controlplane.Snapshot{}, false, journal.ErrConflict
			}
			return controlplane.CloneSnapshot(current), false, nil
		}
		return controlplane.Snapshot{}, false, controlplane.ErrVersionConflict
	}
	if err := controlplane.ValidateActionReservation(current, next, action); err != nil {
		return controlplane.Snapshot{}, false, err
	}
	actions, err := s.readJournalActionsLocked()
	if err != nil {
		return controlplane.Snapshot{}, false, err
	}
	_, exists, err := exactReservation(actions, action)
	if err != nil {
		return controlplane.Snapshot{}, false, err
	}
	actionCopy := action
	tx := transaction{
		SchemaVersion: transactionSchemaVersion, ControlRunID: next.Run.ID,
		ExpectedVersion: expectedVersion, Action: &actionCopy,
		EventKind: journal.EventProjectionUpdated, Next: controlplane.CloneSnapshot(next),
	}
	if err := s.commitTransactionLocked(ctx, tx); err != nil {
		return controlplane.Snapshot{}, false, fmt.Errorf("reserve action and save control-plane record: %w", err)
	}
	return controlplane.CloneSnapshot(next), !exists, nil
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
	if err := s.preflightTransactionOutcomeLocked(tx); err != nil {
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
	if tx.Action != nil {
		if _, err := s.persistReservationLocked(ctx, *tx.Action); err != nil {
			return err
		}
	}
	if err := s.persistSnapshotEventLocked(ctx, tx); err != nil {
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
			tx.Next.Run.ID != id || !validTransactionBinding(tx) {
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
				eventKind, eventActionID, bindingErr := snapshotEventBinding(current, tx.Next)
				if bindingErr != nil || eventKind != tx.EventKind || eventActionID != tx.EventActionID {
					return fmt.Errorf("%w: save transaction journal binding is invalid", controlplane.ErrCorruptStore)
				}
				if tx.Action != nil {
					if err := controlplane.ValidateActionReservation(current, tx.Next, *tx.Action); err != nil {
						return fmt.Errorf("%w: action reservation transaction is invalid: %v", controlplane.ErrCorruptStore, err)
					}
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
		if err := s.preflightTransactionOutcomeLocked(tx); err != nil {
			return fmt.Errorf("%w: transaction outcome binding is invalid: %v", controlplane.ErrCorruptStore, err)
		}
		if tx.Action != nil {
			if _, err := s.persistReservationLocked(context.Background(), *tx.Action); err != nil {
				return fmt.Errorf("%w: recover action reservation %q: %v", controlplane.ErrCorruptStore, id, err)
			}
		}
		if err := s.persistSnapshotEventLocked(context.Background(), tx); err != nil {
			return fmt.Errorf("%w: recover transaction journal %q: %v", controlplane.ErrCorruptStore, id, err)
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

func (s *Store) ensureCommitsIdentityLocked() error {
	return requireSameAuthoritativeDirectory(
		s.journalCommits, s.commitsIdentity, "authoritative commits directory changed after construction",
	)
}

func (s *Store) ensureStagingIdentityLocked() error {
	return requireSameAuthoritativeDirectory(
		s.commitStaging, s.stagingIdentity, "authoritative commit staging directory changed after construction",
	)
}

func requireSameAuthoritativeDirectory(path string, expected os.FileInfo, message string) error {
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 ||
		current.Mode().Perm()&0o077 != 0 || !os.SameFile(expected, current) {
		return fmt.Errorf("%w: %s", controlplane.ErrCorruptStore, message)
	}
	return nil
}

func (s *Store) stageAuthoritativeCommitLocked(
	ctx context.Context,
	commit authoritativeCommit,
) (stagedPath string, returnErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.ensureStagingIdentityLocked(); err != nil {
		return "", err
	}
	encoded, err := journal.CanonicalJSON(commit)
	if err != nil {
		return "", err
	}
	if len(encoded) > maxAuthoritativeCommitRecordBytes {
		return "", journal.ErrInvalidRecord
	}
	temporary, err := os.CreateTemp(
		s.commitStaging,
		".commit-"+strings.TrimSuffix(authoritativeCommitFilename(commit.Action.ID), ".json")+"-*.tmp",
	)
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	open := true
	defer func() {
		if open {
			_ = temporary.Close()
		}
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := temporary.Write(encoded); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	open = false
	if err := syncDirectory(s.commitStaging); err != nil {
		return "", err
	}
	return temporaryPath, nil
}

func (s *Store) recoverCommitStagingLocked() error {
	if err := s.ensureStagingIdentityLocked(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.commitStaging)
	if err != nil {
		return fmt.Errorf("%w: read authoritative commit staging: %v", controlplane.ErrCorruptStore, err)
	}
	removed := false
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil || entry.IsDir() || !info.Mode().IsRegular() ||
			info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
			!validAuthoritativeCommitStagingFilename(entry.Name()) ||
			info.Size() > int64(maxAuthoritativeCommitRecordBytes) {
			return fmt.Errorf("%w: unsafe authoritative commit staging entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
		if err := os.Remove(filepath.Join(s.commitStaging, entry.Name())); err != nil {
			return fmt.Errorf("%w: remove incomplete authoritative commit: %v", controlplane.ErrCorruptStore, err)
		}
		removed = true
	}
	if removed {
		return syncDirectory(s.commitStaging)
	}
	return nil
}

func (s *Store) auditLocked() error {
	rootEntries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("%w: read root: %v", controlplane.ErrCorruptStore, err)
	}
	for _, entry := range rootEntries {
		if entry.Name() != "records" && entry.Name() != "events" && entry.Name() != "transactions" &&
			entry.Name() != "journal" && entry.Name() != "runs" && entry.Name() != "active" {
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
	info, statErr := os.Lstat(s.recordPath(id))
	if statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600) {
		return controlplane.Snapshot{}, fmt.Errorf("%w: unsafe derived snapshot checkpoint", controlplane.ErrCorruptStore)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return controlplane.Snapshot{}, statErr
	}
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

func (s *Store) Reserve(
	ctx context.Context,
	action journal.Action,
) (journal.Action, bool, error) {
	if err := ctx.Err(); err != nil {
		return journal.Action{}, false, err
	}
	if err := journal.ValidateAction(action); err != nil || !validID(action.ID) || !validID(action.ControlRunID) {
		if err != nil {
			return journal.Action{}, false, err
		}
		return journal.Action{}, false, journal.ErrInvalidRecord
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	created, err := s.persistReservationLocked(ctx, action)
	if err != nil {
		return journal.Action{}, false, err
	}
	if err := s.afterBoundary(BoundaryResponse); err != nil {
		return journal.Action{}, false, err
	}
	return action, created, nil
}

func (s *Store) Commit(
	ctx context.Context,
	request journal.CommitRequest,
) (journal.CommitReceipt, error) {
	if err := ctx.Err(); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := journal.ValidateAction(request.Action); err != nil {
		return journal.CommitReceipt{}, err
	}
	canonicalOutcome, err := canonicalJournalEvent(request.Outcome)
	if err != nil {
		return journal.CommitReceipt{}, err
	}
	request.Outcome = canonicalOutcome
	if !validID(request.Action.ID) || !validID(request.Action.ControlRunID) ||
		!validID(request.Outcome.ID) {
		return journal.CommitReceipt{}, journal.ErrInvalidRecord
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := s.recoverCommitStagingLocked(); err != nil {
		return journal.CommitReceipt{}, err
	}
	commits, events, actions, err := s.readAuthoritativeStateLocked()
	if err != nil {
		return journal.CommitReceipt{}, err
	}
	if existing, found, exactErr := s.exactAuthoritativeCommitLocked(commits, request); found || exactErr != nil {
		if exactErr != nil {
			return journal.CommitReceipt{}, exactErr
		}
		return journal.CommitReceipt{
			Action: existing.Action, Reservation: existing.Reservation,
			Outcome: existing.Outcome, Created: false,
		}, nil
	}
	if err := journal.ValidateCommitRequest(request); err != nil {
		return journal.CommitReceipt{}, err
	}
	if _, exists, conflictErr := exactReservation(actions, request.Action); conflictErr != nil || exists {
		if conflictErr != nil {
			return journal.CommitReceipt{}, conflictErr
		}
		return journal.CommitReceipt{}, journal.ErrConflict
	}
	currentRunSequence := runHead(events, request.Action.ControlRunID)
	if request.ExpectedRun.InstallationID != s.installationID ||
		request.ExpectedGlobal.InstallationID != s.installationID ||
		request.ExpectedRun.RunSequence != currentRunSequence ||
		request.ExpectedGlobal.JournalPosition != journal.JournalPosition(len(events)) {
		return journal.CommitReceipt{}, journal.ErrConflict
	}
	if currentRunSequence > ^uint64(0)-2 || uint64(len(events)) > ^uint64(0)-2 {
		return journal.CommitReceipt{}, journal.ErrConflict
	}
	actionDigest, err := journal.Digest(request.Action)
	if err != nil {
		return journal.CommitReceipt{}, err
	}
	reservation := journal.Event{
		ID:           stableJournalID("reservation", request.Action.ControlRunID, request.Action.ID),
		ControlRunID: request.Action.ControlRunID, ActionID: request.Action.ID,
		Kind: journal.EventActionReserved, PayloadDigest: actionDigest,
		OccurredAt: time.Unix(0, 0).UTC(), RunSequence: currentRunSequence + 1,
		JournalPosition: journal.JournalPosition(len(events) + 1),
	}
	outcome := request.Outcome
	outcome.RunSequence = currentRunSequence + 2
	outcome.JournalPosition = journal.JournalPosition(len(events) + 2)
	if err := journal.ValidateEvent(reservation, true); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := journal.ValidateEvent(outcome, true); err != nil {
		return journal.CommitReceipt{}, err
	}
	if reservation.ID == outcome.ID {
		return journal.CommitReceipt{}, journal.ErrConflict
	}
	for _, existing := range events {
		if existing.ID == reservation.ID || existing.ID == outcome.ID {
			return journal.CommitReceipt{}, journal.ErrConflict
		}
	}
	record := authoritativeCommit{
		SchemaVersion: authoritativeCommitSchemaVersion,
		Action:        request.Action, Reservation: reservation, Outcome: outcome,
		RequestPayload: append([]byte(nil), request.RequestPayload...),
		OutcomePayload: append([]byte(nil), request.OutcomePayload...),
	}
	if err := validateAuthoritativeCommit(record); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := s.afterBoundary(BoundaryBeforeAuthoritativeCommit); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := s.ensureCommitsIdentityLocked(); err != nil {
		return journal.CommitReceipt{}, err
	}
	staged, err := s.stageAuthoritativeCommitLocked(ctx, record)
	if err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := s.afterBoundary(BoundaryAuthoritativeCommitPrepared); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := s.ensureStagingIdentityLocked(); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := s.ensureCommitsIdentityLocked(); err != nil {
		return journal.CommitReceipt{}, err
	}
	target := filepath.Join(s.journalCommits, authoritativeCommitFilename(request.Action.ID))
	if _, err := os.Lstat(target); err == nil {
		_ = os.Remove(staged)
		return journal.CommitReceipt{}, journal.ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(staged)
		return journal.CommitReceipt{}, err
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Remove(staged)
		return journal.CommitReceipt{}, err
	}
	if err := syncDirectory(s.journalCommits); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := syncDirectory(s.commitStaging); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := s.afterBoundary(BoundaryAuthoritativeCommitVisible); err != nil {
		return journal.CommitReceipt{}, err
	}
	if err := s.afterBoundary(BoundaryAuthoritativeCommitResponse); err != nil {
		return journal.CommitReceipt{}, err
	}
	return journal.CommitReceipt{
		Action: request.Action, Reservation: reservation, Outcome: outcome, Created: true,
	}, nil
}

func (s *Store) Payload(ctx context.Context, digest string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !journal.ValidDigest(digest) {
		return nil, journal.ErrInvalidRecord
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	commits, _, _, err := s.readAuthoritativeStateLocked()
	if err != nil {
		return nil, err
	}
	if encoded, err := s.readPayloadLocked(digest); err == nil {
		return append([]byte(nil), encoded...), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var found []byte
	for _, commit := range commits {
		for payloadDigest, payload := range map[string][]byte{
			commit.Action.CanonicalRequestDigest: commit.RequestPayload,
			commit.Outcome.PayloadDigest:         commit.OutcomePayload,
		} {
			if payloadDigest != digest {
				continue
			}
			if found != nil && !bytes.Equal(found, payload) {
				return nil, fmt.Errorf("%w: authoritative payload digest collision", controlplane.ErrCorruptStore)
			}
			found = append([]byte(nil), payload...)
		}
	}
	if found == nil {
		return nil, journal.ErrNotFound
	}
	return found, nil
}

func (s *Store) Reservation(
	ctx context.Context,
	controlRunID, actionID string,
) (journal.Action, error) {
	if err := ctx.Err(); err != nil {
		return journal.Action{}, err
	}
	if !validID(controlRunID) || !validID(actionID) {
		return journal.Action{}, journal.ErrNotFound
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.reservationLocked(controlRunID, actionID)
}

func (s *Store) reservationLocked(controlRunID, actionID string) (journal.Action, error) {
	actions, err := s.readJournalActionsLocked()
	if err != nil {
		return journal.Action{}, err
	}
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return journal.Action{}, err
	}
	if err := validateJournalActionBindings(events, actions); err != nil {
		return journal.Action{}, err
	}
	var reservation journal.Action
	found := false
	for _, action := range actions {
		if action.ID != actionID {
			continue
		}
		if found || action.ControlRunID != controlRunID {
			return journal.Action{}, journal.ErrConflict
		}
		reservation, found = action, true
	}
	if !found {
		return journal.Action{}, journal.ErrNotFound
	}
	for _, action := range actions {
		if action.ID != reservation.ID && action.IdempotencyKey == reservation.IdempotencyKey {
			return journal.Action{}, journal.ErrConflict
		}
	}
	digest, err := journal.Digest(reservation)
	if err != nil {
		return journal.Action{}, err
	}
	count := 0
	for _, event := range events {
		if event.ControlRunID != controlRunID || event.ActionID != actionID ||
			(event.Kind != journal.EventActionReserved && event.Kind != journal.EventMigrationAction) {
			continue
		}
		if event.Kind == journal.EventActionReserved && event.PayloadDigest != digest {
			return journal.Action{}, journal.ErrConflict
		}
		count++
	}
	if count != 1 {
		return journal.Action{}, journal.ErrConflict
	}
	return reservation, nil
}

func (s *Store) persistReservationLocked(ctx context.Context, action journal.Action) (bool, error) {
	actions, err := s.readJournalActionsLocked()
	if err != nil {
		return false, err
	}
	existing, exists, err := exactReservation(actions, action)
	if err != nil {
		return false, err
	}
	if exists {
		if err := s.ensureReservationEventLocked(ctx, actions, existing); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := s.ensureRunDirectoriesLocked(action.ControlRunID); err != nil {
		return false, err
	}
	if err := s.afterBoundary(BoundaryBeforeReservation); err != nil {
		return false, err
	}
	actionPath := filepath.Join(s.actionDirectory(action.ControlRunID), action.ID+".json")
	if err := createJSONExclusive(ctx, actionPath, action); err != nil {
		return false, err
	}
	if err := syncDirectory(s.actionDirectory(action.ControlRunID)); err != nil {
		return false, err
	}
	if err := s.afterBoundary(BoundaryReservationPersisted); err != nil {
		return false, err
	}
	if err := s.ensureReservationEventLocked(ctx, actionsWith(actions, action), action); err != nil {
		return false, err
	}
	if err := s.afterBoundary(BoundaryAfterReservation); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ensureReservationEventLocked(
	ctx context.Context,
	actions []journal.Action,
	action journal.Action,
) error {
	digest, err := journal.Digest(action)
	if err != nil {
		return err
	}
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return err
	}
	event := journal.Event{
		ID:           stableJournalID("reservation", action.ControlRunID, action.ID),
		ControlRunID: action.ControlRunID, ActionID: action.ID,
		Kind: journal.EventActionReserved, PayloadDigest: digest,
		OccurredAt: time.Unix(0, 0).UTC(),
	}
	_, err = s.appendJournalLocked(
		ctx, events, actions, action.ControlRunID,
		runHead(events, action.ControlRunID), event,
	)
	return err
}

func (s *Store) Append(
	ctx context.Context,
	controlRunID string,
	expectedRunSequence uint64,
	event journal.Event,
) (journal.Event, error) {
	if err := ctx.Err(); err != nil {
		return journal.Event{}, err
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return journal.Event{}, err
	}
	actions, err := s.readJournalActionsLocked()
	if err != nil {
		return journal.Event{}, err
	}
	return s.appendJournalLocked(ctx, events, actions, controlRunID, expectedRunSequence, event)
}

func (s *Store) RunEvents(
	ctx context.Context,
	cursor journal.RunCursor,
	limit int,
) ([]journal.Event, journal.RunCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, cursor, err
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return nil, cursor, err
	}
	if cursor.InstallationID == "" && cursor.SchemaVersion == 0 && cursor.RunSequence == 0 {
		cursor.InstallationID = s.installationID
		cursor.SchemaVersion = journal.SchemaVersion
	}
	if cursor.InstallationID != s.installationID || cursor.SchemaVersion != journal.SchemaVersion ||
		!validID(cursor.ControlRunID) {
		return nil, cursor, journal.ErrCursor
	}
	run := filterRunEvents(events, cursor.ControlRunID)
	if cursor.RunSequence > uint64(len(run)) {
		return nil, cursor, journal.ErrCursor
	}
	limit = journalPageLimit(limit)
	start := int(cursor.RunSequence)
	end := min(start+limit, len(run))
	result := append([]journal.Event(nil), run[start:end]...)
	next := cursor
	if len(result) > 0 {
		next.RunSequence = result[len(result)-1].RunSequence
	}
	return result, next, nil
}

func (s *Store) Feed(
	ctx context.Context,
	cursor journal.GlobalCursor,
	limit int,
) ([]journal.Event, journal.GlobalCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, cursor, err
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return nil, cursor, err
	}
	if cursor == (journal.GlobalCursor{}) {
		cursor = journal.NewGlobalCursor(s.installationID)
	}
	if cursor.InstallationID != s.installationID || cursor.SchemaVersion != journal.SchemaVersion ||
		uint64(cursor.JournalPosition) > uint64(len(events)) {
		return nil, cursor, journal.ErrCursor
	}
	limit = journalPageLimit(limit)
	start := int(cursor.JournalPosition)
	end := min(start+limit, len(events))
	result := append([]journal.Event(nil), events[start:end]...)
	next := cursor
	if len(result) > 0 {
		next.JournalPosition = result[len(result)-1].JournalPosition
	}
	return result, next, nil
}

func (s *Store) Checkpoint(
	ctx context.Context,
	runCursor journal.RunCursor,
	globalCursor journal.GlobalCursor,
	projected []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return err
	}
	if err := s.validateCheckpointCursors(events, runCursor, globalCursor); err != nil {
		return err
	}
	if globalCursor.JournalPosition != journal.JournalPosition(len(events)) ||
		runCursor.RunSequence != runHead(events, runCursor.ControlRunID) {
		return journal.ErrCursor
	}
	if err := s.ensureRunDirectoriesLocked(runCursor.ControlRunID); err != nil {
		return err
	}
	path := s.checkpointPath(runCursor.ControlRunID)
	var previous journalCheckpoint
	if err := readCanonicalJSON(path, &previous); err == nil {
		if runCursor.RunSequence < previous.RunCursor.RunSequence ||
			globalCursor.JournalPosition < previous.GlobalCursor.JournalPosition {
			return journal.ErrCursor
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		// Checkpoints are derived. Corruption is replaced from the journal.
		rebuilt, rebuiltRun, rebuiltGlobal, rebuildErr := s.rebuildRunProjectionLocked(events, runCursor.ControlRunID)
		if rebuildErr != nil {
			return rebuildErr
		}
		previous = journalCheckpoint{
			SchemaVersion: checkpointSchemaVersion, RunCursor: rebuiltRun,
			GlobalCursor: rebuiltGlobal, Projection: rebuilt,
		}
	}
	checkpoint := journalCheckpoint{
		SchemaVersion: checkpointSchemaVersion, RunCursor: runCursor,
		GlobalCursor: globalCursor, Projection: append([]byte(nil), projected...),
	}
	if err := atomicWriteJSON(ctx, path, checkpoint); err != nil {
		return err
	}
	return s.afterBoundary(BoundaryCheckpointWritten)
}

func (s *Store) LoadCheckpoint(
	ctx context.Context,
	controlRunID string,
) ([]byte, journal.RunCursor, journal.GlobalCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, journal.RunCursor{}, journal.GlobalCursor{}, err
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return nil, journal.RunCursor{}, journal.GlobalCursor{}, err
	}
	var checkpoint journalCheckpoint
	path := s.checkpointPath(controlRunID)
	if err := readCanonicalJSON(path, &checkpoint); err == nil &&
		checkpoint.SchemaVersion == checkpointSchemaVersion &&
		s.validateCheckpointCursors(events, checkpoint.RunCursor, checkpoint.GlobalCursor) == nil {
		expected, projectionErr := projection.RebuildRun(
			filterRunEvents(events[:int(checkpoint.GlobalCursor.JournalPosition)], controlRunID),
		)
		if projectionErr == nil && bytes.Equal(expected, checkpoint.Projection) {
			return append([]byte(nil), checkpoint.Projection...), checkpoint.RunCursor, checkpoint.GlobalCursor, nil
		}
	}
	rebuilt, runCursor, globalCursor, err := s.rebuildRunProjectionLocked(events, controlRunID)
	if err != nil {
		return nil, journal.RunCursor{}, journal.GlobalCursor{}, err
	}
	if err := s.ensureRunDirectoriesLocked(controlRunID); err != nil {
		return nil, journal.RunCursor{}, journal.GlobalCursor{}, err
	}
	checkpoint = journalCheckpoint{
		SchemaVersion: checkpointSchemaVersion, RunCursor: runCursor,
		GlobalCursor: globalCursor, Projection: rebuilt,
	}
	if err := atomicWriteJSON(context.Background(), path, checkpoint); err != nil {
		return nil, journal.RunCursor{}, journal.GlobalCursor{}, err
	}
	return rebuilt, runCursor, globalCursor, nil
}

func (s *Store) ActiveRuns(
	ctx context.Context,
	after string,
	limit int,
) ([]string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return nil, "", err
	}
	activeByRun := s.activeRunHeadsLocked(events)
	ids := make([]string, 0, len(activeByRun))
	for id := range activeByRun {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	start := 0
	if after != "" {
		prefix := s.installationID + ":"
		if !strings.HasPrefix(after, prefix) {
			return nil, "", journal.ErrCursor
		}
		last := strings.TrimPrefix(after, prefix)
		if !validID(last) {
			return nil, "", journal.ErrCursor
		}
		index := sort.SearchStrings(ids, last)
		start = index
		if index < len(ids) && ids[index] == last {
			start++
		}
	}
	limit = journalPageLimit(limit)
	end := min(start+limit, len(ids))
	result := append([]string(nil), ids[start:end]...)
	next := ""
	if end < len(ids) && len(result) > 0 {
		next = s.installationID + ":" + result[len(result)-1]
	}
	return result, next, nil
}

func (s *Store) ensureManifestLocked() error {
	path := filepath.Join(s.journalRoot, "manifest.json")
	var current manifest
	if err := readCanonicalJSON(path, &current); err == nil {
		if current.ManifestSchema != manifestSchemaVersion ||
			current.SchemaVersion != journal.SchemaVersion ||
			strings.TrimSpace(current.InstallationID) == "" {
			return fmt.Errorf("%w: invalid journal manifest", controlplane.ErrCorruptStore)
		}
		s.installationID = current.InstallationID
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: invalid journal manifest: %v", controlplane.ErrCorruptStore, err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	current = manifest{
		ManifestSchema: manifestSchemaVersion,
		InstallationID: "installation_" + hex.EncodeToString(random),
		SchemaVersion:  journal.SchemaVersion,
	}
	if err := atomicWriteJSON(context.Background(), path, current); err != nil {
		return err
	}
	s.installationID = current.InstallationID
	return nil
}

func (s *Store) readAuthoritativeCommitsLocked() ([]authoritativeCommit, error) {
	if err := s.ensureCommitsIdentityLocked(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.journalCommits)
	if err != nil {
		return nil, fmt.Errorf("%w: read authoritative commits: %v", controlplane.ErrCorruptStore, err)
	}
	commits := make([]authoritativeCommit, 0, len(entries))
	actionIDs := make(map[string]bool, len(entries))
	keys := make(map[string]bool, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		path := filepath.Join(s.journalCommits, entry.Name())
		if infoErr != nil || entry.IsDir() || !info.Mode().IsRegular() ||
			info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
			info.Size() > int64(maxAuthoritativeCommitRecordBytes) {
			return nil, fmt.Errorf("%w: unsafe authoritative commit entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
		var commit authoritativeCommit
		if err := readCanonicalJSON(path, &commit); err != nil ||
			entry.Name() != authoritativeCommitFilename(commit.Action.ID) ||
			validateAuthoritativeCommit(commit) != nil {
			return nil, fmt.Errorf("%w: invalid authoritative commit %q", controlplane.ErrCorruptStore, entry.Name())
		}
		if actionIDs[commit.Action.ID] || keys[commit.Action.IdempotencyKey] {
			return nil, fmt.Errorf("%w: duplicate authoritative commit identity", controlplane.ErrCorruptStore)
		}
		actionIDs[commit.Action.ID] = true
		keys[commit.Action.IdempotencyKey] = true
		commits = append(commits, commit)
	}
	sort.Slice(commits, func(i, j int) bool {
		return commits[i].Reservation.JournalPosition < commits[j].Reservation.JournalPosition
	})
	return commits, nil
}

func (s *Store) readAuthoritativeStateLocked() (
	[]authoritativeCommit,
	[]journal.Event,
	[]journal.Action,
	error,
) {
	commits, err := s.readAuthoritativeCommitsLocked()
	if err != nil {
		return nil, nil, nil, err
	}
	events, err := s.readJournalEventsWithCommitsLocked(commits)
	if err != nil {
		return nil, nil, nil, err
	}
	actions, err := s.readJournalActionsWithCommitsLocked(commits)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateJournalActionBindings(events, actions); err != nil {
		return nil, nil, nil, fmt.Errorf(
			"%w: invalid authoritative journal action bindings: %v", controlplane.ErrCorruptStore, err,
		)
	}
	if err := validateAuthoritativeCommitMembership(commits, events, actions); err != nil {
		return nil, nil, nil, fmt.Errorf(
			"%w: invalid authoritative commit membership: %v", controlplane.ErrCorruptStore, err,
		)
	}
	return commits, events, actions, nil
}

func validateAuthoritativeCommitMembership(
	commits []authoritativeCommit,
	events []journal.Event,
	actions []journal.Action,
) error {
	for _, commit := range commits {
		actionCount := 0
		reservationCount := 0
		outcomeCount := 0
		for _, action := range actions {
			if action == commit.Action {
				actionCount++
			}
		}
		for _, event := range events {
			if event == commit.Reservation {
				reservationCount++
			}
			if event == commit.Outcome {
				outcomeCount++
			}
		}
		if actionCount != 1 || reservationCount != 1 || outcomeCount != 1 {
			return journal.ErrConflict
		}
	}
	return nil
}

func (s *Store) exactAuthoritativeCommitLocked(
	commits []authoritativeCommit,
	request journal.CommitRequest,
) (authoritativeCommit, bool, error) {
	var matched *authoritativeCommit
	for index := range commits {
		commit := &commits[index]
		if commit.Action.ID != request.Action.ID &&
			commit.Action.IdempotencyKey != request.Action.IdempotencyKey {
			continue
		}
		if matched != nil || commit.Action.ID != request.Action.ID ||
			commit.Action.IdempotencyKey != request.Action.IdempotencyKey {
			return authoritativeCommit{}, true, journal.ErrConflict
		}
		matched = commit
	}
	if matched == nil {
		return authoritativeCommit{}, false, nil
	}
	if err := journal.ValidateCommitRequest(request); err != nil ||
		matched.Action != request.Action ||
		!bytes.Equal(matched.RequestPayload, request.RequestPayload) ||
		!bytes.Equal(matched.OutcomePayload, request.OutcomePayload) ||
		request.ExpectedRun.InstallationID != s.installationID ||
		request.ExpectedGlobal.InstallationID != s.installationID {
		return authoritativeCommit{}, true, journal.ErrConflict
	}
	wantOutcome := matched.Outcome
	wantOutcome.RunSequence = 0
	wantOutcome.JournalPosition = 0
	if wantOutcome != request.Outcome {
		return authoritativeCommit{}, true, journal.ErrConflict
	}
	return *matched, true, nil
}

func validateAuthoritativeCommit(commit authoritativeCommit) error {
	if commit.SchemaVersion != authoritativeCommitSchemaVersion {
		return journal.ErrInvalidRecord
	}
	if err := journal.ValidateAction(commit.Action); err != nil {
		return err
	}
	if err := journal.ValidateEvent(commit.Reservation, true); err != nil {
		return err
	}
	if err := journal.ValidateEvent(commit.Outcome, true); err != nil {
		return err
	}
	if err := journal.ValidatePayload(commit.RequestPayload, commit.Action.CanonicalRequestDigest); err != nil {
		return err
	}
	if err := journal.ValidatePayload(commit.OutcomePayload, commit.Outcome.PayloadDigest); err != nil {
		return err
	}
	actionDigest, err := journal.Digest(commit.Action)
	if err != nil {
		return err
	}
	wantReservation := journal.Event{
		ID:           stableJournalID("reservation", commit.Action.ControlRunID, commit.Action.ID),
		ControlRunID: commit.Action.ControlRunID, RunSequence: commit.Reservation.RunSequence,
		JournalPosition: commit.Reservation.JournalPosition, ActionID: commit.Action.ID,
		Kind: journal.EventActionReserved, PayloadDigest: actionDigest,
		OccurredAt: time.Unix(0, 0).UTC(),
	}
	if commit.Reservation != wantReservation ||
		commit.Outcome.ControlRunID != commit.Action.ControlRunID ||
		commit.Outcome.ActionID != commit.Action.ID ||
		commit.Outcome.RunSequence != commit.Reservation.RunSequence+1 ||
		commit.Outcome.JournalPosition != commit.Reservation.JournalPosition+1 ||
		commit.Outcome.ID == commit.Reservation.ID {
		return journal.ErrInvalidRecord
	}
	switch commit.Outcome.Kind {
	case journal.EventActionResult, journal.EventActionNotPerformed, journal.EventActionAmbiguous:
	default:
		return journal.ErrInvalidRecord
	}
	return nil
}

func (s *Store) auditJournalLocked() error {
	entries, err := os.ReadDir(s.journalRoot)
	if err != nil {
		return fmt.Errorf("%w: read journal root: %v", controlplane.ErrCorruptStore, err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "manifest.json", "migration.json", "events", "payloads", "commits", "commit-staging":
		default:
			return fmt.Errorf("%w: unexpected journal entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
	}
	_, events, actions, err := s.readAuthoritativeStateLocked()
	if err != nil {
		return err
	}
	if _, err := projection.RebuildInstallation(events); err != nil {
		return fmt.Errorf("%w: invalid authoritative feed: %v", controlplane.ErrCorruptStore, err)
	}
	if err := validateJournalActionBindings(events, actions); err != nil {
		return fmt.Errorf("%w: %v", controlplane.ErrCorruptStore, err)
	}
	payloadEntries, err := os.ReadDir(s.journalPayloads)
	if err != nil {
		return fmt.Errorf("%w: read journal payloads: %v", controlplane.ErrCorruptStore, err)
	}
	for _, entry := range payloadEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") ||
			len(strings.TrimSuffix(entry.Name(), ".json")) != 64 {
			return fmt.Errorf("%w: unsafe journal payload entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
		digest := "sha256:" + strings.TrimSuffix(entry.Name(), ".json")
		if _, err := s.readPayloadLocked(digest); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readJournalEventsLocked() ([]journal.Event, error) {
	commits, err := s.readAuthoritativeCommitsLocked()
	if err != nil {
		return nil, err
	}
	return s.readJournalEventsWithCommitsLocked(commits)
}

func (s *Store) readJournalEventsWithCommitsLocked(
	commits []authoritativeCommit,
) ([]journal.Event, error) {
	entries, err := os.ReadDir(s.journalEvents)
	if err != nil {
		return nil, fmt.Errorf("%w: read journal events: %v", controlplane.ErrCorruptStore, err)
	}
	byPosition := make(map[journal.JournalPosition]journal.Event, len(entries))
	add := func(event journal.Event) error {
		if existing, duplicate := byPosition[event.JournalPosition]; duplicate {
			if existing != event {
				return fmt.Errorf("%w: duplicate journal position %d", controlplane.ErrCorruptStore, event.JournalPosition)
			}
			return fmt.Errorf("%w: duplicate authoritative journal event", controlplane.ErrCorruptStore)
		}
		byPosition[event.JournalPosition] = event
		return nil
	}
	for _, entry := range entries {
		path := filepath.Join(s.journalEvents, entry.Name())
		position, parseErr := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if parseErr != nil || entry.Name() != journalEventFilename(journal.JournalPosition(position)) || position == 0 {
			return nil, fmt.Errorf("%w: invalid journal event filename %q", controlplane.ErrCorruptStore, entry.Name())
		}
		var event journal.Event
		if err := readCanonicalJSON(path, &event); err != nil ||
			journal.ValidateEvent(event, true) != nil ||
			event.JournalPosition != journal.JournalPosition(position) {
			return nil, fmt.Errorf("%w: invalid journal event %q", controlplane.ErrCorruptStore, entry.Name())
		}
		if err := add(event); err != nil {
			return nil, err
		}
	}
	for _, commit := range commits {
		if err := add(commit.Reservation); err != nil {
			return nil, err
		}
		if err := add(commit.Outcome); err != nil {
			return nil, err
		}
	}
	events := make([]journal.Event, 0, len(byPosition))
	for position := 1; position <= len(byPosition); position++ {
		event, ok := byPosition[journal.JournalPosition(position)]
		if !ok {
			return nil, fmt.Errorf("%w: noncontiguous journal position %d", controlplane.ErrCorruptStore, position)
		}
		events = append(events, event)
	}
	if _, err := projection.RebuildInstallation(events); err != nil {
		return nil, fmt.Errorf("%w: invalid journal ordering: %v", controlplane.ErrCorruptStore, err)
	}
	return events, nil
}

func (s *Store) readJournalActionsLocked() ([]journal.Action, error) {
	commits, err := s.readAuthoritativeCommitsLocked()
	if err != nil {
		return nil, err
	}
	return s.readJournalActionsWithCommitsLocked(commits)
}

func (s *Store) readJournalActionsWithCommitsLocked(
	commits []authoritativeCommit,
) ([]journal.Action, error) {
	runEntries, err := os.ReadDir(s.runs)
	if err != nil {
		return nil, fmt.Errorf("%w: read journal runs: %v", controlplane.ErrCorruptStore, err)
	}
	var actions []journal.Action
	for _, runEntry := range runEntries {
		info, infoErr := runEntry.Info()
		if infoErr != nil || !runEntry.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: unsafe journal run %q", controlplane.ErrCorruptStore, runEntry.Name())
		}
		directory := filepath.Join(s.runs, runEntry.Name(), "actions")
		actionEntries, readErr := os.ReadDir(directory)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range actionEntries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return nil, fmt.Errorf("%w: unsafe action entry %q", controlplane.ErrCorruptStore, entry.Name())
			}
			var action journal.Action
			if err := readCanonicalJSON(filepath.Join(directory, entry.Name()), &action); err != nil ||
				journal.ValidateAction(action) != nil ||
				entry.Name() != action.ID+".json" ||
				runEntry.Name() != runDirectoryName(action.ControlRunID) {
				return nil, fmt.Errorf("%w: invalid action entry %q", controlplane.ErrCorruptStore, entry.Name())
			}
			actions = append(actions, action)
		}
	}
	for _, commit := range commits {
		actions = append(actions, commit.Action)
	}
	sort.Slice(actions, func(i, j int) bool {
		if actions[i].ControlRunID == actions[j].ControlRunID {
			return actions[i].ID < actions[j].ID
		}
		return actions[i].ControlRunID < actions[j].ControlRunID
	})
	return actions, nil
}

func (s *Store) appendJournalLocked(
	ctx context.Context,
	events []journal.Event,
	actions []journal.Action,
	controlRunID string,
	expectedRunSequence uint64,
	event journal.Event,
) (journal.Event, error) {
	if event.ControlRunID != controlRunID || !validID(controlRunID) || !validID(event.ID) {
		return journal.Event{}, journal.ErrConflict
	}
	for _, existing := range events {
		if existing.ID != event.ID {
			continue
		}
		candidate := existing
		candidate.RunSequence = 0
		candidate.JournalPosition = 0
		if candidate != event {
			return journal.Event{}, journal.ErrConflict
		}
		return existing, nil
	}
	if err := journal.ValidateEvent(event, false); err != nil {
		return journal.Event{}, err
	}
	if runHead(events, controlRunID) != expectedRunSequence {
		return journal.Event{}, journal.ErrConflict
	}
	if event.ActionID != "" {
		var bound *journal.Action
		for index := range actions {
			if actions[index].ID == event.ActionID {
				if bound != nil || actions[index].ControlRunID != controlRunID {
					return journal.Event{}, journal.ErrConflict
				}
				bound = &actions[index]
			}
		}
		if bound == nil {
			return journal.Event{}, journal.ErrConflict
		}
		if journal.IsOutcome(event.Kind) && !hasPriorReservation(events, event.ControlRunID, event.ActionID) {
			return journal.Event{}, journal.ErrConflict
		}
		if err := journal.ValidateOutcomeTransition(events, event); err != nil {
			return journal.Event{}, err
		}
	}
	event.RunSequence = expectedRunSequence + 1
	event.JournalPosition = journal.JournalPosition(len(events) + 1)
	if err := journal.ValidateEvent(event, true); err != nil {
		return journal.Event{}, err
	}
	if err := s.afterBoundary(BoundaryGlobalPosition); err != nil {
		return journal.Event{}, err
	}
	if err := createJSONExclusive(
		ctx, filepath.Join(s.journalEvents, journalEventFilename(event.JournalPosition)), event,
	); err != nil {
		return journal.Event{}, err
	}
	if err := syncDirectory(s.journalEvents); err != nil {
		return journal.Event{}, err
	}
	if err := s.afterBoundary(BoundaryCanonicalVisible); err != nil {
		return journal.Event{}, err
	}
	if err := s.afterBoundary(BoundaryEventCommitted); err != nil {
		return journal.Event{}, err
	}
	if journal.IsOutcome(event.Kind) {
		if err := s.afterBoundary(BoundaryResultAppended); err != nil {
			return journal.Event{}, err
		}
	}
	if err := s.ensureRunDirectoriesLocked(controlRunID); err != nil {
		return journal.Event{}, err
	}
	indexPath := filepath.Join(s.eventIndexDirectory(controlRunID), runEventFilename(event.RunSequence))
	if err := createJSONExclusive(context.Background(), indexPath, event); err != nil {
		return journal.Event{}, err
	}
	if err := syncDirectory(s.eventIndexDirectory(controlRunID)); err != nil {
		return journal.Event{}, err
	}
	if err := s.afterBoundary(BoundaryRunIndexRepaired); err != nil {
		return journal.Event{}, err
	}
	active := activeRun{
		ControlRunID: controlRunID, RunSequence: event.RunSequence,
		JournalPosition: event.JournalPosition,
	}
	if err := atomicWriteJSON(context.Background(), s.activePath(controlRunID), active); err != nil {
		return journal.Event{}, err
	}
	if err := s.afterBoundary(BoundaryActiveIndexUpdated); err != nil {
		return journal.Event{}, err
	}
	if err := s.afterBoundary(BoundaryResponse); err != nil {
		return journal.Event{}, err
	}
	return event, nil
}

func (s *Store) persistSnapshotEventLocked(ctx context.Context, tx transaction) error {
	digest, err := s.persistPayloadLocked(ctx, tx.Next)
	if err != nil {
		return err
	}
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return err
	}
	actions, err := s.readJournalActionsLocked()
	if err != nil {
		return err
	}
	if err := validateJournalActionBindings(events, actions); err != nil {
		return err
	}
	occurred := tx.Next.Run.UpdatedAt
	if occurred.IsZero() {
		occurred = time.Unix(0, 0).UTC()
	}
	event := journal.Event{
		ID:           stableJournalID("snapshot", tx.Next.Run.ID, fmt.Sprintf("%d", tx.Next.Version)),
		ControlRunID: tx.Next.Run.ID, ActionID: tx.EventActionID, Kind: tx.EventKind,
		PayloadDigest: digest, OccurredAt: occurred,
	}
	_, err = s.appendJournalLocked(
		ctx, events, actions, tx.Next.Run.ID, runHead(events, tx.Next.Run.ID), event,
	)
	if err == nil && tx.Next.Run.Status == controlplane.StatusClosed {
		if removeErr := os.Remove(s.activePath(tx.Next.Run.ID)); removeErr != nil &&
			!errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
	}
	return err
}

func (s *Store) preflightTransactionOutcomeLocked(tx transaction) error {
	if tx.EventActionID == "" {
		return nil
	}
	reservation, err := s.reservationLocked(tx.ControlRunID, tx.EventActionID)
	if err != nil {
		return err
	}
	return controlplane.ValidateOutcomeReservation(
		tx.Next, tx.ExpectedVersion, tx.EventActionID, reservation,
	)
}

func validateJournalActionBindings(events []journal.Event, actions []journal.Action) error {
	byID := make(map[string]journal.Action, len(actions))
	byKey := make(map[string]string, len(actions))
	for _, action := range actions {
		if _, duplicate := byID[action.ID]; duplicate {
			return journal.ErrConflict
		}
		if existingID, duplicate := byKey[action.IdempotencyKey]; duplicate && existingID != action.ID {
			return journal.ErrConflict
		}
		byID[action.ID] = action
		byKey[action.IdempotencyKey] = action.ID
	}
	reservations := make(map[string]bool, len(actions))
	prior := make([]journal.Event, 0, len(events))
	for _, event := range events {
		if event.ActionID == "" {
			prior = append(prior, event)
			continue
		}
		action, ok := byID[event.ActionID]
		if !ok || action.ControlRunID != event.ControlRunID {
			return journal.ErrConflict
		}
		if event.Kind == journal.EventActionReserved || event.Kind == journal.EventMigrationAction {
			if reservations[event.ActionID] {
				return journal.ErrConflict
			}
			reservations[event.ActionID] = true
		}
		if event.Kind == journal.EventActionReserved {
			digest, err := journal.Digest(action)
			if err != nil || digest != event.PayloadDigest {
				return journal.ErrConflict
			}
		}
		if journal.IsOutcome(event.Kind) {
			if !reservations[event.ActionID] || journal.ValidateOutcomeTransition(prior, event) != nil {
				return journal.ErrConflict
			}
		}
		prior = append(prior, event)
	}
	return nil
}

func hasPriorReservation(events []journal.Event, controlRunID, actionID string) bool {
	for _, event := range events {
		if event.ControlRunID == controlRunID && event.ActionID == actionID &&
			(event.Kind == journal.EventActionReserved || event.Kind == journal.EventMigrationAction) {
			return true
		}
	}
	return false
}

func (s *Store) migrateSnapshotsLocked() error {
	markerPath := filepath.Join(s.journalRoot, "migration.json")
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return err
	}
	if exists, err := s.validateExistingMigrationMarkerLocked(); err != nil {
		return err
	} else if exists {
		return nil
	}
	entries, err := os.ReadDir(s.records)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var snapshots []controlplane.Snapshot
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		snapshot, loadErr := s.loadLocked(id)
		if loadErr != nil {
			return loadErr
		}
		snapshots = append(snapshots, snapshot)
	}
	if len(snapshots) > 0 {
		for _, snapshot := range snapshots {
			if err := s.migrateSnapshotLocked(snapshot); err != nil {
				return err
			}
		}
		events, err = s.readJournalEventsLocked()
		if err != nil {
			return err
		}
	}
	digests := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		digest, digestErr := journal.Digest(snapshot)
		if digestErr != nil {
			return digestErr
		}
		digests = append(digests, snapshot.Run.ID+"="+digest)
	}
	snapshotDigest, err := journal.Digest(digests)
	if err != nil {
		return err
	}
	position := journal.JournalPosition(0)
	if len(snapshots) > 0 {
		position = journal.JournalPosition(len(events))
	}
	marker := migrationMarker{
		SchemaVersion:   migrationSchemaVersion,
		JournalPosition: position,
		SnapshotDigest:  snapshotDigest,
	}
	if err := s.validateMigrationMarkerLocked(marker, events); err != nil {
		return err
	}
	return atomicWriteJSON(context.Background(), markerPath, marker)
}

func (s *Store) validateExistingMigrationMarkerLocked() (bool, error) {
	var marker migrationMarker
	if err := readCanonicalJSON(filepath.Join(s.journalRoot, "migration.json"), &marker); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: invalid migration marker", controlplane.ErrCorruptStore)
	}
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return false, err
	}
	if err := s.validateMigrationMarkerLocked(marker, events); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) validateMigrationMarkerLocked(
	marker migrationMarker,
	events []journal.Event,
) error {
	invalid := func() error {
		return fmt.Errorf("%w: migration receipt is not bound to authoritative history", controlplane.ErrCorruptStore)
	}
	if marker.SchemaVersion != migrationSchemaVersion ||
		!journal.ValidDigest(marker.SnapshotDigest) ||
		uint64(marker.JournalPosition) > uint64(len(events)) {
		return invalid()
	}
	emptyDigest, err := journal.Digest([]string{})
	if err != nil {
		return err
	}
	if marker.JournalPosition == 0 {
		if marker.SnapshotDigest != emptyDigest {
			return invalid()
		}
		for _, event := range events {
			if migrationEventKind(event.Kind) {
				return invalid()
			}
		}
		return s.validateRecordJournalCoverageLocked(events, nil)
	}

	prefix := events[:int(marker.JournalPosition)]
	if prefix[len(prefix)-1].Kind != journal.EventMigrationCompleted {
		return invalid()
	}
	for _, event := range events[int(marker.JournalPosition):] {
		if migrationEventKind(event.Kind) {
			return invalid()
		}
	}
	runs := make(map[string]migrationReceiptRun)
	batchStarted := false
	activeRunID := ""
	lastStartedRunID := ""
	for _, event := range prefix {
		if !migrationEventKind(event.Kind) {
			if batchStarted && (activeRunID == "" ||
				(event.Kind != journal.EventActionResult && event.Kind != journal.EventActionAmbiguous) ||
				event.ControlRunID != activeRunID || event.ActionID == "") {
				return invalid()
			}
			continue
		}
		if !batchStarted {
			if event.Kind != journal.EventMigrationStarted {
				return invalid()
			}
			batchStarted = true
		}
		state := runs[event.ControlRunID]
		switch event.Kind {
		case journal.EventMigrationStarted:
			if state.started || activeRunID != "" ||
				lastStartedRunID != "" && event.ControlRunID <= lastStartedRunID {
				return invalid()
			}
			state.started = true
			activeRunID = event.ControlRunID
			lastStartedRunID = event.ControlRunID
		case journal.EventMigrationSnapshot:
			if !state.started || state.snapshot.Run.ID != "" || state.completed ||
				activeRunID != event.ControlRunID {
				return invalid()
			}
			encoded, readErr := s.readPayloadLocked(event.PayloadDigest)
			if readErr != nil || journal.DecodeStrict(encoded, &state.snapshot) != nil ||
				controlplane.ValidateSnapshot(state.snapshot) != nil ||
				state.snapshot.Run.ID != event.ControlRunID {
				return invalid()
			}
			state.digest, err = journal.Digest(state.snapshot)
			if err != nil || state.digest != event.PayloadDigest {
				return invalid()
			}
		case journal.EventMigrationCompleted:
			if !state.started || state.snapshot.Run.ID == "" || state.completed ||
				activeRunID != event.ControlRunID {
				return invalid()
			}
			encoded, readErr := s.readPayloadLocked(event.PayloadDigest)
			var completion migrationCompletion
			if readErr != nil || journal.DecodeStrict(encoded, &completion) != nil ||
				completion.Version != state.snapshot.Version {
				return invalid()
			}
			state.completed = true
			activeRunID = ""
		default:
			if !state.started || state.snapshot.Run.ID != "" || state.completed ||
				activeRunID != event.ControlRunID {
				return invalid()
			}
		}
		runs[event.ControlRunID] = state
	}
	if !batchStarted || activeRunID != "" || len(runs) == 0 {
		return invalid()
	}
	runIDs := make([]string, 0, len(runs))
	for runID, state := range runs {
		if !state.started || state.snapshot.Run.ID == "" || !state.completed {
			return invalid()
		}
		runIDs = append(runIDs, runID)
	}
	sort.Strings(runIDs)
	digests := make([]string, 0, len(runIDs))
	for _, runID := range runIDs {
		digests = append(digests, runID+"="+runs[runID].digest)
	}
	snapshotDigest, err := journal.Digest(digests)
	if err != nil {
		return err
	}
	if snapshotDigest != marker.SnapshotDigest {
		return invalid()
	}
	return s.validateRecordJournalCoverageLocked(events, runs)
}

func (s *Store) validateRecordJournalCoverageLocked(
	events []journal.Event,
	migrated map[string]migrationReceiptRun,
) error {
	represented := make(map[string]bool, len(migrated))
	for runID := range migrated {
		represented[runID] = true
	}
	for _, event := range events {
		if event.Kind != journal.EventProjectionUpdated &&
			event.Kind != journal.EventActionResult && event.Kind != journal.EventActionAmbiguous {
			continue
		}
		encoded, err := s.readPayloadLocked(event.PayloadDigest)
		if err != nil {
			continue
		}
		var snapshot controlplane.Snapshot
		if journal.DecodeStrict(encoded, &snapshot) == nil &&
			controlplane.ValidateSnapshot(snapshot) == nil && snapshot.Run.ID == event.ControlRunID {
			represented[event.ControlRunID] = true
		}
	}
	entries, err := os.ReadDir(s.records)
	if err != nil {
		return fmt.Errorf("%w: read records for migration receipt: %v", controlplane.ErrCorruptStore, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("%w: unsafe record while validating migration receipt", controlplane.ErrCorruptStore)
		}
		info, infoErr := entry.Info()
		id := strings.TrimSuffix(entry.Name(), ".json")
		if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
			!validID(id) || !represented[id] {
			return fmt.Errorf("%w: record %q is absent from authoritative history", controlplane.ErrCorruptStore, id)
		}
	}
	return nil
}

func migrationEventKind(kind journal.EventKind) bool {
	switch kind {
	case journal.EventMigrationStarted, journal.EventMigrationGraph,
		journal.EventMigrationAttempt, journal.EventMigrationSession,
		journal.EventMigrationAction, journal.EventMigrationEvidence,
		journal.EventMigrationMessage, journal.EventMigrationCallback,
		journal.EventMigrationHandoff, journal.EventMigrationClose,
		journal.EventMigrationSnapshot, journal.EventMigrationCompleted:
		return true
	default:
		return false
	}
}

func (s *Store) migrateSnapshotLocked(snapshot controlplane.Snapshot) error {
	type fact struct {
		kind     journal.EventKind
		id       string
		actionID string
		value    any
	}
	facts := []fact{{
		kind: journal.EventMigrationStarted, id: "started", value: struct {
			ControlRunID string `json:"control_run_id"`
		}{snapshot.Run.ID},
	}, {
		kind: journal.EventMigrationGraph, id: "graph", value: snapshot.Graph,
	}}
	appendMap := func(kind journal.EventKind, values map[string]any) {
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			facts = append(facts, fact{kind: kind, id: key, value: values[key]})
		}
	}
	attempts := make(map[string]any, len(snapshot.Attempts))
	for id, value := range snapshot.Attempts {
		attempts[id] = value
	}
	appendMap(journal.EventMigrationAttempt, attempts)
	sessions := make(map[string]any, len(snapshot.Sessions))
	for id, value := range snapshot.Sessions {
		sessions[id] = value
	}
	appendMap(journal.EventMigrationSession, sessions)
	actionIDs := make([]string, 0, len(snapshot.Actions))
	for id := range snapshot.Actions {
		actionIDs = append(actionIDs, id)
	}
	sort.Strings(actionIDs)
	for _, id := range actionIDs {
		value := snapshot.Actions[id]
		if err := s.writeMigratedActionLocked(snapshot, value); err != nil {
			return err
		}
		facts = append(facts, fact{
			kind: journal.EventMigrationAction, id: id, actionID: id, value: value,
		})
		switch {
		case value.Completed:
			facts = append(facts, fact{
				kind: journal.EventActionResult, id: id + "-result",
				actionID: id, value: value,
			})
		case value.Ambiguous:
			facts = append(facts, fact{
				kind: journal.EventActionAmbiguous, id: id + "-ambiguous",
				actionID: id, value: value,
			})
		}
	}
	evidence := make(map[string]any, len(snapshot.Evidence))
	for id, value := range snapshot.Evidence {
		evidence[id] = value
	}
	appendMap(journal.EventMigrationEvidence, evidence)
	messages := make(map[string]any, len(snapshot.Messages))
	for id, value := range snapshot.Messages {
		messages[id] = value
	}
	appendMap(journal.EventMigrationMessage, messages)
	callbacks := make(map[string]any, len(snapshot.Callbacks))
	for id, value := range snapshot.Callbacks {
		callbacks[id] = value
	}
	appendMap(journal.EventMigrationCallback, callbacks)
	handoffs := make(map[string]any, len(snapshot.Handoffs))
	for id, value := range snapshot.Handoffs {
		handoffs[id] = value
	}
	appendMap(journal.EventMigrationHandoff, handoffs)
	facts = append(facts, fact{kind: journal.EventMigrationClose, id: "close", value: snapshot.Run.Close})
	facts = append(facts, fact{kind: journal.EventMigrationSnapshot, id: "snapshot", value: snapshot})
	facts = append(facts, fact{kind: journal.EventMigrationCompleted, id: "completed", value: struct {
		Version uint64 `json:"version"`
	}{snapshot.Version}})
	occurred := snapshot.Run.CreatedAt
	if occurred.IsZero() {
		occurred = time.Unix(0, 0).UTC()
	}
	for _, current := range facts {
		digest, err := s.persistPayloadLocked(context.Background(), current.value)
		if err != nil {
			return err
		}
		events, err := s.readJournalEventsLocked()
		if err != nil {
			return err
		}
		event := journal.Event{
			ID:           stableJournalID("migration", snapshot.Run.ID, string(current.kind), current.id),
			ControlRunID: snapshot.Run.ID, ActionID: current.actionID, Kind: current.kind,
			PayloadDigest: digest, OccurredAt: occurred,
		}
		actions, err := s.readJournalActionsLocked()
		if err != nil {
			return err
		}
		if _, err := s.appendJournalLocked(
			context.Background(), events, actions, snapshot.Run.ID,
			runHead(events, snapshot.Run.ID), event,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeMigratedActionLocked(
	snapshot controlplane.Snapshot,
	legacy controlplane.LifecycleAction,
) error {
	attempt, ok := snapshot.Attempts[legacy.AttemptID]
	if !ok {
		return journal.ErrInvalidRecord
	}
	actionKind, err := controlplane.JournalActionKind(legacy.Kind)
	if err != nil {
		return err
	}
	requestDigest := legacy.RequestDigest
	if !journal.ValidDigest(requestDigest) {
		var err error
		requestDigest, err = journal.Digest(legacy.RequestDigest)
		if err != nil {
			return err
		}
	}
	action := journal.Action{
		ID: legacy.ID, ControlRunID: snapshot.Run.ID, TaskID: attempt.TaskID,
		AttemptID: legacy.AttemptID, Kind: actionKind,
		GraphRevision: snapshot.Graph.Revision, ExpectedProjection: snapshot.Version,
		CanonicalRequestDigest: requestDigest, IdempotencyKey: legacy.ID,
	}
	if err := journal.ValidateAction(action); err != nil || !validID(action.ID) {
		if err != nil {
			return err
		}
		return journal.ErrInvalidRecord
	}
	if err := s.ensureRunDirectoriesLocked(snapshot.Run.ID); err != nil {
		return err
	}
	path := filepath.Join(s.actionDirectory(snapshot.Run.ID), action.ID+".json")
	var existing journal.Action
	if err := readCanonicalJSON(path, &existing); err == nil {
		if existing != action {
			return journal.ErrConflict
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return createJSONExclusive(context.Background(), path, action)
}

func (s *Store) repairSnapshotCheckpointsLocked() error {
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return err
	}
	latest := make(map[string]journal.Event)
	for _, event := range events {
		if event.Kind == journal.EventMigrationSnapshot {
			latest[event.ControlRunID] = event
			continue
		}
		if event.Kind == journal.EventProjectionUpdated ||
			event.Kind == journal.EventActionResult || event.Kind == journal.EventActionAmbiguous {
			var snapshot controlplane.Snapshot
			if encoded, readErr := os.ReadFile(s.payloadPath(event.PayloadDigest)); readErr == nil &&
				json.Unmarshal(encoded, &snapshot) == nil && snapshot.Run.ID == event.ControlRunID {
				latest[event.ControlRunID] = event
			}
		}
	}
	for runID, event := range latest {
		encoded, err := s.readPayloadLocked(event.PayloadDigest)
		if err != nil {
			return fmt.Errorf("%w: missing snapshot payload for %q", controlplane.ErrCorruptStore, runID)
		}
		var snapshot controlplane.Snapshot
		if err := json.Unmarshal(encoded, &snapshot); err != nil {
			return fmt.Errorf("%w: invalid snapshot payload for %q", controlplane.ErrCorruptStore, runID)
		}
		normalized, err := journal.Digest(snapshot)
		if err != nil || normalized != event.PayloadDigest {
			return fmt.Errorf("%w: snapshot payload digest mismatch for %q", controlplane.ErrCorruptStore, runID)
		}
		if err := controlplane.ValidateSnapshot(snapshot); err != nil {
			return fmt.Errorf("%w: invalid snapshot projection for %q: %v", controlplane.ErrCorruptStore, runID, err)
		}
		path := s.recordPath(runID)
		var current controlplane.Snapshot
		if err := readCanonicalJSON(path, &current); err == nil && reflect.DeepEqual(current, snapshot) {
			continue
		}
		if err := atomicWriteJSON(context.Background(), path, snapshot); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) repairJournalDerivedLocked() error {
	events, err := s.readJournalEventsLocked()
	if err != nil {
		return err
	}
	actions, err := s.readJournalActionsLocked()
	if err != nil {
		return err
	}
	for _, action := range actions {
		reserved := false
		for _, event := range events {
			if event.ActionID == action.ID &&
				(event.Kind == journal.EventActionReserved || event.Kind == journal.EventMigrationAction) {
				reserved = true
				break
			}
		}
		if reserved {
			continue
		}
		if err := s.ensureReservationEventLocked(context.Background(), actions, action); err != nil {
			return err
		}
		events, err = s.readJournalEventsLocked()
		if err != nil {
			return err
		}
	}
	for _, event := range events {
		if err := s.ensureRunDirectoriesLocked(event.ControlRunID); err != nil {
			return err
		}
		path := filepath.Join(s.eventIndexDirectory(event.ControlRunID), runEventFilename(event.RunSequence))
		var indexed journal.Event
		if err := readCanonicalJSON(path, &indexed); err != nil || indexed != event {
			if err := atomicWriteJSON(context.Background(), path, event); err != nil {
				return err
			}
		}
	}
	activeEntries, err := os.ReadDir(s.active)
	if err != nil {
		return err
	}
	for _, entry := range activeEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("%w: unsafe active index entry %q", controlplane.ErrCorruptStore, entry.Name())
		}
		if err := os.Remove(filepath.Join(s.active, entry.Name())); err != nil {
			return err
		}
	}
	for runID, active := range s.activeRunHeadsLocked(events) {
		if err := atomicWriteJSON(context.Background(), s.activePath(runID), active); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) activeRunHeadsLocked(events []journal.Event) map[string]activeRun {
	heads := make(map[string]activeRun)
	closed := make(map[string]bool)
	for _, event := range events {
		heads[event.ControlRunID] = activeRun{
			ControlRunID: event.ControlRunID, RunSequence: event.RunSequence,
			JournalPosition: event.JournalPosition,
		}
		encoded, err := s.readPayloadLocked(event.PayloadDigest)
		if err != nil {
			continue
		}
		var snapshot controlplane.Snapshot
		if json.Unmarshal(encoded, &snapshot) == nil && snapshot.Run.ID == event.ControlRunID {
			closed[event.ControlRunID] = snapshot.Run.Status == controlplane.StatusClosed
		}
	}
	for runID, terminal := range closed {
		if terminal {
			delete(heads, runID)
		}
	}
	return heads
}

func (s *Store) persistPayloadLocked(ctx context.Context, value any) (string, error) {
	digest, err := journal.Digest(value)
	if err != nil {
		return "", err
	}
	path := s.payloadPath(digest)
	encoded, err := journal.CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	if existing, err := s.readPayloadLocked(digest); err == nil {
		if !bytes.Equal(existing, encoded) {
			return "", fmt.Errorf("%w: payload digest collision", controlplane.ErrCorruptStore)
		}
		return digest, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := createJSONExclusive(ctx, path, value); err != nil {
		return "", err
	}
	if err := syncDirectory(s.journalPayloads); err != nil {
		return "", err
	}
	return digest, nil
}

func (s *Store) readPayloadLocked(digest string) ([]byte, error) {
	if !journal.ValidDigest(digest) {
		return nil, journal.ErrInvalidRecord
	}
	path := s.payloadPath(digest)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%w: unsafe journal payload", controlplane.ErrCorruptStore)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("%w: journal payload digest mismatch", controlplane.ErrCorruptStore)
	}
	return encoded, nil
}

func (s *Store) validateCheckpointCursors(
	events []journal.Event,
	runCursor journal.RunCursor,
	globalCursor journal.GlobalCursor,
) error {
	if runCursor.InstallationID != s.installationID ||
		globalCursor.InstallationID != s.installationID ||
		runCursor.SchemaVersion != journal.SchemaVersion ||
		globalCursor.SchemaVersion != journal.SchemaVersion ||
		runCursor.ControlRunID == "" ||
		uint64(globalCursor.JournalPosition) > uint64(len(events)) {
		return journal.ErrCursor
	}
	run := filterRunEvents(events[:int(globalCursor.JournalPosition)], runCursor.ControlRunID)
	if runCursor.RunSequence > uint64(len(run)) {
		return journal.ErrCursor
	}
	return nil
}

func (s *Store) rebuildRunProjectionLocked(
	events []journal.Event,
	controlRunID string,
) ([]byte, journal.RunCursor, journal.GlobalCursor, error) {
	run := filterRunEvents(events, controlRunID)
	if len(run) == 0 {
		return nil, journal.RunCursor{}, journal.GlobalCursor{}, journal.ErrNotFound
	}
	projected, err := projection.RebuildRun(run)
	if err != nil {
		return nil, journal.RunCursor{}, journal.GlobalCursor{}, err
	}
	if err := s.afterBoundary(BoundaryProjectionRebuilt); err != nil {
		return nil, journal.RunCursor{}, journal.GlobalCursor{}, err
	}
	runCursor := journal.RunCursor{
		InstallationID: s.installationID, ControlRunID: controlRunID,
		SchemaVersion: journal.SchemaVersion, RunSequence: uint64(len(run)),
	}
	globalCursor := journal.GlobalCursor{
		InstallationID: s.installationID, SchemaVersion: journal.SchemaVersion,
		JournalPosition: journal.JournalPosition(len(events)),
	}
	return projected, runCursor, globalCursor, nil
}

func (s *Store) ensureRunDirectoriesLocked(controlRunID string) error {
	root := s.runDirectory(controlRunID)
	for _, directory := range []string{root, s.actionDirectory(controlRunID), s.eventIndexDirectory(controlRunID)} {
		if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: unsafe run directory", controlplane.ErrCorruptStore)
		}
	}
	return syncDirectory(s.runs)
}

func (s *Store) runDirectory(controlRunID string) string {
	return filepath.Join(s.runs, runDirectoryName(controlRunID))
}

func (s *Store) actionDirectory(controlRunID string) string {
	return filepath.Join(s.runDirectory(controlRunID), "actions")
}

func (s *Store) eventIndexDirectory(controlRunID string) string {
	return filepath.Join(s.runDirectory(controlRunID), "event-index")
}

func (s *Store) checkpointPath(controlRunID string) string {
	return filepath.Join(s.runDirectory(controlRunID), "checkpoint.json")
}

func (s *Store) activePath(controlRunID string) string {
	return filepath.Join(s.active, runDirectoryName(controlRunID)+".json")
}

func (s *Store) payloadPath(digest string) string {
	return filepath.Join(s.journalPayloads, strings.TrimPrefix(digest, "sha256:")+".json")
}

func runDirectoryName(controlRunID string) string {
	sum := sha256.Sum256([]byte(controlRunID))
	return hex.EncodeToString(sum[:])
}

func stableJournalID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{prefix}, values...), "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func authoritativeCommitFilename(actionID string) string {
	sum := sha256.Sum256([]byte(actionID))
	return hex.EncodeToString(sum[:]) + ".json"
}

func canonicalJournalEvent(event journal.Event) (journal.Event, error) {
	encoded, err := journal.CanonicalJSON(event)
	if err != nil {
		return journal.Event{}, err
	}
	var canonical journal.Event
	if err := journal.DecodeStrict(encoded, &canonical); err != nil {
		return journal.Event{}, err
	}
	return canonical, nil
}

func validAuthoritativeCommitStagingFilename(name string) bool {
	const prefix = ".commit-"
	const suffix = ".tmp"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	separator := strings.LastIndexByte(stem, '-')
	if separator != sha256.Size*2 || len(stem)-separator-1 < 1 || len(stem)-separator-1 > 10 {
		return false
	}
	for _, character := range stem[:separator] {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	for _, character := range stem[separator+1:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func journalEventFilename(position journal.JournalPosition) string {
	return fmt.Sprintf("%020d.json", position)
}

func runEventFilename(sequence uint64) string {
	return fmt.Sprintf("%020d.json", sequence)
}

func runHead(events []journal.Event, controlRunID string) uint64 {
	var head uint64
	for _, event := range events {
		if event.ControlRunID == controlRunID {
			head = event.RunSequence
		}
	}
	return head
}

func filterRunEvents(events []journal.Event, controlRunID string) []journal.Event {
	result := make([]journal.Event, 0)
	for _, event := range events {
		if event.ControlRunID == controlRunID {
			result = append(result, event)
		}
	}
	return result
}

func snapshotEventBinding(
	current, next controlplane.Snapshot,
) (journal.EventKind, string, error) {
	kind := journal.EventProjectionUpdated
	actionID := ""
	ids := make([]string, 0, len(next.Actions))
	for id := range next.Actions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		nextAction := next.Actions[id]
		previous := current.Actions[id]
		candidate := journal.EventKind("")
		switch {
		case !previous.Completed && nextAction.Completed:
			candidate = journal.EventActionResult
		case !previous.Ambiguous && nextAction.Ambiguous:
			candidate = journal.EventActionAmbiguous
		}
		if candidate == "" {
			continue
		}
		if actionID != "" {
			return "", "", fmt.Errorf("%w: save introduces multiple terminal action outcomes", controlplane.ErrInvalidRecord)
		}
		kind, actionID = candidate, id
	}
	return kind, actionID, nil
}

func validTransactionBinding(tx transaction) bool {
	switch tx.EventKind {
	case journal.EventProjectionUpdated:
		if tx.EventActionID != "" {
			return false
		}
	case journal.EventActionResult, journal.EventActionAmbiguous:
		if tx.Create || tx.EventActionID == "" || tx.Action != nil {
			return false
		}
		terminal, ok := tx.Next.Actions[tx.EventActionID]
		if !ok || tx.EventKind == journal.EventActionResult && !terminal.Completed ||
			tx.EventKind == journal.EventActionAmbiguous && !terminal.Ambiguous {
			return false
		}
	default:
		return false
	}
	if tx.Action == nil {
		return true
	}
	if tx.Create || journal.ValidateAction(*tx.Action) != nil ||
		tx.Action.ControlRunID != tx.ControlRunID ||
		tx.Action.ExpectedProjection != tx.ExpectedVersion {
		return false
	}
	prepared, ok := tx.Next.Actions[tx.Action.ID]
	preparedKind, err := controlplane.JournalActionKind(prepared.Kind)
	return ok && prepared.ID == tx.Action.ID && prepared.AttemptID == tx.Action.AttemptID &&
		prepared.RequestDigest == tx.Action.CanonicalRequestDigest &&
		err == nil && preparedKind == tx.Action.Kind
}

func exactReservation(
	actions []journal.Action,
	action journal.Action,
) (journal.Action, bool, error) {
	for _, existing := range actions {
		if existing.ID != action.ID && existing.IdempotencyKey != action.IdempotencyKey {
			continue
		}
		if existing != action {
			return journal.Action{}, false, journal.ErrConflict
		}
		return existing, true, nil
	}
	return journal.Action{}, false, nil
}

func actionsWith(actions []journal.Action, action journal.Action) []journal.Action {
	result := append([]journal.Action(nil), actions...)
	return append(result, action)
}

func journalPageLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func readCanonicalJSON(path string, destination any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: unsafe canonical file %q", controlplane.ErrCorruptStore, path)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := journal.DecodeStrict(encoded, destination); err != nil {
		return err
	}
	canonical, err := journal.CanonicalJSON(destination)
	if err != nil {
		return err
	}
	if !bytes.Equal(encoded, canonical) {
		return fmt.Errorf("%w: noncanonical JSON %q", controlplane.ErrCorruptStore, path)
	}
	return nil
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
