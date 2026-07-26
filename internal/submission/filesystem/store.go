// Package filesystem persists deterministic leaf-submission reservations for
// the v1 single-replica installation.
package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/submission"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

var rootLocks sync.Map

// Store is the process-local compare-and-swap owner for one durable root.
type Store struct {
	root                string
	records             string
	idempotency         string
	rootIdentity        os.FileInfo
	recordsIdentity     os.FileInfo
	idempotencyIdentity os.FileInfo
	lock                *sync.Mutex
}

type binding struct {
	CredentialID  string `json:"credential_id"`
	KeyDigest     string `json:"key_digest"`
	RunID         string `json:"run_id"`
	RequestDigest string `json:"request_digest"`
}

var _ submission.Store = (*Store)(nil)

// New opens or creates one durable single-writer submission store.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("create submission store: root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("create submission store: resolve root: %w", err)
	}
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, corrupt("store root is a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("create submission store: inspect root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create submission store root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("create submission store: canonicalize root: %w", err)
	}
	if err := requireSafeDirectory(canonical); err != nil {
		return nil, corrupt("store root is unsafe")
	}
	records := filepath.Join(canonical, "records")
	idempotency := filepath.Join(canonical, "idempotency")
	for _, directory := range []string{records, idempotency} {
		created := false
		if err := os.Mkdir(directory, 0o700); err == nil {
			created = true
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create submission store directory: %w", err)
		}
		if err := requirePrivateDirectory(directory); err != nil {
			return nil, corrupt("store directory is unsafe")
		}
		if created {
			if err := syncDirectory(canonical); err != nil {
				return nil, fmt.Errorf("sync submission store root: %w", err)
			}
		}
	}
	lockValue, _ := rootLocks.LoadOrStore(canonical, &sync.Mutex{})
	rootIdentity, err := os.Lstat(canonical)
	if err != nil {
		return nil, fmt.Errorf("inspect submission store root identity: %w", err)
	}
	recordsIdentity, err := os.Lstat(records)
	if err != nil {
		return nil, fmt.Errorf("inspect submission records identity: %w", err)
	}
	idempotencyIdentity, err := os.Lstat(idempotency)
	if err != nil {
		return nil, fmt.Errorf("inspect submission idempotency identity: %w", err)
	}
	store := &Store{
		root:                canonical,
		records:             records,
		idempotency:         idempotency,
		rootIdentity:        rootIdentity,
		recordsIdentity:     recordsIdentity,
		idempotencyIdentity: idempotencyIdentity,
		lock:                lockValue.(*sync.Mutex),
	}
	store.lock.Lock()
	defer store.lock.Unlock()
	if err := store.ensureRootLocked(); err != nil {
		return nil, err
	}
	if err := store.recoverTemporaryFilesLocked(); err != nil {
		return nil, err
	}
	if err := store.auditLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

// Reserve durably binds one credential-scoped key before it can own a trigger.
func (s *Store) Reserve(
	ctx context.Context,
	reservation submission.Reservation,
) (submission.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, false, err
	}
	if err := validateReservation(reservation); err != nil {
		return submission.Record{}, false, err
	}
	expected := cloneRecord(reservation.Record)
	keyDigest := digestKey(expected.CredentialID, reservation.IdempotencyKey)
	expectedBinding := binding{
		CredentialID: expected.CredentialID,
		KeyDigest:    keyDigest, RunID: expected.RunID,
		RequestDigest: expected.RequestDigest,
	}

	s.lock.Lock()
	defer s.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return submission.Record{}, false, err
	}
	if err := s.prepareLocked(); err != nil {
		return submission.Record{}, false, err
	}

	bound, bindingExists, err := s.loadBindingIfExistsLocked(keyDigest)
	if err != nil {
		return submission.Record{}, false, err
	}
	if bindingExists {
		if bound != expectedBinding {
			return submission.Record{}, false, submission.ErrIdempotencyConflict
		}
		current, exists, err := s.loadRecordIfExistsLocked(expected.RunID)
		if err != nil {
			return submission.Record{}, false, err
		}
		if !exists {
			return tombstoneRecord(bound), false, submission.ErrIdempotencyConflict
		}
		if !sameReservationBinding(current, expected) {
			return cloneRecord(current), false, submission.ErrIdempotencyConflict
		}
		return cloneRecord(current), false, nil
	}
	if bound, alreadyBound, err := s.findBindingForRunLocked(expected.RunID); err != nil {
		return submission.Record{}, false, err
	} else if alreadyBound {
		return tombstoneRecord(bound), false, submission.ErrIdempotencyConflict
	}

	current, recordExists, err := s.loadRecordIfExistsLocked(expected.RunID)
	if err != nil {
		return submission.Record{}, false, err
	}
	owner := true
	if recordExists {
		if !sameReservationBinding(current, expected) {
			return cloneRecord(current), false, submission.ErrIdempotencyConflict
		}
		owner = current.Trigger == nil && current.CancellationRequested == nil
	} else {
		if err := atomicWriteJSON(ctx, s.recordPath(expected.RunID), expected); err != nil {
			return submission.Record{}, false, fmt.Errorf("persist submission record: %w", err)
		}
		current, _, err = s.loadRecordIfExistsLocked(expected.RunID)
		if err != nil {
			return submission.Record{}, false, err
		}
	}
	if err := atomicWriteJSON(ctx, s.bindingPath(keyDigest), expectedBinding); err != nil {
		return submission.Record{}, false, fmt.Errorf("persist submission binding: %w", err)
	}
	if err := s.auditLocked(); err != nil {
		return submission.Record{}, false, err
	}
	return cloneRecord(current), owner, nil
}

