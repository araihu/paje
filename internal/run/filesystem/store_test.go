package runfilesystem_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/run"
	runfilesystem "github.com/araihu/paje/internal/run/filesystem"
	"github.com/araihu/paje/internal/template"
)

func TestReserveIsIdempotentAndDetectsHashConflict(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := runfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reservation := validReservation("run-1", "hash-a")
	first, created, err := store.Reserve(context.Background(), reservation)
	if err != nil || !created {
		t.Fatalf("first Reserve() = created %v err %v", created, err)
	}
	reservation.NewRunID = "run-2"
	second, created, err := store.Reserve(context.Background(), reservation)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("repeat Reserve() = %#v created %v err %v", second, created, err)
	}
	reservation.InputHash = "hash-b"
	if _, _, err := store.Reserve(context.Background(), reservation); !errors.Is(err, run.ErrIdempotencyConflict) {
		t.Fatalf("conflicting Reserve() error = %v", err)
	}
	assertNoTemps(t, root)
}

func TestSaveUsesCompareAndSwapAndReopensNestedRecord(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := runfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Reserve(context.Background(), validReservation("run-nested", "hash"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := run.Transition(record, run.StatusResolving, record.UpdatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(context.Background(), next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 {
		t.Fatalf("version = %d, want 2", saved.Version)
	}
	if _, err := store.Save(context.Background(), next, 1); !errors.Is(err, run.ErrVersionConflict) {
		t.Fatalf("second Save() error = %v", err)
	}

	next, err = run.Transition(saved, run.StatusExecuting, saved.UpdatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	next.BaseSHA = strings.Repeat("a", 40)
	next.MemorySnapshot = []memory.Memory{{ID: "memory-1", Content: "nested", Metadata: map[string]string{"scope": "run"}}}
	ref := artifact.Reference{RunID: next.ID, Digest: strings.Repeat("b", 64), Size: 123}
	next.Artifact = &ref
	decision := approval.Result{RunID: next.ID, ArtifactDigest: ref.Digest, Approved: true, Actor: "alice", DecidedAt: next.UpdatedAt}
	next.Approval = &decision
	publication := publisher.Result{Provider: "github", Branch: "paje/" + next.ID, CommitSHA: strings.Repeat("c", 40), PullRequestID: "42", PullRequestURL: "https://example.test/pull/42"}
	next.Publication = &publication
	next.Stages = []run.StageResult{{Name: "execute", Status: run.StageWarning, StartedAt: next.CreatedAt, FinishedAt: next.UpdatedAt, Attempts: 2, Evidence: map[string]string{"command": "go test"}, Failure: &run.Failure{Stage: "verify", Class: run.FailureVerification, Retryable: false, Diagnostic: "failed", CauseCode: "exit_1"}}}
	saved, err = store.Save(context.Background(), next, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertNoTemps(t, root)

	reopened, err := runfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, saved) {
		t.Fatalf("reopened record differs:\n got %#v\nwant %#v", loaded, saved)
	}
	loaded.MemorySnapshot[0].Metadata["scope"] = "mutated"
	again, err := reopened.Load(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.MemorySnapshot[0].Metadata["scope"] != "run" {
		t.Fatal("Load() exposed retained nested memory metadata")
	}
}

func TestBlankIdempotencyKeyCreatesDistinctCallerRunIDsWithoutIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := runfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reservation := validReservation("run-1", "hash")
	reservation.IdempotencyKey = ""
	if _, created, err := store.Reserve(context.Background(), reservation); err != nil || !created {
		t.Fatalf("Reserve(run-1) created %v err %v", created, err)
	}
	reservation.NewRunID = "run-2"
	if _, created, err := store.Reserve(context.Background(), reservation); err != nil || !created {
		t.Fatalf("Reserve(run-2) created %v err %v", created, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "idempotency"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("blank key wrote %d idempotency files", len(entries))
	}
}

func TestStoreRejectsUnsafeRunIDAndCanceledContext(t *testing.T) {
	t.Parallel()
	store, err := runfilesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reservation := validReservation("../escape", "hash")
	if _, _, err := store.Reserve(context.Background(), reservation); err == nil {
		t.Fatal("unsafe run ID accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Reserve(ctx, validReservation("run-1", "hash")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Reserve() error = %v", err)
	}
}

func validReservation(id, hash string) run.Reservation {
	return run.Reservation{
		NewRunID: id, Template: template.ID{Name: "code-change", Version: 1},
		IdempotencyKey: "request-1", InputHash: hash, Input: json.RawMessage(`{"task":"change"}`),
		RepositoryURI: "https://example.test/repo.git", BaseRef: "main", PublicationMode: "pull_request",
		CreatedAt: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
	}
}

func assertNoTemps(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("temporary file remains: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
