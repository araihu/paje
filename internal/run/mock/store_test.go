package runmock_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/run"
	runmock "github.com/araihu/paje/internal/run/mock"
	"github.com/araihu/paje/internal/template"
)

func TestStoreMatchesReservationCASAndDefensiveCopyBehavior(t *testing.T) {
	t.Parallel()
	store := runmock.NewStore()
	reservation := reservation("run-1")
	record, created, err := store.Reserve(context.Background(), reservation)
	if err != nil || !created {
		t.Fatalf("Reserve() created %v err %v", created, err)
	}
	reservation.NewRunID = "run-other"
	reused, created, err := store.Reserve(context.Background(), reservation)
	if err != nil || created || reused.ID != record.ID {
		t.Fatalf("repeat Reserve() = %#v created %v err %v", reused, created, err)
	}
	next, err := run.Transition(record, run.StatusResolving, record.UpdatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	next.Stages = []run.StageResult{{Name: "resolve", Status: run.StageRunning, StartedAt: next.CreatedAt, Attempts: 1, Evidence: map[string]string{"query": "original"}}}
	saved, err := store.Save(context.Background(), next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), next, 1); !errors.Is(err, run.ErrVersionConflict) {
		t.Fatalf("repeat Save() error = %v", err)
	}
	next.Stages[0].Evidence["query"] = "caller-mutated"
	saved.Stages[0].Evidence["query"] = "result-mutated"
	loaded, err := store.Load(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Stages[0].Evidence["query"] != "original" {
		t.Fatalf("mock retained mutation: %#v", loaded.Stages)
	}
}

func TestConfiguredFailuresAndConcurrency(t *testing.T) {
	t.Parallel()
	want := errors.New("unavailable")
	store := runmock.NewStore(runmock.Config{ReserveError: want, LoadError: want, SaveError: want})
	if _, _, err := store.Reserve(context.Background(), reservation("run-1")); !errors.Is(err, want) {
		t.Fatalf("Reserve() error = %v", err)
	}
	store.SetReserveError(nil)
	record, _, err := store.Reserve(context.Background(), reservation("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), record.ID); !errors.Is(err, want) {
		t.Fatalf("Load() error = %v", err)
	}
	store.SetLoadError(nil)
	store.SetSaveError(nil)

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Load(context.Background(), record.ID); err != nil {
				t.Errorf("Load() error = %v", err)
			}
		}()
	}
	wg.Wait()
}

func reservation(id string) run.Reservation {
	return run.Reservation{
		NewRunID: id, Template: template.ID{Name: "code-change", Version: 1}, IdempotencyKey: "key",
		InputHash: "hash", Input: json.RawMessage(`{"task":"change"}`), RepositoryURI: "repo",
		BaseRef: "main", PublicationMode: "pull_request", CreatedAt: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
	}
}