// BindTrigger records the first exact provider reference and rejects drift.
func (s *Store) BindTrigger(
	ctx context.Context,
	runID string,
	reference submission.TriggerReference,
) (submission.Record, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	if !validID(runID) || !safeRequired(reference.Provider) || !safeRequired(reference.ExternalRunID) {
		return submission.Record{}, submission.ErrCorruptStore
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	if err := s.prepareLocked(); err != nil {
		return submission.Record{}, err
	}
	record, exists, err := s.loadRecordIfExistsLocked(runID)
	if err != nil {
		return submission.Record{}, err
	}
	if !exists {
		return submission.Record{}, submission.ErrNotFound
	}
	if _, err := s.bindingForRunLocked(runID); err != nil {
		return submission.Record{}, err
	}
	if record.Trigger != nil {
		if *record.Trigger != reference {
			return cloneRecord(record), submission.ErrIdempotencyConflict
		}
		return cloneRecord(record), nil
	}
	value := reference
	record.Trigger = &value
	if err := atomicWriteJSON(ctx, s.recordPath(runID), record); err != nil {
		return submission.Record{}, fmt.Errorf("persist submission trigger: %w", err)
	}
	if err := s.auditLocked(); err != nil {
		return submission.Record{}, err
	}
	return cloneRecord(record), nil
}

// Load returns one complete, currently bound durable record.
func (s *Store) Load(ctx context.Context, runID string) (submission.Record, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	if !validID(runID) {
		return submission.Record{}, submission.ErrNotFound
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := s.prepareLocked(); err != nil {
		return submission.Record{}, err
	}
	record, exists, err := s.loadRecordIfExistsLocked(runID)
	if err != nil {
		return submission.Record{}, err
	}
	if !exists {
		return submission.Record{}, submission.ErrNotFound
	}
	if _, err := s.bindingForRunLocked(runID); err != nil {
		return submission.Record{}, err
	}
	return cloneRecord(record), nil
}

// LoadByKey returns the complete record or its immutable binding tombstone.
func (s *Store) LoadByKey(
	ctx context.Context,
	credentialID string,
	key string,
) (submission.Record, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	if !safeRequired(credentialID) || !validKey(key) {
		return submission.Record{}, submission.ErrNotFound
	}
	keyDigest := digestKey(credentialID, key)
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := s.prepareLocked(); err != nil {
		return submission.Record{}, err
	}
	bound, exists, err := s.loadBindingIfExistsLocked(keyDigest)
	if err != nil {
		return submission.Record{}, err
	}
	if !exists || bound.CredentialID != credentialID || bound.KeyDigest != keyDigest {
		return submission.Record{}, submission.ErrNotFound
	}
	record, recordExists, err := s.loadRecordIfExistsLocked(bound.RunID)
	if err != nil {
		return submission.Record{}, err
	}
	if !recordExists {
		return tombstoneRecord(bound), nil
	}
	return cloneRecord(record), nil
}

// MarkCancellationRequested records the first monotonic cancellation time.
func (s *Store) MarkCancellationRequested(
	ctx context.Context,
	runID string,
	at time.Time,
) (submission.Record, error) {
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	if !validID(runID) {
		return submission.Record{}, submission.ErrNotFound
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return submission.Record{}, err
	}
	if err := s.prepareLocked(); err != nil {
		return submission.Record{}, err
	}
	record, exists, err := s.loadRecordIfExistsLocked(runID)
	if err != nil {
		return submission.Record{}, err
	}
	if !exists {
		return submission.Record{}, submission.ErrNotFound
	}
	if _, err := s.bindingForRunLocked(runID); err != nil {
		return submission.Record{}, err
	}
	if record.CancellationRequested != nil {
		return cloneRecord(record), nil
	}
	if at.IsZero() || at.Before(record.UpdatedAt) {
		return cloneRecord(record), submission.ErrIdempotencyConflict
	}
	value := at
	record.CancellationRequested = &value
	record.UpdatedAt = at
	if err := atomicWriteJSON(ctx, s.recordPath(runID), record); err != nil {
		return submission.Record{}, fmt.Errorf("persist submission cancellation: %w", err)
	}
	if err := s.auditLocked(); err != nil {
		return submission.Record{}, err
	}
	return cloneRecord(record), nil
}

func (s *Store) prepareLocked() error {
	if err := s.ensureRootLocked(); err != nil {
		return err
	}
	return s.auditLocked()
}

func (s *Store) ensureRootLocked() error {
	if err := requireSameDirectory(s.root, s.rootIdentity, false); err != nil {
		return corrupt("store root changed after construction")
	}
	if err := requireSameDirectory(s.records, s.recordsIdentity, true); err != nil {
		return corrupt("records directory is unsafe")
	}
	if err := requireSameDirectory(s.idempotency, s.idempotencyIdentity, true); err != nil {
		return corrupt("idempotency directory is unsafe")
	}
	return nil
}

func requireSameDirectory(path string, expected os.FileInfo, private bool) error {
	if err := requireSafeDirectory(path); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if private && current.Mode().Perm()&0o077 != 0 {
		return errors.New("directory is not private")
	}
	if !os.SameFile(expected, current) {
		return errors.New("directory identity changed")
	}
	return nil
}

func (s *Store) recoverTemporaryFilesLocked() error {
	for _, directory := range []string{s.records, s.idempotency} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return corrupt("read store directory")
		}
		removed := false
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".tmp") {
				continue
			}
			if !validTemporaryName(entry.Name(), directory == s.idempotency) {
				return corrupt("unexpected temporary file")
			}
			path := filepath.Join(directory, entry.Name())
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
				return corrupt("unsafe temporary file")
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("recover submission temporary file: %w", err)
			}
			removed = true
		}
		if removed {
			if err := syncDirectory(directory); err != nil {
				return fmt.Errorf("sync recovered submission directory: %w", err)
			}
		}
	}
	return nil
}

