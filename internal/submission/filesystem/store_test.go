package filesystem_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/submission"
	submissionfilesystem "github.com/araihu/paje/internal/submission/filesystem"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

func validReservation() submission.Reservation {
	createdAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	return submission.Reservation{
		IdempotencyKey: strings.Repeat("a", 32),
		Record: submission.Record{
			RunID:                "paje_abc",
			CredentialID:         "cred-codex-service",
			RequestDigest:        strings.Repeat("1", 64),
			IdempotencyKeyDigest: strings.Repeat("2", 64),
			Template:             templatecodechange.ID,
			CanonicalInput:       json.RawMessage(`{"task_description":"change timeout"}`),
			Origin: submission.Origin{
				Harness: "codex", SessionID: "session-1", TurnID: "turn-1",
			},
			RootRunID: "paje_abc",
			Depth:     0,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
	}
}

func newStore(t *testing.T, root string) *submissionfilesystem.Store {
	t.Helper()
	store, err := submissionfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestFilesystemStorePersistsReservationAndTriggerAcrossRestart(t *testing.T) {
	root := t.TempDir()
	reservation := validReservation()
	store := newStore(t, root)
	first, owned, err := store.Reserve(context.Background(), reservation)
	if err != nil || !owned {
		t.Fatalf("Reserve() = (%#v, %v, %v), want owner", first, owned, err)
	}
	beforeBinding := newStore(t, root)
	reusedBeforeBinding, owned, err := beforeBinding.Reserve(context.Background(), reservation)
	if err != nil || owned || reusedBeforeBinding.Trigger != nil {
		t.Fatalf("pre-bind restart Reserve() = (%#v, %v, %v), want unbound reuse", reusedBeforeBinding, owned, err)
	}
	reference := submission.TriggerReference{Provider: "hatchet", ExternalRunID: "run-1"}
	if _, err := beforeBinding.BindTrigger(context.Background(), first.RunID, reference); err != nil {
		t.Fatal(err)
	}

	restarted := newStore(t, root)
	loaded, err := restarted.Load(context.Background(), first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Trigger == nil || *loaded.Trigger != reference {
		t.Fatalf("restarted Load() trigger = %#v, want %#v", loaded.Trigger, reference)
	}
	reused, owned, err := restarted.Reserve(context.Background(), reservation)
	if err != nil || owned || reused.Trigger == nil || *reused.Trigger != reference {
		t.Fatalf("restarted Reserve() = (%#v, %v, %v), want bound reuse", reused, owned, err)
	}
}

func TestFilesystemStoreRecoversRecordWrittenBeforeBinding(t *testing.T) {
	root := t.TempDir()
	reservation := validReservation()
	store := newStore(t, root)
	if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	bindingPath := onlyJSONFile(t, filepath.Join(root, "idempotency"))
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}

	restarted := newStore(t, root)
	changed := reservation
	changed.Record.RequestDigest = strings.Repeat("f", 64)
	if _, _, err := restarted.Reserve(context.Background(), changed); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("changed Reserve() error = %v, want idempotency conflict", err)
	}
	recovered, owned, err := restarted.Reserve(context.Background(), reservation)
	if err != nil || !owned || recovered.RunID != reservation.Record.RunID {
		t.Fatalf("recovery Reserve() = (%#v, %v, %v), want recovered owner", recovered, owned, err)
	}
	fullyRestarted := newStore(t, root)
	_, owned, err = fullyRestarted.Reserve(context.Background(), reservation)
	if err != nil || owned {
		t.Fatalf("post-recovery Reserve() = (_, %v, %v), want reuse", owned, err)
	}
}

func TestFilesystemStoreRejectsSecondKeyForExistingRunWithoutCorruptingStore(t *testing.T) {
	root := t.TempDir()
	store := newStore(t, root)
	first := validReservation()
	if _, _, err := store.Reserve(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.IdempotencyKey = strings.Repeat("b", 32)
	if _, _, err := store.Reserve(context.Background(), second); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("second-key Reserve() error = %v, want idempotency conflict", err)
	}
	if _, err := submissionfilesystem.New(root); err != nil {
		t.Fatalf("store was corrupted by rejected second key: %v", err)
	}
}

func TestFilesystemStoreRejectsSecondKeyForTombstonedRunWithoutCorruptingStore(t *testing.T) {
	root := t.TempDir()
	store := newStore(t, root)
	first := validReservation()
	if _, _, err := store.Reserve(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(onlyJSONFile(t, filepath.Join(root, "records"))); err != nil {
		t.Fatal(err)
	}

	second := first
	second.IdempotencyKey = strings.Repeat("b", 32)
	if _, _, err := store.Reserve(context.Background(), second); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("second-key tombstone Reserve() error = %v, want idempotency conflict", err)
	}
	original, err := store.LoadByKey(context.Background(), first.Record.CredentialID, first.IdempotencyKey)
	if err != nil || original.RunID != first.Record.RunID || original.RequestDigest != first.Record.RequestDigest {
		t.Fatalf("original LoadByKey() = (%#v, %v), want authoritative tombstone", original, err)
	}
	if _, err := store.LoadByKey(context.Background(), second.Record.CredentialID, second.IdempotencyKey); !errors.Is(err, submission.ErrNotFound) {
		t.Fatalf("second-key LoadByKey() error = %v, want not found", err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "idempotency")); err != nil || len(entries) != 1 {
		t.Fatalf("idempotency entries = %d, %v, want original binding only", len(entries), err)
	}
	if _, err := submissionfilesystem.New(root); err != nil {
		t.Fatalf("store was corrupted by rejected tombstone key: %v", err)
	}
}

func TestFilesystemStoreBindingTombstoneSurvivesRecordPruning(t *testing.T) {
	root := t.TempDir()
	reservation := validReservation()
	store := newStore(t, root)
	if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(onlyJSONFile(t, filepath.Join(root, "records"))); err != nil {
		t.Fatal(err)
	}

	restarted := newStore(t, root)
	tombstone, err := restarted.LoadByKey(
		context.Background(),
		reservation.Record.CredentialID,
		reservation.IdempotencyKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.RunID != reservation.Record.RunID ||
		tombstone.CredentialID != reservation.Record.CredentialID ||
		tombstone.RequestDigest != reservation.Record.RequestDigest {
		t.Fatalf("LoadByKey() tombstone = %#v", tombstone)
	}
	if _, _, err := restarted.Reserve(context.Background(), reservation); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("exact tombstone Reserve() error = %v, want immutable-key conflict", err)
	}
	changed := reservation
	changed.Record.RequestDigest = strings.Repeat("f", 64)
	if _, _, err := restarted.Reserve(context.Background(), changed); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("changed Reserve() error = %v, want immutable-key conflict", err)
	}
}

