package filesystem_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane"
	"github.com/araihu/paje/internal/controlplane/filesystem"
	"github.com/araihu/paje/internal/controlplane/journal"
	"github.com/araihu/paje/internal/controlplane/projection"
)

func TestJournalFilesystemPersistsCanonicalFeedAndRepairsDerivedState(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	installationID := readInstallationID(t, root)
	action := journal.Action{
		ID: "action-a", ControlRunID: "control-a", TaskID: "task-a", AttemptID: "attempt-a",
		Kind: journal.KindDispatch, GraphRevision: 1, ExpectedProjection: 1,
		CanonicalRequestDigest: digest("request-a"), IdempotencyKey: "key-a",
	}
	if _, created, err := store.Reserve(context.Background(), action); err != nil || !created {
		t.Fatalf("Reserve() = created %v, error %v", created, err)
	}
	result, err := store.Append(context.Background(), action.ControlRunID, 1, journal.Event{
		ID: "event-result-a", ControlRunID: action.ControlRunID, ActionID: action.ID,
		Kind: journal.EventActionResult, PayloadDigest: digest("result-a"),
		OccurredAt: time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RunSequence != 2 || result.JournalPosition != 2 {
		t.Fatalf("Append() positions = %d/%d, want 2/2", result.RunSequence, result.JournalPosition)
	}
	runCursor := journal.RunCursor{
		InstallationID: installationID, ControlRunID: action.ControlRunID,
		SchemaVersion: journal.SchemaVersion, RunSequence: 2,
	}
	globalCursor := journal.GlobalCursor{
		InstallationID: installationID, SchemaVersion: journal.SchemaVersion,
		JournalPosition: 2,
	}
	runEvents, _, err := store.RunEvents(
		context.Background(),
		journal.NewRunCursor(installationID, action.ControlRunID),
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := projection.RebuildRun(runEvents)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(context.Background(), runCursor, globalCursor, projected); err != nil {
		t.Fatal(err)
	}

	runHash := fmt.Sprintf("%x", sha256.Sum256([]byte(action.ControlRunID)))
	indexPath := filepath.Join(root, "runs", runHash, "event-index", "00000000000000000002.json")
	checkpointPath := filepath.Join(root, "runs", runHash, "checkpoint.json")
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath, []byte(`{"edited":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	feed, next, err := reopened.Feed(context.Background(), journal.NewGlobalCursor(installationID), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 || next.JournalPosition != 2 || feed[1] != result {
		t.Fatalf("Feed() = %#v cursor %#v", feed, next)
	}
	repaired, loadedRun, loadedGlobal, err := reopened.LoadCheckpoint(context.Background(), action.ControlRunID)
	if err != nil {
		t.Fatal(err)
	}
	if string(repaired) != string(projected) || loadedRun != runCursor || loadedGlobal != globalCursor {
		t.Fatalf("repaired checkpoint = %q %#v %#v", repaired, loadedRun, loadedGlobal)
	}
	if info, err := os.Lstat(indexPath); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("repaired index mode = %#v error %v", info, err)
	}
}

func TestActiveRunsResumesAfterBoundaryDeactivationAfterFilesystemRestart(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	installationID := readInstallationID(t, root)
	for _, runID := range []string{"run-c", "run-a", "run-b"} {
		snapshot := validSnapshot(t)
		snapshot.Run.ID = runID
		snapshot.Graph.ControlRunID = runID
		if _, err := store.Create(context.Background(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	first, cursor, err := store.ActiveRuns(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first) != "[run-a]" || cursor != installationID+":run-a" {
		t.Fatalf("ActiveRuns(first) = %v cursor %q", first, cursor)
	}
	boundary, err := store.Load(context.Background(), "run-a")
	if err != nil {
		t.Fatal(err)
	}
	boundary.Run.Status = controlplane.StatusClosed
	if _, err := store.Save(context.Background(), boundary, boundary.Version); err != nil {
		t.Fatal(err)
	}

	restarted, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	second, cursor, err := restarted.ActiveRuns(context.Background(), cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(second) != "[run-b]" || cursor != installationID+":run-b" {
		t.Fatalf("ActiveRuns(after deactivation) = %v cursor %q", second, cursor)
	}
	third, cursor, err := restarted.ActiveRuns(context.Background(), cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(third) != "[run-c]" || cursor != "" {
		t.Fatalf("ActiveRuns(final page) = %v cursor %q", third, cursor)
	}
	if _, _, err := restarted.ActiveRuns(context.Background(), installationID+":", 1); !errors.Is(err, journal.ErrCursor) {
		t.Fatalf("ActiveRuns(malformed cursor) error = %v, want ErrCursor", err)
	}
}

func TestUnknownRunCursorIsIndependentOfUnrelatedFilesystemEventsAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	installationID := readInstallationID(t, root)
	cursor := journal.NewRunCursor(installationID, "run-unknown")
	assertUnknownRunPage := func(t *testing.T, store *filesystem.Store, label string) {
		t.Helper()
		events, next, err := store.RunEvents(context.Background(), cursor, 10)
		if err != nil {
			t.Fatalf("RunEvents(%s) error = %v", label, err)
		}
		if len(events) != 0 || next != cursor {
			t.Fatalf("RunEvents(%s) = %#v cursor %#v, want empty and %#v", label, events, next, cursor)
		}
	}
	assertUnknownRunPage(t, store, "before unrelated append")
	if _, err := store.Append(context.Background(), "run-related", 0, journal.Event{
		ID: "related-event", ControlRunID: "run-related", Kind: journal.EventProjectionUpdated,
		PayloadDigest: digest("related"), OccurredAt: time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	assertUnknownRunPage(t, restarted, "after unrelated append and restart")
	future := cursor
	future.RunSequence = 1
	if _, _, err := restarted.RunEvents(context.Background(), future, 10); !errors.Is(err, journal.ErrCursor) {
		t.Fatalf("RunEvents(unknown positive sequence) error = %v, want ErrCursor", err)
	}
}

func TestJournalMigrationImportsLegacySnapshotOnceAndRepairsEditedCheckpoint(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"records", "events", "transactions"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := validSnapshot(t)
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(root, "records", legacy.Run.ID+".json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	installationID := readInstallationID(t, root)
	firstFeed, firstCursor, err := store.Feed(context.Background(), journal.NewGlobalCursor(installationID), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstFeed) < 4 || firstFeed[0].Kind != journal.EventMigrationStarted ||
		firstFeed[len(firstFeed)-1].Kind != journal.EventMigrationCompleted {
		t.Fatalf("migration feed = %#v", firstFeed)
	}
	if err := os.WriteFile(filepath.Join(root, "records", legacy.Run.ID+".json"), []byte(`{"edited":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	secondFeed, secondCursor, err := reopened.Feed(context.Background(), journal.NewGlobalCursor(installationID), 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(secondFeed, firstFeed) || secondCursor != firstCursor {
		t.Fatalf("restart reimported or changed migration:\nfirst %#v\nsecond %#v", firstFeed, secondFeed)
	}
	repaired, err := reopened.Load(context.Background(), legacy.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repaired, legacy) {
		t.Fatalf("journal repair changed snapshot:\n got %#v\nwant %#v", repaired, legacy)
	}
}

func TestMigrationMarkerRejectsUnboundReceiptWithoutFilesystemMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, *filesystem.Store, controlplane.Snapshot, []journal.Event, *testMigrationMarker)
	}{
		{
			name: "snapshot digest mismatch",
			mutate: func(t *testing.T, root string, _ *filesystem.Store, _ controlplane.Snapshot, _ []journal.Event, marker *testMigrationMarker) {
				marker.SnapshotDigest = digest("wrong snapshot set")
				writeTestCanonicalJSON(t, filepath.Join(root, "journal", "migration.json"), marker)
			},
		},
		{
			name: "forged valid-looking digest",
			mutate: func(t *testing.T, root string, _ *filesystem.Store, legacy controlplane.Snapshot, _ []journal.Event, marker *testMigrationMarker) {
				forged, err := journal.Digest([]string{legacy.Run.ID + "=" + digest("forged snapshot")})
				if err != nil {
					t.Fatal(err)
				}
				marker.SnapshotDigest = forged
				writeTestCanonicalJSON(t, filepath.Join(root, "journal", "migration.json"), marker)
			},
		},
		{
			name: "position inside migration boundary",
			mutate: func(t *testing.T, root string, _ *filesystem.Store, _ controlplane.Snapshot, _ []journal.Event, marker *testMigrationMarker) {
				marker.JournalPosition--
				writeTestCanonicalJSON(t, filepath.Join(root, "journal", "migration.json"), marker)
			},
		},
		{
			name: "position after ordinary event",
			mutate: func(t *testing.T, root string, store *filesystem.Store, legacy controlplane.Snapshot, _ []journal.Event, marker *testMigrationMarker) {
				current, err := store.Load(context.Background(), legacy.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				next := controlplane.CloneSnapshot(current)
				next.Events = append(next.Events, controlplane.Event{
					Kind: controlplane.EventSteering, TaskID: "task-a", Digest: digest("ordinary event"),
				})
				if _, err := store.Save(context.Background(), next, current.Version); err != nil {
					t.Fatal(err)
				}
				feed, cursor, err := store.Feed(context.Background(), journal.GlobalCursor{}, 100)
				if err != nil {
					t.Fatal(err)
				}
				if feed[len(feed)-1].Kind != journal.EventProjectionUpdated {
					t.Fatalf("later event kind = %q", feed[len(feed)-1].Kind)
				}
				marker.JournalPosition = cursor.JournalPosition
				writeTestCanonicalJSON(t, filepath.Join(root, "journal", "migration.json"), marker)
			},
		},
		{
			name: "stale marker omits current record",
			mutate: func(t *testing.T, root string, _ *filesystem.Store, _ controlplane.Snapshot, _ []journal.Event, _ *testMigrationMarker) {
				missing := validSnapshot(t)
				missing.Run.ID = "control-2"
				missing.Graph.ControlRunID = missing.Run.ID
				if err := controlplane.ValidateSnapshot(missing); err != nil {
					t.Fatal(err)
				}
				writeTestCanonicalJSON(t, filepath.Join(root, "records", missing.Run.ID+".json"), missing)
			},
		},
		{
			name: "missing migration completion",
			mutate: func(t *testing.T, root string, _ *filesystem.Store, _ controlplane.Snapshot, events []journal.Event, marker *testMigrationMarker) {
				last := events[len(events)-1]
				if last.Kind != journal.EventMigrationCompleted {
					t.Fatalf("last migration event = %q", last.Kind)
				}
				if err := os.Remove(testJournalEventPath(root, last.JournalPosition)); err != nil {
					t.Fatal(err)
				}
				marker.JournalPosition--
				writeTestCanonicalJSON(t, filepath.Join(root, "journal", "migration.json"), marker)
			},
		},
		{
			name: "duplicate migration completion",
			mutate: func(t *testing.T, root string, store *filesystem.Store, _ controlplane.Snapshot, events []journal.Event, marker *testMigrationMarker) {
				last := events[len(events)-1]
				duplicate := last
				duplicate.ID = "forged_duplicate_migration_completed"
				duplicate.RunSequence = 0
				duplicate.JournalPosition = 0
				appended, err := store.Append(context.Background(), last.ControlRunID, last.RunSequence, duplicate)
				if err != nil {
					t.Fatal(err)
				}
				marker.JournalPosition = appended.JournalPosition
				writeTestCanonicalJSON(t, filepath.Join(root, "journal", "migration.json"), marker)
			},
		},
		{
			name: "reordered migration completion",
			mutate: func(t *testing.T, root string, _ *filesystem.Store, _ controlplane.Snapshot, events []journal.Event, _ *testMigrationMarker) {
				before := events[len(events)-2]
				after := events[len(events)-1]
				if before.Kind != journal.EventMigrationSnapshot || after.Kind != journal.EventMigrationCompleted {
					t.Fatalf("migration tail = %q, %q", before.Kind, after.Kind)
				}
				beforePosition, beforeSequence := before.JournalPosition, before.RunSequence
				afterPosition, afterSequence := after.JournalPosition, after.RunSequence
				before, after = after, before
				before.JournalPosition, before.RunSequence = beforePosition, beforeSequence
				after.JournalPosition, after.RunSequence = afterPosition, afterSequence
				writeTestCanonicalJSON(t, testJournalEventPath(root, beforePosition), before)
				writeTestCanonicalJSON(t, testJournalEventPath(root, afterPosition), after)
			},
		},
		{
			name: "altered migration snapshot payload digest",
			mutate: func(t *testing.T, root string, _ *filesystem.Store, legacy controlplane.Snapshot, events []journal.Event, _ *testMigrationMarker) {
				altered := controlplane.CloneSnapshot(legacy)
				altered.Run.UpdatedAt = time.Date(2026, time.July, 26, 13, 0, 0, 0, time.UTC)
				if err := controlplane.ValidateSnapshot(altered); err != nil {
					t.Fatal(err)
				}
				alteredDigest, err := journal.Digest(altered)
				if err != nil {
					t.Fatal(err)
				}
				writeTestCanonicalJSON(
					t, filepath.Join(root, "journal", "payloads", alteredDigest[len("sha256:"):]+".json"), altered,
				)
				for _, event := range events {
					if event.Kind != journal.EventMigrationSnapshot {
						continue
					}
					event.PayloadDigest = alteredDigest
					writeTestCanonicalJSON(t, testJournalEventPath(root, event.JournalPosition), event)
					return
				}
				t.Fatal("migration snapshot event not found")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, store, legacy, events, marker := newMigratedFilesystemFixture(t)
			test.mutate(t, root, store, legacy, events, &marker)
			before := snapshotFilesystemTree(t, root)
			if _, err := filesystem.New(root); !errors.Is(err, controlplane.ErrCorruptStore) {
				t.Fatalf("New(unbound migration receipt) error = %v, want ErrCorruptStore", err)
			}
			if after := snapshotFilesystemTree(t, root); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected migration receipt mutated filesystem:\nbefore %#v\nafter  %#v", before, after)
			}
		})
	}
}

func TestMigrationMarkerRemainsValidAfterLaterOrdinaryEventsAndRestart(t *testing.T) {
	root, store, legacy, _, marker := newMigratedFilesystemFixture(t)
	markerPath := filepath.Join(root, "journal", "migration.json")
	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Load(context.Background(), legacy.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := controlplane.CloneSnapshot(current)
	next.Events = append(next.Events, controlplane.Event{
		Kind: controlplane.EventSteering, TaskID: "task-a", Digest: digest("later ordinary event"),
	})
	saved, err := store.Save(context.Background(), next, current.Version)
	if err != nil {
		t.Fatal(err)
	}
	wantFeed, wantCursor, err := store.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if wantCursor.JournalPosition <= marker.JournalPosition ||
		wantFeed[len(wantFeed)-1].Kind != journal.EventProjectionUpdated {
		t.Fatalf("later feed boundary = %#v last %#v", wantCursor, wantFeed[len(wantFeed)-1])
	}
	for restart := 0; restart < 2; restart++ {
		reopened, err := filesystem.New(root)
		if err != nil {
			t.Fatalf("New(valid historical migration receipt, restart %d): %v", restart, err)
		}
		loaded, err := reopened.Load(context.Background(), legacy.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(loaded, saved) {
			t.Fatalf("restarted snapshot = %#v, want %#v", loaded, saved)
		}
		feed, cursor, err := reopened.Feed(context.Background(), journal.GlobalCursor{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(feed, wantFeed) || cursor != wantCursor {
			t.Fatalf("restart changed feed: %#v cursor %#v", feed, cursor)
		}
		gotMarker, err := os.ReadFile(markerPath)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotMarker, markerBytes) {
			t.Fatalf("restart changed historical migration receipt: got %q want %q", gotMarker, markerBytes)
		}
		store = reopened
	}
}

func TestUnknownKindMigrationFailsBeforeJournalMutationAndCleanlyReopens(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"records", "events", "transactions"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := snapshotWithPersistentAttempt(t)
	action := controlplane.LifecycleAction{
		ID: "unknown-action", AttemptID: "attempt-a",
		Kind:          agentharness.ActionKind("future_kind"),
		RequestDigest: digest("unknown-request"),
		PreparedAt:    time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
	}
	legacy.Actions[action.ID] = action
	attempt := legacy.Attempts[action.AttemptID]
	attempt.ActionIDs = append(attempt.ActionIDs, action.ID)
	legacy.Attempts[action.AttemptID] = attempt
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	recordPath := filepath.Join(root, "records", legacy.Run.ID+".json")
	if err := os.WriteFile(recordPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.New(root); err == nil {
		t.Fatal("New(legacy snapshot with unknown action kind) error = nil")
	}
	gotRecord, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRecord, encoded) {
		t.Fatalf("rejected migration changed legacy snapshot: got %q want %q", gotRecord, encoded)
	}
	for _, directory := range []string{
		"events", "transactions", filepath.Join("journal", "events"),
		filepath.Join("journal", "payloads"), "runs", "active",
	} {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("rejected migration populated %s: %#v", directory, entries)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "journal", "migration.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected migration marker stat error = %v, want not exist", err)
	}

	delete(legacy.Actions, action.ID)
	attempt = legacy.Attempts[action.AttemptID]
	attempt.ActionIDs = nil
	legacy.Attempts[action.AttemptID] = attempt
	valid, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	valid = append(valid, '\n')
	if err := os.WriteFile(recordPath, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.New(root); err != nil {
		t.Fatalf("New() after repairing rejected legacy snapshot: %v", err)
	}
}

func TestUnknownKindTransactionIsRejectedBeforeAnyFilesystemMutation(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(context.Background(), snapshotWithPersistentAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	next := controlplane.CloneSnapshot(current)
	action := controlplane.LifecycleAction{
		ID: "unknown-action", AttemptID: "attempt-a",
		Kind:          agentharness.ActionKind("future_kind"),
		RequestDigest: digest("unknown-request"),
		PreparedAt:    time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
	}
	next.Actions[action.ID] = action
	attempt := next.Attempts[action.AttemptID]
	attempt.ActionIDs = append(attempt.ActionIDs, action.ID)
	next.Attempts[action.AttemptID] = attempt
	before := snapshotFilesystemTree(t, root)
	beforeFeed, beforeCursor, err := store.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), next, current.Version); !errors.Is(err, controlplane.ErrInvalidRecord) {
		t.Fatalf("Save(unknown action kind) error = %v, want ErrInvalidRecord", err)
	}
	if after := snapshotFilesystemTree(t, root); !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected unknown-kind transaction mutated filesystem: before %#v after %#v", before, after)
	}
	reopened, err := filesystem.New(root)
	if err != nil {
		t.Fatalf("New() after rejected unknown-kind transaction: %v", err)
	}
	loaded, err := reopened.Load(context.Background(), current.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, current) {
		t.Fatalf("rejected unknown-kind transaction changed snapshot: got %#v want %#v", loaded, current)
	}
	feed, cursor, err := reopened.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(feed, beforeFeed) || cursor != beforeCursor {
		t.Fatalf("rejected unknown-kind transaction changed feed: %#v cursor %#v", feed, cursor)
	}
}

func TestJournalLoadRepairsDeletedSnapshotCheckpointWithoutRestart(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := store.Create(context.Background(), validSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "records", want.Run.ID+".json")); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background(), want.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load(repaired checkpoint) = %#v, want %#v", got, want)
	}
}

func TestJournalRestartFailsClosedForCanonicalEventCorruptionAndSymlink(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"corrupt", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(filepath.Dir(path)), "outside-event")
			if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Append(context.Background(), "control-a", 0, journal.Event{
				ID: "event-a", ControlRunID: "control-a", Kind: journal.EventProjectionUpdated,
				PayloadDigest: digest("event-a"), OccurredAt: time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC),
			}); err != nil {
				t.Fatal(err)
			}
			eventPath := filepath.Join(root, "journal", "events", "00000000000000000001.json")
			test.mutate(t, eventPath)
			if _, err := filesystem.New(root); !errors.Is(err, controlplane.ErrCorruptStore) {
				t.Fatalf("New(corrupt journal) error = %v, want ErrCorruptStore", err)
			}
		})
	}
}