func (s *Store) auditLocked() error {
	rootEntries, err := os.ReadDir(s.root)
	if err != nil {
		return corrupt("read store root")
	}
	for _, entry := range rootEntries {
		if entry.Name() != "records" && entry.Name() != "idempotency" {
			return corrupt("unexpected root entry")
		}
	}

	recordEntries, err := os.ReadDir(s.records)
	if err != nil {
		return corrupt("read records")
	}
	records := make(map[string]submission.Record, len(recordEntries))
	for _, entry := range recordEntries {
		if !validRecordFilename(entry.Name()) {
			return corrupt("invalid record filename")
		}
		var record submission.Record
		if err := readCanonicalJSON(filepath.Join(s.records, entry.Name()), &record); err != nil {
			return corrupt("invalid record file")
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if record.RunID != id || validateRecord(record) != nil {
			return corrupt("invalid durable record")
		}
		if _, duplicate := records[record.RunID]; duplicate {
			return corrupt("duplicate logical record")
		}
		records[record.RunID] = record
	}

	bindingEntries, err := os.ReadDir(s.idempotency)
	if err != nil {
		return corrupt("read idempotency bindings")
	}
	bindingsByRun := make(map[string]binding, len(bindingEntries))
	for _, entry := range bindingEntries {
		if !validBindingFilename(entry.Name()) {
			return corrupt("invalid binding filename")
		}
		var bound binding
		if err := readCanonicalJSON(filepath.Join(s.idempotency, entry.Name()), &bound); err != nil {
			return corrupt("invalid binding file")
		}
		filenameDigest := strings.TrimSuffix(entry.Name(), ".json")
		if validateBinding(bound) != nil || bound.KeyDigest != filenameDigest {
			return corrupt("invalid durable binding")
		}
		if _, duplicate := bindingsByRun[bound.RunID]; duplicate {
			return corrupt("duplicate logical binding")
		}
		bindingsByRun[bound.RunID] = bound
		if record, exists := records[bound.RunID]; exists &&
			(record.CredentialID != bound.CredentialID || record.RequestDigest != bound.RequestDigest) {
			return corrupt("record and binding disagree")
		}
	}

	for _, record := range records {
		if record.Depth == 0 {
			continue
		}
		parent, exists := records[record.Origin.ParentRunID]
		if !exists || parent.CredentialID != record.CredentialID ||
			parent.Origin.Harness != record.Origin.Harness ||
			parent.Depth+1 != record.Depth || parent.RootRunID != record.RootRunID {
			return corrupt("record lineage disagrees")
		}
		root, exists := records[record.RootRunID]
		if !exists || root.Depth != 0 || root.RootRunID != root.RunID ||
			root.CredentialID != record.CredentialID || root.Origin.Harness != record.Origin.Harness {
			return corrupt("record root lineage disagrees")
		}
	}
	return nil
}

func (s *Store) loadRecordIfExistsLocked(runID string) (submission.Record, bool, error) {
	var record submission.Record
	err := readCanonicalJSON(s.recordPath(runID), &record)
	if errors.Is(err, os.ErrNotExist) {
		return submission.Record{}, false, nil
	}
	if err != nil || record.RunID != runID || validateRecord(record) != nil {
		return submission.Record{}, false, corrupt("invalid durable record")
	}
	return record, true, nil
}

func (s *Store) loadBindingIfExistsLocked(keyDigest string) (binding, bool, error) {
	var bound binding
	err := readCanonicalJSON(s.bindingPath(keyDigest), &bound)
	if errors.Is(err, os.ErrNotExist) {
		return binding{}, false, nil
	}
	if err != nil || validateBinding(bound) != nil || bound.KeyDigest != keyDigest {
		return binding{}, false, corrupt("invalid durable binding")
	}
	return bound, true, nil
}

func (s *Store) bindingForRunLocked(runID string) (binding, error) {
	result, found, err := s.findBindingForRunLocked(runID)
	if err != nil {
		return binding{}, err
	}
	if !found {
		return binding{}, corrupt("record has no reservation binding")
	}
	return result, nil
}

func (s *Store) findBindingForRunLocked(runID string) (binding, bool, error) {
	entries, err := os.ReadDir(s.idempotency)
	if err != nil {
		return binding{}, false, corrupt("read idempotency bindings")
	}
	found := false
	var result binding
	for _, entry := range entries {
		var candidate binding
		if err := readCanonicalJSON(filepath.Join(s.idempotency, entry.Name()), &candidate); err != nil {
			return binding{}, false, corrupt("invalid durable binding")
		}
		if candidate.RunID != runID {
			continue
		}
		if found {
			return binding{}, false, corrupt("duplicate logical binding")
		}
		found = true
		result = candidate
	}
	return result, found, nil
}

func validateReservation(reservation submission.Reservation) error {
	if !validKey(reservation.IdempotencyKey) || validateRecord(reservation.Record) != nil ||
		reservation.Record.Trigger != nil || reservation.Record.CancellationRequested != nil ||
		!reservation.Record.CreatedAt.Equal(reservation.Record.UpdatedAt) {
		return submission.ErrCorruptStore
	}
	return nil
}

func validateRecord(record submission.Record) error {
	canonicalInput, err := run.CanonicalInput(record.CanonicalInput)
	if err != nil {
		return err
	}
	switch {
	case !validID(record.RunID), !safeRequired(record.CredentialID):
		return errors.New("record identity is invalid")
	case !validDigest(record.RequestDigest), !validDigest(record.IdempotencyKeyDigest):
		return errors.New("record digest is invalid")
	case record.Template != templatecodechange.ID:
		return errors.New("record template is invalid")
	case !bytes.Equal(record.CanonicalInput, canonicalInput):
		return errors.New("record input is not canonical")
	case !safeRequired(record.Origin.Harness), !safeRequired(record.Origin.SessionID), !safeRequired(record.Origin.TurnID):
		return errors.New("record origin is invalid")
	case !validID(record.RootRunID), record.Depth < 0 || record.Depth > 1:
		return errors.New("record lineage is invalid")
	case record.Depth == 0 && (record.RootRunID != record.RunID || record.Origin.ParentRunID != ""):
		return errors.New("root lineage is invalid")
	case record.Depth > 0 && (!validID(record.Origin.ParentRunID) || record.RootRunID == record.RunID):
		return errors.New("child lineage is invalid")
	case record.CreatedAt.IsZero(), record.UpdatedAt.IsZero(), record.UpdatedAt.Before(record.CreatedAt):
		return errors.New("record timestamps are invalid")
	case record.CancellationRequested != nil &&
		(record.CancellationRequested.Before(record.CreatedAt) || record.CancellationRequested.After(record.UpdatedAt)):
		return errors.New("record cancellation timestamp is invalid")
	}
	if record.Trigger != nil &&
		(!safeRequired(record.Trigger.Provider) || !safeRequired(record.Trigger.ExternalRunID)) {
		return errors.New("record trigger is invalid")
	}
	return nil
}

func validateBinding(bound binding) error {
	if !safeRequired(bound.CredentialID) || !validDigest(bound.KeyDigest) ||
		!validID(bound.RunID) || !validDigest(bound.RequestDigest) {
		return errors.New("binding is invalid")
	}
	return nil
}

func sameReservationBinding(record, expected submission.Record) bool {
	return record.RunID == expected.RunID &&
		record.CredentialID == expected.CredentialID &&
		record.RequestDigest == expected.RequestDigest &&
		record.IdempotencyKeyDigest == expected.IdempotencyKeyDigest &&
		record.Template == expected.Template &&
		bytes.Equal(record.CanonicalInput, expected.CanonicalInput) &&
		record.Origin == expected.Origin &&
		record.RootRunID == expected.RootRunID &&
		record.Depth == expected.Depth
}

func tombstoneRecord(bound binding) submission.Record {
	return submission.Record{
		RunID: bound.RunID, CredentialID: bound.CredentialID,
		RequestDigest: bound.RequestDigest,
	}
}

func digestKey(credentialID, key string) string {
	sum := sha256.Sum256([]byte(credentialID + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !('0' <= character && character <= '9') && !('a' <= character && character <= 'f') {
			return false
		}
	}
	return true
}

func validKey(key string) bool {
	if len(key) < 16 || len(key) > 128 || strings.TrimSpace(key) != key {
		return false
	}
	for _, character := range key {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func safeRequired(value string) bool {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
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

func validRecordFilename(name string) bool {
	return strings.HasSuffix(name, ".json") && validID(strings.TrimSuffix(name, ".json"))
}

func validBindingFilename(name string) bool {
	return strings.HasSuffix(name, ".json") && validDigest(strings.TrimSuffix(name, ".json"))
}

func validTemporaryName(name string, isBinding bool) bool {
	if !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".tmp") {
		return false
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp")
	separator := strings.LastIndexByte(stem, '.')
	if separator < 1 || separator == len(stem)-1 {
		return false
	}
	for _, character := range stem[separator+1:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	target := stem[:separator]
	if isBinding {
		return validBindingFilename(target)
	}
	return validRecordFilename(target)
}

func (s *Store) recordPath(runID string) string {
	return filepath.Join(s.records, runID+".json")
}

func (s *Store) bindingPath(keyDigest string) string {
	return filepath.Join(s.idempotency, keyDigest+".json")
}

func cloneRecord(source submission.Record) submission.Record {
	cloned := source
	cloned.CanonicalInput = append(json.RawMessage(nil), source.CanonicalInput...)
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

func corrupt(message string) error {
	return fmt.Errorf("%w: %s", submission.ErrCorruptStore, message)
}