func TestFilesystemStoreRestartRemovesAbandonedAtomicTemporaryFile(t *testing.T) {
	root := t.TempDir()
	store := newStore(t, root)
	if _, _, err := store.Reserve(context.Background(), validReservation()); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(root, "records", ".paje_abc.json.12345.tmp")
	writeFile(t, temporary, []byte("partial"))
	if _, err := submissionfilesystem.New(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned temporary still exists: %v", err)
	}
}

func TestFilesystemStoreRejectsUnsafeRoot(t *testing.T) {
	if _, err := submissionfilesystem.New("  "); err == nil {
		t.Fatal("New() accepted blank root")
	}
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(parent, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := submissionfilesystem.New(linkRoot); !errors.Is(err, submission.ErrCorruptStore) {
		t.Fatalf("New(symlink) error = %v, want corrupt store", err)
	}
}

func TestFilesystemStoreRejectsAdversarialEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "corrupt JSON",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, onlyJSONFile(t, filepath.Join(root, "records")), []byte("{\n"))
			},
		},
		{
			name: "duplicate logical record",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				record := onlyJSONFile(t, filepath.Join(root, "records"))
				encoded, err := os.ReadFile(record)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(root, "records", "paje_duplicate.json"), encoded)
			},
		},
		{
			name: "directory in records",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, "records", "paje_directory.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink in records",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				target := onlyJSONFile(t, filepath.Join(root, "records"))
				if err := os.Symlink(target, filepath.Join(root, "records", "paje_symlink.json")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "noncanonical record JSON",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				path := onlyJSONFile(t, filepath.Join(root, "records"))
				encoded, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, path, append([]byte(" "), encoded...))
			},
		},
		{
			name: "unknown binding field",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				path := onlyJSONFile(t, filepath.Join(root, "idempotency"))
				encoded, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				encoded = []byte(strings.TrimSuffix(string(encoded), "}\n") + `,"unknown":true}` + "\n")
				writeFile(t, path, encoded)
			},
		},
		{
			name: "record and binding disagree",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				path := onlyJSONFile(t, filepath.Join(root, "idempotency"))
				var document map[string]any
				encoded, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(encoded, &document); err != nil {
					t.Fatal(err)
				}
				document["request_digest"] = strings.Repeat("f", 64)
				encoded, err = json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, path, append(encoded, '\n'))
			},
		},
		{
			name: "unexpected root entry",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, filepath.Join(root, "unexpected"), []byte("no\n"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := newStore(t, root)
			if _, _, err := store.Reserve(context.Background(), validReservation()); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, root)
			if _, err := submissionfilesystem.New(root); !errors.Is(err, submission.ErrCorruptStore) {
				t.Fatalf("New() error = %v, want corrupt store", err)
			}
		})
	}
}

