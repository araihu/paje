package runfilesystem_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	runmock "github.com/araihu/paje/internal/run/mock"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/workerprofile"
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

func TestReserveReconcilesRunCommittedBeforeIdempotencyBinding(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := runfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reservation := validReservation("run-original", "hash-a")
	original, _, err := store.Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	removeOnlyIndexFile(t, root)

	reopened, err := runfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	conflict := reservation
	conflict.NewRunID = "run-conflict"
	conflict.InputHash = "hash-b"
	if _, _, err := reopened.Reserve(context.Background(), conflict); !errors.Is(err, run.ErrIdempotencyConflict) {
		t.Fatalf("reconcile conflicting hash error = %v", err)
	}

	retry := reservation
	retry.NewRunID = "run-retry"
	reused, created, err := reopened.Reserve(context.Background(), retry)
	if err != nil || created {
		t.Fatalf("reconciled Reserve() created %v err %v", created, err)
	}
	if reused.ID != original.ID {
		t.Fatalf("reconciled run ID = %q, want %q", reused.ID, original.ID)
	}
	entries, err := os.ReadDir(filepath.Join(root, "idempotency"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("reconciliation wrote %d bindings, want 1", len(entries))
	}
}

func TestReserveRejectsDuplicateOrCorruptReconciliationCandidates(t *testing.T) {
	t.Parallel()
	t.Run("duplicate", func(t *testing.T) {
		root := t.TempDir()
		store, err := runfilesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		reservation := validReservation("run-original", "hash-a")
		record, _, err := store.Reserve(context.Background(), reservation)
		if err != nil {
			t.Fatal(err)
		}
		removeOnlyIndexFile(t, root)
		record.ID = "run-duplicate"
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "runs", "run-duplicate.json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Reserve(context.Background(), reservation); !errors.Is(err, run.ErrIdempotencyConflict) {
			t.Fatalf("duplicate reconciliation error = %v", err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		root := t.TempDir()
		store, err := runfilesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "runs", "corrupt.json"), []byte(`{"id":`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Reserve(context.Background(), validReservation("run-new", "hash")); err == nil {
			t.Fatal("Reserve() silently ignored corrupt reconciliation candidate")
		}
	})

	t.Run("directory shaped like committed run", func(t *testing.T) {
		root := t.TempDir()
		store, err := runfilesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "runs", "run-directory.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Reserve(context.Background(), validReservation("run-new", "hash")); !errors.Is(err, run.ErrInvalidRecord) {
			t.Fatalf("directory reconciliation error = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("unexpected committed entry", func(t *testing.T) {
		root := t.TempDir()
		store, err := runfilesystem.New(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "runs", "unexpected"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Reserve(context.Background(), validReservation("run-new", "hash")); !errors.Is(err, run.ErrInvalidRecord) {
			t.Fatalf("unexpected entry error = %v, want ErrInvalidRecord", err)
		}
	})
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

func TestFilesystemStoreRoundTripsResolvedWorkerProfileAndBindings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := runfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reservation := validReservation("run-resolved", "hash")
	reservation.Input, err = run.CanonicalInput(json.RawMessage(`{
		"task_description":"change",
		"repository_uri":"https://example.test/repo.git",
		"base_ref":"main",
		"tags":{"app_id":"araihu-paje","user_id":"guilhermecastro"},
		"worker_profile":"codex-go@1",
		"profile":"go",
		"publication":{"mode":"pull_request","provider":"github","target_branch":"main"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(reservation.Input)
	reservation.InputHash = hex.EncodeToString(sum[:])
	record, _, err := store.Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	next, err := run.Transition(record, run.StatusResolving, record.UpdatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	next, err = run.UpsertStage(next, run.StageResult{
		Name: "resolve", Status: run.StageRunning,
		StartedAt: next.UpdatedAt, Attempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolving, err := store.Save(context.Background(), next, record.Version)
	if err != nil {
		t.Fatal(err)
	}
	next = withResolvedStateJSON(t, resolving)
	saved, err := store.Save(context.Background(), next, resolving.Version)
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := runfilesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"worker_profile", "secret_bindings"} {
		if !strings.Contains(string(encoded), `"`+field+`"`) {
			t.Fatalf("filesystem round trip omitted %q: %s", field, encoded)
		}
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
	retried, created, err := store.Reserve(context.Background(), reservation)
	if err != nil || created || retried.ID != "run-1" {
		t.Fatalf("Reserve(run-1 retry) record=%#v created=%v err=%v", retried, created, err)
	}
	conflict := reservation
	conflict.InputHash = "different"
	if _, _, err := store.Reserve(context.Background(), conflict); !errors.Is(err, run.ErrIdempotencyConflict) {
		t.Fatalf("Reserve(run-1 conflict) error=%v, want %v", err, run.ErrIdempotencyConflict)
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

func TestAdaptersCanonicalizeReservationInputIdentically(t *testing.T) {
	t.Parallel()
	reservation := validReservation("run-filesystem", "hash")
	reservation.IdempotencyKey = ""
	reservation.Input = json.RawMessage(" { \"z\" : 2, \"a\" : 1 } ")

	filesystemStore, err := runfilesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fromFilesystem, _, err := filesystemStore.Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	reservation.NewRunID = "run-mock"
	fromMock, _, err := runmock.NewStore().Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"a":1,"z":2}`
	if string(fromFilesystem.Input) != want || string(fromMock.Input) != want {
		t.Fatalf("canonical inputs = filesystem %q mock %q, want %q", fromFilesystem.Input, fromMock.Input, want)
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

func removeOnlyIndexFile(t *testing.T, root string) {
	t.Helper()
	directory := filepath.Join(root, "idempotency")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("idempotency entries = %d, want 1", len(entries))
	}
	if err := os.Remove(filepath.Join(directory, entries[0].Name())); err != nil {
		t.Fatal(err)
	}
}

func withResolvedStateJSON(t *testing.T, record run.Record) run.Record {
	t.Helper()
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Runtime: workerprofile.Runtime{
			Kind: workerprofile.RuntimeOCI, Image: "example.test/worker@sha256:" + strings.Repeat("a", 64),
			Platform: "linux/amd64", Network: workerprofile.NetworkNone, ReadOnlyRoot: true,
		},
		Resources: workerprofile.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 64},
		Harness:   workerprofile.Harness{ID: "codex", Version: "0.144.5"},
		Secrets: []workerprofile.SecretRequirement{{
			Capability: "harness.codex-auth", BindingRevision: 1, Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/codex", Required: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	fields["worker_profile"] = profile
	fields["secret_bindings"] = []secret.BindingRef{{
		Capability: "harness.codex-auth", Revision: 1,
	}}
	encoded, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var result run.Record
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