func TestCrashJournalAppendRecoversAtEveryAuthoritativeBoundary(t *testing.T) {
	boundaries := []filesystem.DurableBoundary{
		filesystem.BoundaryGlobalPosition,
		filesystem.BoundaryCanonicalVisible,
		filesystem.BoundaryEventCommitted,
		filesystem.BoundaryRunIndexRepaired,
		filesystem.BoundaryActiveIndexUpdated,
		filesystem.BoundaryResponse,
	}
	for _, boundary := range boundaries {
		t.Run(string(boundary), func(t *testing.T) {
			root := t.TempDir()
			if _, err := filesystem.New(root); err != nil {
				t.Fatal(err)
			}
			crash := errors.New("crash at " + string(boundary))
			injected := false
			faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
				if !injected && got == boundary {
					injected = true
					return crash
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			input := journal.Event{
				ID: "event-a", ControlRunID: "control-a", Kind: journal.EventProjectionUpdated,
				PayloadDigest: digest("event-a"), OccurredAt: time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC),
			}
			if _, err := faulted.Append(context.Background(), "control-a", 0, input); !errors.Is(err, crash) {
				t.Fatalf("Append(%s) error = %v, want injected crash", boundary, err)
			}
			if !injected {
				t.Fatalf("boundary %s was not reached", boundary)
			}

			restarted, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			installationID := readInstallationID(t, root)
			beforeRetry, _, err := restarted.Feed(context.Background(), journal.NewGlobalCursor(installationID), 10)
			if err != nil {
				t.Fatal(err)
			}
			if boundary == filesystem.BoundaryGlobalPosition && len(beforeRetry) != 0 {
				t.Fatalf("pre-visibility crash persisted %d events, want 0", len(beforeRetry))
			}
			if boundary != filesystem.BoundaryGlobalPosition && len(beforeRetry) != 1 {
				t.Fatalf("post-visibility crash persisted %d events, want 1", len(beforeRetry))
			}
			got, err := restarted.Append(context.Background(), "control-a", 0, input)
			if err != nil {
				t.Fatal(err)
			}
			feed, cursor, err := restarted.Feed(context.Background(), journal.NewGlobalCursor(installationID), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(feed) != 1 || cursor.JournalPosition != 1 || feed[0] != got {
				t.Fatalf("recovered feed = %#v cursor %#v", feed, cursor)
			}
		})
	}
}

func TestCrashReservationAndResultNeverDuplicateActionEffects(t *testing.T) {
	t.Run("before reservation", func(t *testing.T) {
		root := t.TempDir()
		if _, err := filesystem.New(root); err != nil {
			t.Fatal(err)
		}
		crash := errors.New("before reservation")
		faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
			if got == filesystem.BoundaryBeforeReservation {
				return crash
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		action := journal.Action{
			ID: "action-a", ControlRunID: "control-a", TaskID: "task-a", AttemptID: "attempt-a",
			Kind: journal.KindDispatch, GraphRevision: 1,
			CanonicalRequestDigest: digest("request-a"), IdempotencyKey: "key-a",
		}
		if _, _, err := faulted.Reserve(context.Background(), action); !errors.Is(err, crash) {
			t.Fatalf("Reserve() error = %v, want injected crash", err)
		}
		restarted, err := filesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, created, err := restarted.Reserve(context.Background(), action); err != nil || !created {
			t.Fatalf("Reserve(retry) = created %v error %v", created, err)
		}
	})

	t.Run("reservation persisted before event", func(t *testing.T) {
		root := t.TempDir()
		if _, err := filesystem.New(root); err != nil {
			t.Fatal(err)
		}
		crash := errors.New("reservation file persisted")
		faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
			if got == filesystem.BoundaryReservationPersisted {
				return crash
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		action := journal.Action{
			ID: "action-a", ControlRunID: "control-a", TaskID: "task-a", AttemptID: "attempt-a",
			Kind: journal.KindDispatch, GraphRevision: 1,
			CanonicalRequestDigest: digest("request-a"), IdempotencyKey: "key-a",
		}
		if _, _, err := faulted.Reserve(context.Background(), action); !errors.Is(err, crash) {
			t.Fatalf("Reserve() error = %v, want injected crash", err)
		}
		restarted, err := filesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, created, err := restarted.Reserve(context.Background(), action); err != nil || created {
			t.Fatalf("Reserve(recovered) = created %v error %v", created, err)
		}
		installationID := readInstallationID(t, root)
		feed, _, err := restarted.Feed(context.Background(), journal.NewGlobalCursor(installationID), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(feed) != 1 || feed[0].Kind != journal.EventActionReserved {
			t.Fatalf("recovered reservation feed = %#v", feed)
		}
	})

	t.Run("reservation response loss", func(t *testing.T) {
		root := t.TempDir()
		if _, err := filesystem.New(root); err != nil {
			t.Fatal(err)
		}
		crash := errors.New("reservation response lost")
		injected := false
		faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
			if !injected && got == filesystem.BoundaryAfterReservation {
				injected = true
				return crash
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		action := journal.Action{
			ID: "action-a", ControlRunID: "control-a", TaskID: "task-a", AttemptID: "attempt-a",
			Kind: journal.KindDispatch, GraphRevision: 1,
			CanonicalRequestDigest: digest("request-a"), IdempotencyKey: "key-a",
		}
		if _, _, err := faulted.Reserve(context.Background(), action); !errors.Is(err, crash) {
			t.Fatalf("Reserve() error = %v, want injected crash", err)
		}
		restarted, err := filesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		if got, created, err := restarted.Reserve(context.Background(), action); err != nil || created || got != action {
			t.Fatalf("Reserve(replay) = %#v created %v error %v", got, created, err)
		}
		installationID := readInstallationID(t, root)
		feed, _, err := restarted.Feed(context.Background(), journal.NewGlobalCursor(installationID), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(feed) != 1 || feed[0].Kind != journal.EventActionReserved {
			t.Fatalf("reservation feed = %#v", feed)
		}
	})

	t.Run("result response loss", func(t *testing.T) {
		root := t.TempDir()
		store, err := filesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		action := journal.Action{
			ID: "action-a", ControlRunID: "control-a", TaskID: "task-a", AttemptID: "attempt-a",
			Kind: journal.KindDispatch, GraphRevision: 1,
			CanonicalRequestDigest: digest("request-a"), IdempotencyKey: "key-a",
		}
		if _, _, err := store.Reserve(context.Background(), action); err != nil {
			t.Fatal(err)
		}
		crash := errors.New("result response lost")
		injected := false
		faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
			if !injected && got == filesystem.BoundaryResultAppended {
				injected = true
				return crash
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		result := journal.Event{
			ID: "result-a", ControlRunID: "control-a", ActionID: action.ID,
			Kind: journal.EventActionResult, PayloadDigest: digest("result-a"),
			OccurredAt: time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC),
		}
		if _, err := faulted.Append(context.Background(), "control-a", 1, result); !errors.Is(err, crash) {
			t.Fatalf("Append(result) error = %v, want injected crash", err)
		}
		restarted, err := filesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := restarted.Append(context.Background(), "control-a", 1, result); err != nil {
			t.Fatal(err)
		}
		installationID := readInstallationID(t, root)
		feed, _, err := restarted.Feed(context.Background(), journal.NewGlobalCursor(installationID), 10)
		if err != nil {
			t.Fatal(err)
		}
		results := 0
		for _, event := range feed {
			if event.ActionID == action.ID && event.Kind == journal.EventActionResult {
				results++
			}
		}
		if results != 1 {
			t.Fatalf("bound results = %d, feed %#v", results, feed)
		}
	})
}

func TestCrashEveryActionKindRestartsWithOneDurableOutcome(t *testing.T) {
	kinds := []journal.Kind{
		journal.KindDispatch, journal.KindRegisterRuntime, journal.KindSend,
		journal.KindAcknowledge, journal.KindCallback, journal.KindObserve,
		journal.KindWait, journal.KindInterrupt, journal.KindCancel,
		journal.KindAllocateResource, journal.KindDisposeResource,
		journal.KindVerifyCandidate, journal.KindRunGate, journal.KindIntegrate,
		journal.KindPublish, journal.KindVerifyTargetTree, journal.KindCloseRuntime,
		journal.KindArchiveSession, journal.KindCloseRun,
	}
	outcomes := []journal.EventKind{
		journal.EventActionResult,
		journal.EventActionNotPerformed,
		journal.EventActionAmbiguous,
	}
	for index, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			root := t.TempDir()
			store, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			action := journal.Action{
				ID: "action-" + string(kind), ControlRunID: "control-a",
				TaskID: "task-a", AttemptID: "attempt-a", Kind: kind,
				GraphRevision: 1, CanonicalRequestDigest: digest("request-" + string(kind)),
				IdempotencyKey: "key-" + string(kind),
			}
			if _, _, err := store.Reserve(context.Background(), action); err != nil {
				t.Fatal(err)
			}
			crash := errors.New("lost outcome response")
			injected := false
			faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
				if !injected && got == filesystem.BoundaryResponse {
					injected = true
					return crash
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			outcome := journal.Event{
				ID: "outcome-" + string(kind), ControlRunID: "control-a", ActionID: action.ID,
				Kind: outcomes[index%len(outcomes)], PayloadDigest: digest("outcome-" + string(kind)),
				OccurredAt: time.Date(2026, time.July, 26, 0, 0, index, 0, time.UTC),
			}
			if _, err := faulted.Append(context.Background(), "control-a", 1, outcome); !errors.Is(err, crash) {
				t.Fatalf("Append() error = %v, want response loss", err)
			}
			restarted, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.Append(context.Background(), "control-a", 1, outcome); err != nil {
				t.Fatal(err)
			}
			installationID := readInstallationID(t, root)
			feed, _, err := restarted.Feed(context.Background(), journal.NewGlobalCursor(installationID), 10)
			if err != nil {
				t.Fatal(err)
			}
			bound := 0
			for _, event := range feed {
				if event.ActionID == action.ID && journal.IsOutcome(event.Kind) {
					bound++
				}
			}
			if bound != 1 {
				t.Fatalf("durable outcomes = %d, feed %#v", bound, feed)
			}
		})
	}
}

func TestCrashMigrationResumesWithoutRenumberingOrSecondImport(t *testing.T) {
	boundaries := []filesystem.DurableBoundary{
		filesystem.BoundaryGlobalPosition,
		filesystem.BoundaryCanonicalVisible,
		filesystem.BoundaryRunIndexRepaired,
		filesystem.BoundaryActiveIndexUpdated,
		filesystem.BoundaryResponse,
	}
	for _, boundary := range boundaries {
		t.Run(string(boundary), func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"records", "events", "transactions"} {
				if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			legacy := validSnapshot(t)
			encoded, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(root, "records", legacy.Run.ID+".json"),
				append(encoded, '\n'), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			crash := errors.New("migration crash at " + string(boundary))
			injected := false
			if _, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
				if !injected && got == boundary {
					injected = true
					return crash
				}
				return nil
			})); !errors.Is(err, crash) {
				t.Fatalf("NewWithOptions(%s) error = %v, want injected crash", boundary, err)
			}
			restarted, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			installationID := readInstallationID(t, root)
			first, cursor, err := restarted.Feed(context.Background(), journal.NewGlobalCursor(installationID), 100)
			if err != nil {
				t.Fatal(err)
			}
			again, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			second, secondCursor, err := again.Feed(context.Background(), journal.NewGlobalCursor(installationID), 100)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, second) || cursor != secondCursor ||
				first[0].Kind != journal.EventMigrationStarted ||
				first[len(first)-1].Kind != journal.EventMigrationCompleted {
				t.Fatalf("migration replay changed:\nfirst %#v %#v\nsecond %#v %#v", first, cursor, second, secondCursor)
			}
		})
	}
}