func TestFilesystemStoreDetectsRootSymlinkAfterConstruction(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "store")
	store := newStore(t, root)
	reservation := validReservation()
	if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), reservation.Record.RunID); !errors.Is(err, submission.ErrCorruptStore) {
		t.Fatalf("Load() error = %v, want corrupt store", err)
	}
}

func TestFilesystemStoreDetectsOrdinaryRootReplacementAfterConstruction(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "store")
	store := newStore(t, root)
	reservation := validReservation()
	if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"records", "idempotency"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Load(context.Background(), reservation.Record.RunID); !errors.Is(err, submission.ErrCorruptStore) {
		t.Fatalf("Load() after ordinary root replacement error = %v, want corrupt store", err)
	}
	if _, _, err := store.Reserve(context.Background(), reservation); !errors.Is(err, submission.ErrCorruptStore) {
		t.Fatalf("Reserve() after ordinary root replacement error = %v, want corrupt store", err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "records")); err != nil || len(entries) != 0 {
		t.Fatalf("replacement records entries = %d, %v, want no mutation", len(entries), err)
	}
}

func TestFilesystemStoreDetectsDirectoryReplacementAfterConstruction(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "store")
	store := newStore(t, root)
	reservation := validReservation()
	if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(root, "records")
	if err := os.Rename(records, filepath.Join(parent, "records-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(records, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), reservation.Record.RunID); !errors.Is(err, submission.ErrCorruptStore) {
		t.Fatalf("Load() after records replacement error = %v, want corrupt store", err)
	}
}

func TestFilesystemStoreUsesPrivateCanonicalFiles(t *testing.T) {
	root := t.TempDir()
	store := newStore(t, root)
	if _, _, err := store.Reserve(context.Background(), validReservation()); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Join(root, "records"), filepath.Join(root, "idempotency")} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", directory, info.Mode().Perm())
		}
	}
	for _, directory := range []string{filepath.Join(root, "records"), filepath.Join(root, "idempotency")} {
		path := onlyJSONFile(t, directory)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, info.Mode().Perm())
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
			t.Fatalf("%s is not newline-terminated canonical JSON", path)
		}
		if strings.Contains(string(encoded), validReservation().IdempotencyKey) {
			t.Fatalf("%s retained the clear idempotency key", path)
		}
	}
}

func TestFilesystemStoreRejectsNonMonotonicInitialCancellation(t *testing.T) {
	store := newStore(t, t.TempDir())
	reservation := validReservation()
	if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	_, err := store.MarkCancellationRequested(
		context.Background(),
		reservation.Record.RunID,
		reservation.Record.UpdatedAt.Add(-time.Second),
	)
	if !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("MarkCancellationRequested() error = %v, want idempotency conflict", err)
	}
}

func TestFilesystemStoreRejectsUnknownTemplateRecord(t *testing.T) {
	store := newStore(t, t.TempDir())
	reservation := validReservation()
	reservation.Record.Template = template.ID{Name: "other", Version: 1}
	if _, _, err := store.Reserve(context.Background(), reservation); !errors.Is(err, submission.ErrCorruptStore) {
		t.Fatalf("Reserve() error = %v, want corrupt store", err)
	}
}

func onlyJSONFile(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("%s entries = %#v, want one JSON file", directory, entries)
	}
	return filepath.Join(directory, entries[0].Name())
}

func writeFile(t *testing.T, path string, encoded []byte) {
	t.Helper()
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
