// Package runfilesystem persists durable run records on a local filesystem.
package runfilesystem

import (
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

	"github.com/araihu/paje/internal/run"
)

var rootLocks sync.Map

type Store struct {
	root            string
	runsDirectory   string
	indexDirectory  string
	transactionLock *sync.Mutex
}

var _ run.Store = (*Store)(nil)

type binding struct {
	RunID     string `json:"run_id"`
	InputHash string `json:"input_hash"`
}

func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("create run store: root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("create run store: resolve root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create run store root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("create run store: canonicalize root: %w", err)
	}
	runsDirectory := filepath.Join(canonical, "runs")
	indexDirectory := filepath.Join(canonical, "idempotency")
	for _, directory := range []string{runsDirectory, indexDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create run store directory: %w", err)
		}
	}
	lock, _ := rootLocks.LoadOrStore(canonical, &sync.Mutex{})
	return &Store{
		root: canonical, runsDirectory: runsDirectory, indexDirectory: indexDirectory,
		transactionLock: lock.(*sync.Mutex),
	}, nil
}

func (s *Store) Reserve(ctx context.Context, reservation run.Reservation) (run.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return run.Record{}, false, err
	}
	if err := run.ValidateReservation(reservation); err != nil {
		return run.Record{}, false, err
	}

	s.transactionLock.Lock()
	defer s.transactionLock.Unlock()
	if err := ctx.Err(); err != nil {
		return run.Record{}, false, err
	}

	var indexPath string
	if reservation.IdempotencyKey != "" {
		indexPath = s.indexPath(reservation.Template.String(), reservation.IdempotencyKey)
		var existing binding
		err := readJSON(indexPath, &existing)
		switch {
		case err == nil:
			if existing.InputHash != reservation.InputHash {
				return run.Record{}, false, run.ErrIdempotencyConflict
			}
			record, loadErr := s.loadLocked(existing.RunID)
			if loadErr != nil {
				return run.Record{}, false, fmt.Errorf("reserve bound run: %w", loadErr)
			}
			if record.Template != reservation.Template || record.IdempotencyKey != reservation.IdempotencyKey {
				return run.Record{}, false, fmt.Errorf("reserve bound run: %w", run.ErrIdempotencyConflict)
			}
			return record, false, nil
		case !errors.Is(err, os.ErrNotExist):
			return run.Record{}, false, fmt.Errorf("reserve read idempotency binding: %w", err)
		}
	}

	record, err := run.NewRecord(reservation)
	if err != nil {
		return run.Record{}, false, err
	}
	runPath := s.runPath(record.ID)
	if _, err := os.Lstat(runPath); err == nil {
		return run.Record{}, false, run.ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return run.Record{}, false, fmt.Errorf("reserve inspect run: %w", err)
	}
	if err := atomicWriteJSON(ctx, runPath, record); err != nil {
		return run.Record{}, false, fmt.Errorf("reserve write run: %w", err)
	}
	if indexPath != "" {
		if err := atomicWriteJSON(ctx, indexPath, binding{RunID: record.ID, InputHash: record.InputHash}); err != nil {
			removeErr := removeAndSync(runPath)
			if removeErr != nil {
				return run.Record{}, false, fmt.Errorf("reserve write binding: %v; rollback run: %w", err, removeErr)
			}
			return run.Record{}, false, fmt.Errorf("reserve write binding: %w", err)
		}
	}
	return run.CloneRecord(record), true, nil
}

func (s *Store) Load(ctx context.Context, id string) (run.Record, error) {
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}
	if !validRunID(id) {
		return run.Record{}, run.ErrNotFound
	}
	s.transactionLock.Lock()
	defer s.transactionLock.Unlock()
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}
	return s.loadLocked(id)
}

func (s *Store) Save(ctx context.Context, next run.Record, expectedVersion uint64) (run.Record, error) {
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}
	if !validRunID(next.ID) {
		return run.Record{}, run.ErrNotFound
	}
	s.transactionLock.Lock()
	defer s.transactionLock.Unlock()
	if err := ctx.Err(); err != nil {
		return run.Record{}, err
	}

	current, err := s.loadLocked(next.ID)
	if err != nil {
		return run.Record{}, err
	}
	if current.Version != expectedVersion {
		return run.Record{}, run.ErrVersionConflict
	}
	saved, err := run.PrepareSave(current, next)
	if err != nil {
		return run.Record{}, err
	}
	if err := atomicWriteJSON(ctx, s.runPath(saved.ID), saved); err != nil {
		return run.Record{}, fmt.Errorf("save run: %w", err)
	}
	return run.CloneRecord(saved), nil
}

func (s *Store) loadLocked(id string) (run.Record, error) {
	var record run.Record
	if err := readJSON(s.runPath(id), &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return run.Record{}, run.ErrNotFound
		}
		return run.Record{}, fmt.Errorf("load run: %w", err)
	}
	if record.ID != id {
		return run.Record{}, fmt.Errorf("load run: %w: file identity mismatch", run.ErrInvalidRecord)
	}
	if err := run.Validate(record); err != nil {
		return run.Record{}, fmt.Errorf("load run: %w", err)
	}
	return run.CloneRecord(record), nil
}

func (s *Store) runPath(id string) string {
	return filepath.Join(s.runsDirectory, id+".json")
}

func (s *Store) indexPath(templateID, key string) string {
	sum := sha256.Sum256([]byte(templateID + "\x00" + key))
	return filepath.Join(s.indexDirectory, hex.EncodeToString(sum[:])+".json")
}

func atomicWriteJSON(ctx context.Context, target string, value any) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode canonical JSON: %w", err)
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(target)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	open := true
	defer func() {
		var closeErr error
		if open {
			closeErr = temporary.Close()
		}
		removeErr := os.Remove(temporaryPath)
		if returnErr == nil && closeErr != nil {
			returnErr = closeErr
		}
		if returnErr == nil && removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			returnErr = removeErr
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary permissions: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	open = false
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync containing directory: %w", err)
	}
	return nil
}

func readJSON(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("trailing JSON data")
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

func removeAndSync(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func validRunID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for index, character := range id {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || index > 0 && (character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}