func TestRestartRebuildsCursorIndexAndPreservesCompareAndSwap(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	state := validSnapshot(t)
	created, err := store.Create(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	next := controlplane.CloneSnapshot(created)
	next.Events = append(next.Events, controlplane.Event{
		Cursor: 1, ControlRunID: next.Run.ID, Kind: controlplane.EventSteering,
		TaskID: "task-a", Digest: digest("event"),
	})
	saved, err := store.Save(context.Background(), next, created.Version)
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(context.Background(), state.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != saved.Version || loaded.Run.EventCursor != 1 {
		t.Fatalf("Load() version/cursor = %d/%d, want %d/1", loaded.Version, loaded.Run.EventCursor, saved.Version)
	}
	events, cursor, err := reopened.EventsAfter(context.Background(), state.Run.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || cursor != 1 {
		t.Fatalf("EventsAfter() = %#v, %d", events, cursor)
	}
	if _, err := reopened.Save(context.Background(), loaded, created.Version); !errors.Is(err, controlplane.ErrVersionConflict) {
		t.Fatalf("Save(stale) error = %v, want ErrVersionConflict", err)
	}
	changedIdentity := controlplane.CloneSnapshot(loaded)
	changedIdentity.Run.PrincipalID = "other-principal"
	if _, err := reopened.Save(context.Background(), changedIdentity, loaded.Version); !errors.Is(err, controlplane.ErrImmutableBoundary) {
		t.Fatalf("Save(changed principal) error = %v, want ErrImmutableBoundary", err)
	}
}

func TestRestartRecoversInterruptedSnapshotAndEventTransactionAtEveryBoundary(t *testing.T) {
	t.Parallel()

	boundaries := []filesystem.DurableBoundary{
		filesystem.BoundaryTransactionCommitted,
		filesystem.BoundaryEventRootCommitted,
		filesystem.BoundaryEventCommitted,
		filesystem.BoundarySnapshotCommitted,
		filesystem.BoundaryTransactionCleared,
	}
	for _, boundary := range boundaries {
		t.Run(string(boundary), func(t *testing.T) {
			root := t.TempDir()
			initial, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			created, err := initial.Create(context.Background(), validSnapshot(t))
			if err != nil {
				t.Fatal(err)
			}
			crash := errors.New("injected crash at " + string(boundary))
			injected := false
			faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
				if !injected && got == boundary {
					injected = true
					return crash
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			next := controlplane.CloneSnapshot(created)
			next.Events = append(next.Events, controlplane.Event{
				Kind: controlplane.EventSteering, TaskID: "task-a", Digest: digest("event-" + string(boundary)),
			})
			if _, err := faulted.Save(context.Background(), next, created.Version); !errors.Is(err, crash) {
				t.Fatalf("Save(%s) error = %v, want injected crash", boundary, err)
			}
			if !injected {
				t.Fatalf("fault boundary %s was not reached", boundary)
			}

			restarted, err := filesystem.New(root)
			if err != nil {
				t.Fatalf("New() after %s: %v", boundary, err)
			}
			loaded, err := restarted.Load(context.Background(), created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Version != created.Version+1 || loaded.Run.EventCursor != 1 || len(loaded.Events) != 1 {
				t.Fatalf("recovered state after %s = version %d cursor %d events %d", boundary, loaded.Version, loaded.Run.EventCursor, len(loaded.Events))
			}
			events, cursor, err := restarted.EventsAfter(context.Background(), created.Run.ID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || cursor != 1 || events[0] != loaded.Events[0] {
				t.Fatalf("recovered event stream after %s = %#v cursor %d", boundary, events, cursor)
			}
		})
	}
}

func TestRestartRepairsDerivedSnapshotAndFailsClosedForSymlinkAndUnexpectedEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		recoverable bool
		mutate      func(*testing.T, string)
	}{
		{"corrupt derived checkpoint", true, func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "records", "control-1.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", false, func(t *testing.T, root string) {
			t.Helper()
			target := filepath.Join(root, "outside")
			if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "records", "linked.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"unexpected", false, func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "records", "surprise.txt"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(context.Background(), validSnapshot(t)); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, root)
			reopened, err := filesystem.New(root)
			if test.recoverable {
				if err != nil {
					t.Fatalf("New(derived checkpoint corruption) error = %v", err)
				}
				if _, err := reopened.Load(context.Background(), "control-1"); err != nil {
					t.Fatalf("Load(repaired checkpoint) error = %v", err)
				}
			} else if !errors.Is(err, controlplane.ErrCorruptStore) {
				t.Fatalf("New(corrupt store) error = %v, want ErrCorruptStore", err)
			}
		})
	}
}

func TestStoreCompareAndSwapSerializesConcurrentWriters(t *testing.T) {
	t.Parallel()

	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(context.Background(), validSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	type result struct{ err error }
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for _, name := range []string{"first", "second"} {
		go func() {
			ready.Done()
			<-start
			next := controlplane.CloneSnapshot(created)
			next.Events = append(next.Events, controlplane.Event{
				Kind: controlplane.EventSteering, TaskID: "task-a", Digest: digest(name),
			})
			_, saveErr := store.Save(context.Background(), next, created.Version)
			results <- result{err: saveErr}
		}()
	}
	ready.Wait()
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			successes++
		case errors.Is(got.err, controlplane.ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("Save() unexpected error = %v", got.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent saves = %d successes, %d conflicts", successes, conflicts)
	}
}

func TestOutcomeSaveRejectsMultipleTerminalActionsAndKeepsSingleOutcomeBound(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	state, reservations := snapshotWithPendingActions(t)
	created, err := store.Create(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	for _, reservation := range reservations {
		if _, wasCreated, reserveErr := store.Reserve(context.Background(), reservation); reserveErr != nil || !wasCreated {
			t.Fatalf("Reserve(%s) = created %v error %v", reservation.ID, wasCreated, reserveErr)
		}
	}
	beforeFeed, beforeCursor, err := store.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	multiple := controlplane.CloneSnapshot(created)
	completeSnapshotAction(&multiple, "action-a", "result-a")
	completeSnapshotAction(&multiple, "action-b", "result-b")
	for retry := 0; retry < 3; retry++ {
		if _, saveErr := store.Save(context.Background(), multiple, created.Version); !errors.Is(saveErr, controlplane.ErrInvalidRecord) {
			t.Fatalf("Save(two outcomes, retry %d) error = %v, want ErrInvalidRecord", retry, saveErr)
		}
		reopened, reopenErr := filesystem.New(root)
		if reopenErr != nil {
			t.Fatal(reopenErr)
		}
		loaded, loadErr := reopened.Load(context.Background(), created.Run.ID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if !reflect.DeepEqual(loaded, created) {
			t.Fatalf("rejected multi-outcome Save changed snapshot on retry %d", retry)
		}
		feed, cursor, feedErr := reopened.Feed(context.Background(), journal.GlobalCursor{}, 100)
		if feedErr != nil {
			t.Fatal(feedErr)
		}
		if !reflect.DeepEqual(feed, beforeFeed) || cursor != beforeCursor {
			t.Fatalf("rejected multi-outcome Save changed feed on retry %d", retry)
		}
		store = reopened
	}

	first := controlplane.CloneSnapshot(created)
	completeSnapshotAction(&first, "action-a", "result-a")
	firstSaved, err := store.Save(context.Background(), first, created.Version)
	if err != nil {
		t.Fatal(err)
	}
	second := controlplane.CloneSnapshot(firstSaved)
	ambiguateSnapshotAction(&second, "action-b", "ambiguous-b")
	fullySaved, err := store.Save(context.Background(), second, firstSaved.Version)
	if err != nil {
		t.Fatal(err)
	}
	feed, _, err := store.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalFactsHaveSingleReservedOutcome(t, fullySaved, feed)
}

func TestRebindOutcomeSavePreflightRejectsMissingReservationAndImmutableIdentityWithoutSideEffects(t *testing.T) {
	t.Run("missing reservation", func(t *testing.T) {
		root := t.TempDir()
		store, err := filesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		state, _ := snapshotWithPendingActions(t)
		delete(state.Actions, "action-b")
		attempt := state.Attempts["attempt-a"]
		attempt.ActionIDs = []string{"action-a"}
		state.Attempts[attempt.ID] = attempt
		created, err := store.Create(context.Background(), state)
		if err != nil {
			t.Fatal(err)
		}
		next := controlplane.CloneSnapshot(created)
		ambiguateSnapshotAction(&next, "action-a", "missing_reservation")
		assertRejectedFilesystemOutcomeIsSideEffectFree(t, root, store, created, next)
	})

	mutations := []struct {
		name   string
		mutate func(*controlplane.Snapshot)
	}{
		{
			name: "request digest",
			mutate: func(next *controlplane.Snapshot) {
				action := next.Actions["action-a"]
				action.RequestDigest = digest("rebound-request")
				next.Actions[action.ID] = action
			},
		},
		{
			name: "kind",
			mutate: func(next *controlplane.Snapshot) {
				action := next.Actions["action-a"]
				action.Kind = agentharness.ActionInterrupt
				next.Actions[action.ID] = action
			},
		},
		{
			name: "attempt",
			mutate: func(next *controlplane.Snapshot) {
				rebound := next.Attempts["attempt-a"]
				rebound.ID = "attempt-rebound"
				rebound.State = controlplane.AttemptCompleted
				rebound.ActionIDs = nil
				next.Attempts[rebound.ID] = rebound
				action := next.Actions["action-a"]
				action.AttemptID = rebound.ID
				next.Actions[action.ID] = action
			},
		},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			state, reservations := snapshotWithPendingActions(t)
			delete(state.Actions, "action-b")
			attempt := state.Attempts["attempt-a"]
			attempt.ActionIDs = []string{"action-a"}
			state.Attempts[attempt.ID] = attempt
			created, err := store.Create(context.Background(), state)
			if err != nil {
				t.Fatal(err)
			}
			if _, wasCreated, err := store.Reserve(context.Background(), reservations[0]); err != nil || !wasCreated {
				t.Fatalf("Reserve() = created %v error %v", wasCreated, err)
			}
			next := controlplane.CloneSnapshot(created)
			ambiguateSnapshotAction(&next, "action-a", "rebound_identity")
			test.mutate(&next)
			assertRejectedFilesystemOutcomeIsSideEffectFree(t, root, store, created, next)
		})
	}
}

func assertRejectedFilesystemOutcomeIsSideEffectFree(
	t *testing.T,
	root string,
	store *filesystem.Store,
	want controlplane.Snapshot,
	next controlplane.Snapshot,
) {
	t.Helper()
	beforeTree := snapshotFilesystemTree(t, root)
	beforeFeed, beforeCursor, err := store.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), next, want.Version); err == nil {
		t.Fatal("Save(invalid terminal outcome) error = nil")
	}
	if afterTree := snapshotFilesystemTree(t, root); !reflect.DeepEqual(afterTree, beforeTree) {
		t.Fatalf("rejected Save mutated filesystem: before %#v after %#v", beforeTree, afterTree)
	}
	restarted, err := filesystem.New(root)
	if err != nil {
		t.Fatalf("New() after rejected Save: %v", err)
	}
	loaded, err := restarted.Load(context.Background(), want.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("rejected Save changed snapshot: got %#v want %#v", loaded, want)
	}
	feed, cursor, err := restarted.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(feed, beforeFeed) || cursor != beforeCursor {
		t.Fatalf("rejected Save changed journal: feed %#v cursor %#v", feed, cursor)
	}
}

type testMigrationMarker struct {
	SchemaVersion   string                  `json:"schema_version"`
	JournalPosition journal.JournalPosition `json:"journal_position"`
	SnapshotDigest  string                  `json:"snapshot_digest"`
}

func newMigratedFilesystemFixture(
	t *testing.T,
) (string, *filesystem.Store, controlplane.Snapshot, []journal.Event, testMigrationMarker) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"records", "events", "transactions"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := validSnapshot(t)
	writeTestCanonicalJSON(t, filepath.Join(root, "records", legacy.Run.ID+".json"), legacy)
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	feed, cursor, err := store.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	marker := readTestMigrationMarker(t, root)
	if marker.JournalPosition != cursor.JournalPosition || len(feed) == 0 ||
		feed[len(feed)-1].Kind != journal.EventMigrationCompleted {
		t.Fatalf("migration fixture feed = %#v cursor %#v marker %#v", feed, cursor, marker)
	}
	return root, store, legacy, feed, marker
}

func readTestMigrationMarker(t *testing.T, root string) testMigrationMarker {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(root, "journal", "migration.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker testMigrationMarker
	if err := json.Unmarshal(encoded, &marker); err != nil {
		t.Fatal(err)
	}
	return marker
}

func testJournalEventPath(root string, position journal.JournalPosition) string {
	return filepath.Join(root, "journal", "events", fmt.Sprintf("%020d.json", position))
}

func writeTestCanonicalJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := journal.CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapshotFilesystemTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[relative+"/"] = nil
			return nil
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[relative] = encoded
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestUnknownKindRecoveryRejectsCancelRebindingBeforeMutationAndCleanlyReopens(t *testing.T) {
	root := t.TempDir()
	initial, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	state := snapshotWithPersistentAttempt(t)
	if _, err := initial.Create(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("crash after transaction marker")
	injected := false
	faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(boundary filesystem.DurableBoundary) error {
		if !injected && boundary == filesystem.BoundaryTransactionCommitted {
			injected = true
			return crash
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	service := controlplane.NewService(faulted, controlplane.WithClock(func() time.Time {
		return time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	}))
	if _, err := service.PrepareAction(
		context.Background(), state.Run.ID, "attempt-a", agentharness.ActionSend, digest("recovery-request"),
	); !errors.Is(err, crash) {
		t.Fatalf("PrepareAction() error = %v, want injected crash", err)
	}
	if !injected {
		t.Fatal("transaction fault was not injected")
	}
	transactionPath := filepath.Join(root, "transactions", state.Run.ID+".json")
	original, err := os.ReadFile(transactionPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(original, &document); err != nil {
		t.Fatal(err)
	}
	reservation, ok := document["action"].(map[string]any)
	if !ok {
		t.Fatalf("transaction action = %#v", document["action"])
	}
	actionID, ok := reservation["id"].(string)
	if !ok || actionID == "" {
		t.Fatalf("transaction action id = %#v", reservation["id"])
	}
	reservation["kind"] = string(journal.KindCancel)
	next, ok := document["next"].(map[string]any)
	if !ok {
		t.Fatalf("transaction next = %#v", document["next"])
	}
	actions, ok := next["actions"].(map[string]any)
	if !ok {
		t.Fatalf("transaction actions = %#v", next["actions"])
	}
	lifecycle, ok := actions[actionID].(map[string]any)
	if !ok {
		t.Fatalf("transaction lifecycle action = %#v", actions[actionID])
	}
	lifecycle["kind"] = "future_kind"
	corrupt, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	corrupt = append(corrupt, '\n')
	if err := os.WriteFile(transactionPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotFilesystemTree(t, root)
	if _, err := filesystem.New(root); err == nil {
		t.Fatal("New(transaction with unknown action kind) error = nil")
	}
	if after := snapshotFilesystemTree(t, root); !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected recovery mutated filesystem: before %#v after %#v", before, after)
	}

	if err := os.WriteFile(transactionPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := filesystem.New(root)
	if err != nil {
		t.Fatalf("New() after restoring valid transaction: %v", err)
	}
	stored, err := reopened.Reservation(context.Background(), state.Run.ID, actionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Kind != journal.KindSend {
		t.Fatalf("recovered reservation kind = %q, want send", stored.Kind)
	}
	feed, _, err := reopened.Feed(context.Background(), journal.GlobalCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	reservations := 0
	for _, event := range feed {
		if event.Kind == journal.EventActionReserved && event.ActionID == actionID {
			reservations++
		}
	}
	if reservations != 1 {
		t.Fatalf("recovered action reservation events = %d, want 1", reservations)
	}
}

func TestCrashReservationAndSnapshotTransactionRecoversWithoutDuplicateOrStrand(t *testing.T) {
	boundaries := []filesystem.DurableBoundary{
		filesystem.BoundaryTransactionCommitted,
		filesystem.BoundaryBeforeReservation,
		filesystem.BoundaryReservationPersisted,
		filesystem.BoundaryGlobalPosition,
		filesystem.BoundaryCanonicalVisible,
		filesystem.BoundaryEventCommitted,
		filesystem.BoundaryRunIndexRepaired,
		filesystem.BoundaryActiveIndexUpdated,
		filesystem.BoundaryAfterReservation,
		filesystem.BoundaryResponse,
		filesystem.BoundaryEventRootCommitted,
		filesystem.BoundarySnapshotCommitted,
		filesystem.BoundaryTransactionCleared,
	}
	for _, boundary := range boundaries {
		t.Run(string(boundary), func(t *testing.T) {
			root := t.TempDir()
			initial, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			state := snapshotWithPersistentAttempt(t)
			if _, err := initial.Create(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			crash := errors.New("crash at " + string(boundary))
			injected := false
			faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
				if !injected && got == boundary {
					injected = true
					return crash
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			service := controlplane.NewService(faulted, controlplane.WithClock(func() time.Time {
				return time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
			}))
			if _, err := service.PrepareAction(
				context.Background(), state.Run.ID, "attempt-a", agentharness.ActionSend, digest("atomic-response-loss"),
			); !errors.Is(err, crash) {
				t.Fatalf("PrepareAction(%s) error = %v, want injected crash", boundary, err)
			}
			if !injected {
				t.Fatalf("boundary %s was not reached", boundary)
			}

			restarted, err := filesystem.New(root)
			if err != nil {
				t.Fatalf("New() after %s: %v", boundary, err)
			}
			restartedService := controlplane.NewService(restarted, controlplane.WithClock(func() time.Time {
				return time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
			}))
			action, err := restartedService.PrepareAction(
				context.Background(), state.Run.ID, "attempt-a", agentharness.ActionSend, digest("atomic-response-loss"),
			)
			if err != nil {
				t.Fatalf("PrepareAction(replay after %s): %v", boundary, err)
			}
			beforeReplay, err := restarted.Load(context.Background(), state.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			feedBefore, cursorBefore, err := restarted.Feed(context.Background(), journal.GlobalCursor{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restartedService.PrepareAction(
				context.Background(), state.Run.ID, "attempt-a", agentharness.ActionSend, digest("atomic-response-loss"),
			); err != nil {
				t.Fatalf("PrepareAction(second replay after %s): %v", boundary, err)
			}
			afterReplay, err := restarted.Load(context.Background(), state.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			feedAfter, cursorAfter, err := restarted.Feed(context.Background(), journal.GlobalCursor{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeReplay, afterReplay) || !reflect.DeepEqual(feedBefore, feedAfter) || cursorBefore != cursorAfter {
				t.Fatalf("exact replay after %s duplicated durable effects", boundary)
			}
			reservations := 0
			for _, event := range feedAfter {
				if event.ActionID == action.ID && event.Kind == journal.EventActionReserved {
					reservations++
				}
			}
			if beforeReplay.Actions[action.ID].ID == "" || reservations != 1 {
				t.Fatalf("recovered action after %s = %#v with %d reservations", boundary, beforeReplay.Actions[action.ID], reservations)
			}
		})
	}
}

func TestCrashOutcomeTransactionPreservesExactJ03Binding(t *testing.T) {
	for _, boundary := range []filesystem.DurableBoundary{
		filesystem.BoundaryResultAppended,
		filesystem.BoundaryResponse,
		filesystem.BoundarySnapshotCommitted,
		filesystem.BoundaryTransactionCleared,
	} {
		t.Run(string(boundary), func(t *testing.T) {
			root := t.TempDir()
			initial, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			state, reservations := snapshotWithPendingActions(t)
			delete(state.Actions, "action-b")
			attempt := state.Attempts["attempt-a"]
			attempt.ActionIDs = []string{"action-a"}
			state.Attempts[attempt.ID] = attempt
			created, err := initial.Create(context.Background(), state)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := initial.Reserve(context.Background(), reservations[0]); err != nil {
				t.Fatal(err)
			}
			next := controlplane.CloneSnapshot(created)
			completeSnapshotAction(&next, "action-a", "crash-result-a")
			crash := errors.New("crash at " + string(boundary))
			injected := false
			faulted, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
				if !injected && got == boundary {
					injected = true
					return crash
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := faulted.Save(context.Background(), next, created.Version); !errors.Is(err, crash) {
				t.Fatalf("Save(%s) error = %v, want injected crash", boundary, err)
			}
			restarted, err := filesystem.New(root)
			if err != nil {
				t.Fatalf("New() after %s: %v", boundary, err)
			}
			loaded, err := restarted.Load(context.Background(), created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			feed, _, err := restarted.Feed(context.Background(), journal.GlobalCursor{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			assertTerminalFactsHaveSingleReservedOutcome(t, loaded, feed)
		})
	}
}

func validSnapshot(t *testing.T) controlplane.Snapshot {
	t.Helper()
	graph := controlplane.TaskGraph{
		SchemaVersion: controlplane.SchemaVersion, ControlRunID: "control-1", Revision: 1,
		Tasks: []controlplane.Task{{
			ID: "task-a", Goal: "test", State: controlplane.TaskPending,
			Projects: []controlplane.ProjectRef{{
				ID: "project-a", Repository: "https://example.test/a.git", BaseRef: "main",
				BaseSHA:        digest("base"),
				WorkspaceScope: "workspace-a", CredentialScope: "credential-a",
				MailboxNamespace: "mail-a", EvidenceNamespace: "evidence-a",
			}},
			Ownership: controlplane.Ownership{Mutable: []string{"internal/a/**"}},
			Placement: controlplane.ExecutionPlacement{
				ParallelismPrimitive: "local_sequential", ExecutionPlacement: "current_control_agent",
				PlacementRationale: "test", CapabilityRequirements: []string{"local"},
				LifecycleOwner: "parent", Fallback: "block",
			},
			FrozenInputs: []controlplane.FrozenInput{{ID: "spec", Digest: digest("spec")}},
			Acceptance:   []controlplane.Gate{{ID: "test", Digest: digest("test")}},
		}},
		IntegrationOrder: []string{"task-a"},
		CombinedGates:    []controlplane.Gate{{ID: "combined", Digest: digest("combined")}},
	}
	snapshot, err := controlplane.NewSnapshot(controlplane.ControlRun{
		SchemaVersion: controlplane.SchemaVersion, ID: graph.ControlRunID,
		PrincipalID: "principal", GoalDigest: digest("goal"),
		GraphRevision: 1, Status: controlplane.StatusOpen,
	}, graph)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func snapshotWithPersistentAttempt(t *testing.T) controlplane.Snapshot {
	t.Helper()
	snapshot := validSnapshot(t)
	capabilities := agentharness.CapabilitySnapshot{
		HarnessID: "test-harness",
		Primitives: map[agentharness.Primitive]agentharness.PrimitiveCapabilities{
			agentharness.PersistentSession: {
				Primitive: agentharness.PersistentSession,
				Capabilities: agentharness.CapabilitySet(
					agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait,
					agentharness.CapRuntimeIdentity, agentharness.CapAcknowledge,
					agentharness.CapSend, agentharness.CapCallback, agentharness.CapCursor,
					agentharness.CapInterrupt, agentharness.CapIdempotency,
					agentharness.CapRestart, agentharness.CapArchive, agentharness.CapIsolation,
				),
				ConcurrencyLimit: 2,
			},
		},
	}
	snapshot.Attempts["attempt-a"] = controlplane.PlacementAttempt{
		ID: "attempt-a", TaskID: "task-a", Primitive: agentharness.PersistentSession,
		CapabilitySnapshot: capabilities, LifecycleOwner: "parent", State: controlplane.AttemptActive,
		RuntimeWorkIDs: []string{"runtime-a"}, ActionIDs: []string{},
		ObservedEvents: map[string]string{}, PromotionTrigger: controlplane.PromotionTriggerNone,
	}
	if err := controlplane.ValidateSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func snapshotWithPendingActions(t *testing.T) (controlplane.Snapshot, []journal.Action) {
	t.Helper()
	snapshot := snapshotWithPersistentAttempt(t)
	attempt := snapshot.Attempts["attempt-a"]
	preparedAt := time.Date(2026, time.July, 26, 11, 0, 0, 0, time.UTC)
	reservations := make([]journal.Action, 0, 2)
	for _, id := range []string{"action-a", "action-b"} {
		requestDigest := digest("request-" + id)
		snapshot.Actions[id] = controlplane.LifecycleAction{
			ID: id, AttemptID: attempt.ID, Kind: agentharness.ActionSend,
			RequestDigest: requestDigest, PreparedAt: preparedAt,
		}
		attempt.ActionIDs = append(attempt.ActionIDs, id)
		reservations = append(reservations, journal.Action{
			ID: id, ControlRunID: snapshot.Run.ID, TaskID: attempt.TaskID, AttemptID: attempt.ID,
			Kind: journal.KindSend, GraphRevision: snapshot.Graph.Revision,
			ExpectedProjection: snapshot.Version, CanonicalRequestDigest: requestDigest,
			IdempotencyKey: "key-" + id,
		})
	}
	snapshot.Attempts[attempt.ID] = attempt
	if err := controlplane.ValidateSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot, reservations
}

func completeSnapshotAction(snapshot *controlplane.Snapshot, actionID, resultLabel string) {
	action := snapshot.Actions[actionID]
	action.Completed = true
	action.CompletedAt = time.Date(2026, time.July, 26, 11, 30, 0, 0, time.UTC)
	action.Result = agentharness.ActionResult{
		ActionID: actionID, RuntimeWorkIDs: []string{"runtime-a"},
		ResultDigest: digest(resultLabel), MessageReceipt: "receipt-" + resultLabel,
	}
	snapshot.Actions[actionID] = action
}

func ambiguateSnapshotAction(snapshot *controlplane.Snapshot, actionID, code string) {
	action := snapshot.Actions[actionID]
	action.Ambiguous = true
	action.AmbiguityCode = code
	snapshot.Actions[actionID] = action
}

func assertTerminalFactsHaveSingleReservedOutcome(
	t *testing.T,
	snapshot controlplane.Snapshot,
	feed []journal.Event,
) {
	t.Helper()
	reservationPosition := make(map[string]journal.JournalPosition)
	outcomes := make(map[string]int)
	for index, event := range feed {
		switch event.Kind {
		case journal.EventActionReserved, journal.EventMigrationAction:
			if reservationPosition[event.ActionID] != 0 {
				t.Fatalf("action %q has duplicate reservations", event.ActionID)
			}
			reservationPosition[event.ActionID] = event.JournalPosition
		case journal.EventActionResult, journal.EventActionAmbiguous,
			journal.EventActionNotPerformed, journal.EventActionSuperseded:
			if reservationPosition[event.ActionID] == 0 || reservationPosition[event.ActionID] >= event.JournalPosition {
				t.Fatalf("terminal action event %#v has no prior reservation", event)
			}
			if err := journal.ValidateOutcomeTransition(feed[:index], event); err != nil {
				t.Fatalf("terminal action event %#v violates J03: %v", event, err)
			}
			outcomes[event.ActionID]++
		}
	}
	for actionID, action := range snapshot.Actions {
		if !action.Completed && !action.Ambiguous {
			continue
		}
		if outcomes[actionID] != 1 {
			t.Fatalf("terminal action %q has %d J03 outcomes, want exactly 1", actionID, outcomes[actionID])
		}
	}
}

func TestAuthoritativeFilesystemCommitPersistsReceiptAndPayloadsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	request := filesystemCommitRequest(t, readInstallationID(t, root), "run-a", "action-a", "key-a")
	// A caller clock may carry a monotonic reading and a named location. The
	// authoritative receipt must still round-trip through canonical JSON exactly.
	request.Outcome.OccurredAt = time.Now()
	first, err := store.Commit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Reservation.RunSequence != 1 || first.Reservation.JournalPosition != 1 ||
		first.Outcome.RunSequence != 2 || first.Outcome.JournalPosition != 2 {
		t.Fatalf("Commit() = %#v", first)
	}

	restarted, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Commit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Created || replayed.Action != first.Action ||
		replayed.Reservation != first.Reservation || replayed.Outcome != first.Outcome {
		t.Fatalf("Commit(restart replay) = %#v, want immutable %#v with Created=false", replayed, first)
	}
	assertFilesystemPayload(t, restarted, request.Action.CanonicalRequestDigest, request.RequestPayload)
	assertFilesystemPayload(t, restarted, request.Outcome.PayloadDigest, request.OutcomePayload)
	feed, cursor, err := restarted.Feed(context.Background(), journal.GlobalCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 || feed[0] != first.Reservation || feed[1] != first.Outcome ||
		cursor.JournalPosition != 2 {
		t.Fatalf("Feed(restarted) = %#v cursor %#v", feed, cursor)
	}
}

func TestAuthoritativeFilesystemReplayAndPayloadRejectGloballyInconsistentCommit(t *testing.T) {
	tests := []struct {
		name string
		call func(*filesystem.Store, journal.CommitRequest) error
	}{
		{
			name: "exact replay",
			call: func(store *filesystem.Store, request journal.CommitRequest) error {
				_, err := store.Commit(context.Background(), request)
				return err
			},
		},
		{
			name: "payload",
			call: func(store *filesystem.Store, request journal.CommitRequest) error {
				_, err := store.Payload(context.Background(), request.Action.CanonicalRequestDigest)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			request := filesystemCommitRequest(t, readInstallationID(t, root), "run-a", "action-a", "key-a")
			if _, err := store.Commit(context.Background(), request); err != nil {
				t.Fatal(err)
			}

			path := authoritativeCommitFixturePath(root, request.Action.ID)
			record := readAuthoritativeCommitFixture(t, path)
			record.Reservation.RunSequence = 3
			record.Reservation.JournalPosition = 3
			record.Outcome.RunSequence = 4
			record.Outcome.JournalPosition = 4
			writeAuthoritativeCommitFixture(t, path, record)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			if err := test.call(store, request); !errors.Is(err, controlplane.ErrCorruptStore) {
				t.Fatalf("operation error = %v, want ErrCorruptStore", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("rejected operation mutated the corrupt authoritative record")
			}
			assertDirectoryEntryCount(t, filepath.Join(root, "journal", "commits"), 1)
			assertDirectoryEntryCount(t, filepath.Join(root, "journal", "commit-staging"), 0)
		})
	}
}

func TestAuthoritativeFilesystemOperationsRejectLifetimeCommitDirectoryReplacement(t *testing.T) {
	operations := []struct {
		name    string
		prepare bool
		call    func(*filesystem.Store, journal.CommitRequest) error
	}{
		{
			name: "commit",
			call: func(store *filesystem.Store, request journal.CommitRequest) error {
				_, err := store.Commit(context.Background(), request)
				return err
			},
		},
		{
			name:    "exact replay",
			prepare: true,
			call: func(store *filesystem.Store, request journal.CommitRequest) error {
				_, err := store.Commit(context.Background(), request)
				return err
			},
		},
		{
			name:    "payload",
			prepare: true,
			call: func(store *filesystem.Store, request journal.CommitRequest) error {
				_, err := store.Payload(context.Background(), request.Action.CanonicalRequestDigest)
				return err
			},
		},
	}
	for _, replacement := range []string{"directory", "symlink"} {
		for _, operation := range operations {
			t.Run(replacement+"/"+operation.name, func(t *testing.T) {
				root := t.TempDir()
				store, err := filesystem.New(root)
				if err != nil {
					t.Fatal(err)
				}
				request := filesystemCommitRequest(t, readInstallationID(t, root), "run-a", "action-a", "key-a")
				if operation.prepare {
					if _, err := store.Commit(context.Background(), request); err != nil {
						t.Fatal(err)
					}
				}

				commits := filepath.Join(root, "journal", "commits")
				original := filepath.Join(t.TempDir(), "original-commits")
				if err := os.Rename(commits, original); err != nil {
					t.Fatal(err)
				}
				originalCount := directoryEntryCount(t, original)
				switch replacement {
				case "directory":
					if err := os.Mkdir(commits, 0o700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(original, commits); err != nil {
						t.Fatal(err)
					}
				}

				if err := operation.call(store, request); !errors.Is(err, controlplane.ErrCorruptStore) {
					t.Fatalf("operation error = %v, want ErrCorruptStore", err)
				}
				if got := directoryEntryCount(t, original); got != originalCount {
					t.Fatalf("original commits entries = %d, want unchanged %d", got, originalCount)
				}
				if replacement == "directory" {
					assertDirectoryEntryCount(t, commits, 0)
				}
			})
		}
	}
}

func TestAuthoritativeFilesystemCommitRejectsLifetimeStagingDirectoryReplacement(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			root := t.TempDir()
			crash := errors.New("prepared boundary reached")
			injected := false
			store, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(boundary filesystem.DurableBoundary) error {
				if boundary == filesystem.BoundaryAuthoritativeCommitPrepared {
					injected = true
					return crash
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			staging := filepath.Join(root, "journal", "commit-staging")
			original := filepath.Join(t.TempDir(), "original-staging")
			if err := os.Rename(staging, original); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "directory":
				if err := os.Mkdir(staging, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(original, staging); err != nil {
					t.Fatal(err)
				}
			}
			request := filesystemCommitRequest(t, readInstallationID(t, root), "run-a", "action-a", "key-a")
			if _, err := store.Commit(context.Background(), request); !errors.Is(err, controlplane.ErrCorruptStore) {
				t.Fatalf("Commit() error = %v, want ErrCorruptStore", err)
			}
			if injected {
				t.Fatal("commit reached the prepared boundary after staging identity replacement")
			}
			assertDirectoryEntryCount(t, original, 0)
			if replacement == "directory" {
				assertDirectoryEntryCount(t, staging, 0)
			}
			assertDirectoryEntryCount(t, filepath.Join(root, "journal", "commits"), 0)
		})
	}
}

func TestAuthoritativeFilesystemRecoveryRejectsLifetimeStagingDirectoryReplacement(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			root := t.TempDir()
			crash := errors.New("crash after preparation")
			failPrepared := true
			store, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(boundary filesystem.DurableBoundary) error {
				if failPrepared && boundary == filesystem.BoundaryAuthoritativeCommitPrepared {
					failPrepared = false
					return crash
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			request := filesystemCommitRequest(t, readInstallationID(t, root), "run-a", "action-a", "key-a")
			if _, err := store.Commit(context.Background(), request); !errors.Is(err, crash) {
				t.Fatalf("first Commit() error = %v, want injected crash", err)
			}
			staging := filepath.Join(root, "journal", "commit-staging")
			original := filepath.Join(t.TempDir(), "original-staging")
			if err := os.Rename(staging, original); err != nil {
				t.Fatal(err)
			}
			assertDirectoryEntryCount(t, original, 1)
			switch replacement {
			case "directory":
				if err := os.Mkdir(staging, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(original, staging); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := store.Commit(context.Background(), request); !errors.Is(err, controlplane.ErrCorruptStore) {
				t.Fatalf("Commit(recovery) error = %v, want ErrCorruptStore", err)
			}
			assertDirectoryEntryCount(t, original, 1)
			if replacement == "directory" {
				assertDirectoryEntryCount(t, staging, 0)
			}
			assertDirectoryEntryCount(t, filepath.Join(root, "journal", "commits"), 0)
		})
	}
}

func TestAuthoritativeFilesystemCommitEnforcesGlobalAndPerRunCAS(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	installationID := readInstallationID(t, root)
	first := filesystemCommitRequest(t, installationID, "run-a", "action-a", "key-a")
	if _, err := store.Commit(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := filesystemCommitRequest(t, installationID, "run-b", "action-b", "key-b")
	if _, err := store.Commit(context.Background(), second); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("Commit(stale global cursor) error = %v, want ErrConflict", err)
	}
	if _, err := store.Payload(context.Background(), second.Action.CanonicalRequestDigest); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("Payload(rejected global CAS) error = %v, want ErrNotFound", err)
	}
	second.ExpectedGlobal.JournalPosition = 2
	secondReceipt, err := store.Commit(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if secondReceipt.Reservation.JournalPosition != 3 || secondReceipt.Outcome.JournalPosition != 4 {
		t.Fatalf("second receipt = %#v", secondReceipt)
	}

	staleRun := filesystemCommitRequest(t, installationID, "run-a", "action-c", "key-c")
	staleRun.ExpectedGlobal.JournalPosition = 4
	if _, err := store.Commit(context.Background(), staleRun); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("Commit(stale run cursor) error = %v, want ErrConflict", err)
	}
	changedReplay := first
	changedReplay.RequestPayload = canonicalFilesystemPayload(t, map[string]any{"request": "changed"})
	if _, err := store.Commit(context.Background(), changedReplay); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("Commit(changed replay) error = %v, want ErrConflict", err)
	}
	feed, cursor, err := store.Feed(context.Background(), journal.GlobalCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 4 || cursor.JournalPosition != 4 {
		t.Fatalf("Feed() = %#v cursor %#v, want two complete commits", feed, cursor)
	}
}

func TestCrashAuthoritativeFilesystemCommitNeverPublishesPartialState(t *testing.T) {
	boundaries := []filesystem.DurableBoundary{
		filesystem.BoundaryBeforeAuthoritativeCommit,
		filesystem.BoundaryAuthoritativeCommitPrepared,
		filesystem.BoundaryAuthoritativeCommitVisible,
		filesystem.BoundaryAuthoritativeCommitResponse,
	}
	for _, boundary := range boundaries {
		t.Run(string(boundary), func(t *testing.T) {
			root := t.TempDir()
			crash := errors.New("crash at " + string(boundary))
			injected := false
			store, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
				if !injected && got == boundary {
					injected = true
					return crash
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			request := filesystemCommitRequest(t, readInstallationID(t, root), "run-a", "action-a", "key-a")
			if _, err := store.Commit(context.Background(), request); !errors.Is(err, crash) {
				t.Fatalf("Commit() error = %v, want injected crash", err)
			}
			if !injected {
				t.Fatal("fault boundary was not reached")
			}
			directFeed, directCursor, err := store.Feed(context.Background(), journal.GlobalCursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			directVisible := boundary == filesystem.BoundaryAuthoritativeCommitVisible ||
				boundary == filesystem.BoundaryAuthoritativeCommitResponse
			if directVisible {
				if len(directFeed) != 2 || directCursor.JournalPosition != 2 {
					t.Fatalf("post-error visible feed = %#v cursor %#v", directFeed, directCursor)
				}
			} else if len(directFeed) != 0 || directCursor.JournalPosition != 0 {
				t.Fatalf("post-error invisible feed = %#v cursor %#v", directFeed, directCursor)
			}

			restarted, err := filesystem.New(root)
			if err != nil {
				t.Fatalf("New(restart) error = %v", err)
			}
			feed, cursor, err := restarted.Feed(context.Background(), journal.GlobalCursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			visible := boundary == filesystem.BoundaryAuthoritativeCommitVisible ||
				boundary == filesystem.BoundaryAuthoritativeCommitResponse
			if visible {
				if len(feed) != 2 || cursor.JournalPosition != 2 {
					t.Fatalf("post-visibility restart feed = %#v cursor %#v", feed, cursor)
				}
				assertFilesystemPayload(t, restarted, request.Action.CanonicalRequestDigest, request.RequestPayload)
				assertFilesystemPayload(t, restarted, request.Outcome.PayloadDigest, request.OutcomePayload)
			} else {
				if len(feed) != 0 || cursor.JournalPosition != 0 {
					t.Fatalf("pre-visibility restart feed = %#v cursor %#v", feed, cursor)
				}
				if _, err := restarted.Payload(context.Background(), request.Action.CanonicalRequestDigest); !errors.Is(err, journal.ErrNotFound) {
					t.Fatalf("Payload(pre-visibility) error = %v, want ErrNotFound", err)
				}
			}
			receipt, err := restarted.Commit(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Created == visible {
				t.Fatalf("Commit(replay) Created = %v, want %v", receipt.Created, !visible)
			}
			feed, cursor, err = restarted.Feed(context.Background(), journal.GlobalCursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(feed) != 2 || cursor.JournalPosition != 2 {
				t.Fatalf("recovered feed = %#v cursor %#v", feed, cursor)
			}
		})
	}
}

func TestCrashAuthoritativeFilesystemCommitNearPayloadLimitDiscardsPreparedStage(t *testing.T) {
	root := t.TempDir()
	crash := errors.New("crash after near-limit commit preparation")
	store, err := filesystem.NewWithOptions(root, filesystem.WithFaultInjector(func(got filesystem.DurableBoundary) error {
		if got == filesystem.BoundaryAuthoritativeCommitPrepared {
			return crash
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := filesystemCommitRequest(t, readInstallationID(t, root), "run-a", "action-a", "key-a")
	nearLimit := []byte("\"" + strings.Repeat("x", journal.MaxPayloadBytes-3) + "\"\n")
	if len(nearLimit) != journal.MaxPayloadBytes {
		t.Fatalf("near-limit fixture bytes = %d, want %d", len(nearLimit), journal.MaxPayloadBytes)
	}
	request.RequestPayload = append([]byte(nil), nearLimit...)
	request.Action.CanonicalRequestDigest = digestFilesystemPayload(request.RequestPayload)
	request.OutcomePayload = append([]byte(nil), nearLimit...)
	request.Outcome.PayloadDigest = digestFilesystemPayload(request.OutcomePayload)
	if _, err := store.Commit(context.Background(), request); !errors.Is(err, crash) {
		t.Fatalf("Commit() error = %v, want injected crash", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "journal", "commit-staging"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("prepared staging entries = %v error %v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= int64(2*journal.MaxPayloadBytes) {
		t.Fatalf("prepared encoded record size = %d, want base64-expanded size above %d", info.Size(), 2*journal.MaxPayloadBytes)
	}

	restarted, err := filesystem.New(root)
	if err != nil {
		t.Fatalf("New(restart near payload limit) error = %v", err)
	}
	receipt, err := restarted.Commit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Created {
		t.Fatalf("Commit(retry) = %#v, want newly created receipt", receipt)
	}
	assertFilesystemPayload(t, restarted, request.Action.CanonicalRequestDigest, nearLimit)
	reopened, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	assertFilesystemPayload(t, reopened, request.Outcome.PayloadDigest, nearLimit)
}

func TestAuthoritativeFilesystemCommitCorruptionSymlinkDuplicateAndUnknownFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "corrupt commit",
			mutate: func(t *testing.T, root string) {
				entries, err := os.ReadDir(filepath.Join(root, "journal", "commits"))
				if err != nil || len(entries) != 1 {
					t.Fatalf("commit entries = %v error %v", entries, err)
				}
				if err := os.WriteFile(filepath.Join(root, "journal", "commits", entries[0].Name()), []byte("{\"corrupt\":true}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink commit",
			mutate: func(t *testing.T, root string) {
				if err := os.Symlink(filepath.Join(root, "journal", "manifest.json"), filepath.Join(root, "journal", "commits", "symlink.json")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unknown staging entry",
			mutate: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "journal", "commit-staging", "unknown"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed staging entry",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "journal", "commit-staging", ".commit-not-an-action-hash-1.tmp")
				if err := os.WriteFile(path, []byte("incomplete"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := filesystem.New(root)
			if err != nil {
				t.Fatal(err)
			}
			request := filesystemCommitRequest(t, readInstallationID(t, root), "run-a", "action-a", "key-a")
			if _, err := store.Commit(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, root)
			if _, err := filesystem.New(root); !errors.Is(err, controlplane.ErrCorruptStore) {
				t.Fatalf("New(corrupt authoritative commit) error = %v, want ErrCorruptStore", err)
			}
		})
	}
}

func TestAuthoritativeFilesystemCommitRejectsStructurallyValidDuplicateIdentity(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	installationID := readInstallationID(t, root)
	first := filesystemCommitRequest(t, installationID, "run-a", "action-a", "key-a")
	if _, err := store.Commit(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := filesystemCommitRequest(t, installationID, "run-b", "action-b", "key-b")
	second.ExpectedGlobal.JournalPosition = 2
	if _, err := store.Commit(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.New(root); err != nil {
		t.Fatalf("New(structurally valid second commit) error = %v", err)
	}

	path := authoritativeCommitFixturePath(root, second.Action.ID)
	record := readAuthoritativeCommitFixture(t, path)
	record.Action.IdempotencyKey = first.Action.IdempotencyKey
	actionDigest, err := journal.Digest(record.Action)
	if err != nil {
		t.Fatal(err)
	}
	record.Reservation.PayloadDigest = actionDigest
	writeAuthoritativeCommitFixture(t, path, record)
	if _, err := filesystem.New(root); !errors.Is(err, controlplane.ErrCorruptStore) {
		t.Fatalf("New(duplicate idempotency identity) error = %v, want ErrCorruptStore", err)
	}
}

type authoritativeCommitFixture struct {
	SchemaVersion  string         `json:"schema_version"`
	Action         journal.Action `json:"action"`
	Reservation    journal.Event  `json:"reservation"`
	Outcome        journal.Event  `json:"outcome"`
	RequestPayload []byte         `json:"request_payload"`
	OutcomePayload []byte         `json:"outcome_payload"`
}

func authoritativeCommitFixturePath(root, actionID string) string {
	return filepath.Join(
		root, "journal", "commits", fmt.Sprintf("%x.json", sha256.Sum256([]byte(actionID))),
	)
}

func readAuthoritativeCommitFixture(t *testing.T, path string) authoritativeCommitFixture {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record authoritativeCommitFixture
	if err := json.Unmarshal(encoded, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func writeAuthoritativeCommitFixture(t *testing.T, path string, record authoritativeCommitFixture) {
	t.Helper()
	encoded, err := journal.CanonicalJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func directoryEntryCount(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func assertDirectoryEntryCount(t *testing.T, path string, want int) {
	t.Helper()
	if got := directoryEntryCount(t, path); got != want {
		t.Fatalf("directory %q entries = %d, want %d", path, got, want)
	}
}

func filesystemCommitRequest(
	t *testing.T,
	installationID, runID, actionID, key string,
) journal.CommitRequest {
	t.Helper()
	requestPayload := canonicalFilesystemPayload(t, map[string]any{
		"action_id":      actionID,
		"control_run_id": runID,
		"principal":      "principal-a",
		"project":        "project-a",
		"request":        "admit",
	})
	outcomePayload := canonicalFilesystemPayload(t, map[string]any{
		"decision": "admit",
		"weight":   1,
	})
	return journal.CommitRequest{
		Action: journal.Action{
			ID: actionID, ControlRunID: runID, TaskID: "task-a", AttemptID: "attempt-a",
			Kind: journal.KindDispatch, GraphRevision: 1, ExpectedProjection: 1,
			CanonicalRequestDigest: digestFilesystemPayload(requestPayload), IdempotencyKey: key,
		},
		ExpectedRun:    journal.NewRunCursor(installationID, runID),
		ExpectedGlobal: journal.NewGlobalCursor(installationID),
		RequestPayload: requestPayload,
		Outcome: journal.Event{
			ID: "outcome-" + actionID, ControlRunID: runID, ActionID: actionID,
			Kind: journal.EventActionResult, PayloadDigest: digestFilesystemPayload(outcomePayload),
			OccurredAt: time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		},
		OutcomePayload: outcomePayload,
	}
}

func canonicalFilesystemPayload(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := journal.CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func digestFilesystemPayload(value []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(value))
}

func assertFilesystemPayload(t *testing.T, store journal.AuthoritativeStore, digest string, want []byte) {
	t.Helper()
	got, err := store.Payload(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Payload(%q) = %q, want %q", digest, got, want)
	}
	if len(got) != 0 {
		got[0] ^= 0xff
	}
	again, err := store.Payload(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, want) {
		t.Fatalf("Payload(%q) was mutable: %q, want %q", digest, again, want)
	}
}

func digest(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}

func readInstallationID(t *testing.T, root string) string {
	t.Helper()
	var manifest struct {
		InstallationID string `json:"installation_id"`
		SchemaVersion  uint32 `json:"schema_version"`
	}
	encoded, err := os.ReadFile(filepath.Join(root, "journal", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.InstallationID == "" || manifest.SchemaVersion != journal.SchemaVersion {
		t.Fatalf("manifest = %#v", manifest)
	}
	return manifest.InstallationID
}
