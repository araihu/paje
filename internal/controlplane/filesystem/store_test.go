package filesystem_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/araihu/paje/internal/controlplane"
	"github.com/araihu/paje/internal/controlplane/filesystem"
)

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

func TestRestartFailsClosedForCorruptSymlinkAndUnexpectedEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"corrupt", func(t *testing.T, root string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, "records", "control-1.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, root string) {
			t.Helper()
			target := filepath.Join(root, "outside")
			if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "records", "linked.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"unexpected", func(t *testing.T, root string) {
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
			if _, err := filesystem.New(root); !errors.Is(err, controlplane.ErrCorruptStore) {
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

func digest(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}
