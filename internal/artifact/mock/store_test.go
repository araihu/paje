package artifactmock_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/araihu/paje/internal/artifact"
	artifactmock "github.com/araihu/paje/internal/artifact/mock"
	"github.com/araihu/paje/internal/template"
)

func TestStoreSharesReferenceAndDefensiveCopies(t *testing.T) {
	t.Parallel()
	store := artifactmock.NewStore()
	bundle := mockBundle()
	ref, err := store.Save(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	want, err := artifact.ReferenceFor(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if ref != want {
		t.Fatalf("Save ref = %#v, want %#v", ref, want)
	}
	bundle.AgentOutput[0] = 'X'
	loaded, err := store.Load(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.AgentOutput) != "agent output" {
		t.Fatalf("mock leaked input mutation: %q", loaded.AgentOutput)
	}
	loaded.AgentOutput[0] = 'Y'
	again, err := store.Load(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.AgentOutput) != "agent output" {
		t.Fatalf("mock leaked output mutation: %q", again.AgentOutput)
	}
	snapshot := store.Snapshot()
	if len(snapshot.Saves) != 1 || len(snapshot.Loads) != 2 || len(snapshot.Bundles) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestStoreFailuresAndConcurrentAccess(t *testing.T) {
	t.Parallel()
	store := artifactmock.NewStore()
	saveErr := errors.New("save unavailable")
	store.SetSaveError(saveErr)
	if _, err := store.Save(context.Background(), mockBundle()); !errors.Is(err, saveErr) {
		t.Fatalf("save error = %v", err)
	}
	store.SetSaveError(nil)
	ref, err := store.Save(context.Background(), mockBundle())
	if err != nil {
		t.Fatal(err)
	}
	loadErr := errors.New("load unavailable")
	store.SetLoadError(loadErr)
	if _, err := store.Load(context.Background(), ref); !errors.Is(err, loadErr) {
		t.Fatalf("load error = %v", err)
	}
	store.SetLoadError(nil)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Load(context.Background(), ref); err != nil {
				t.Errorf("Load: %v", err)
			}
		}()
	}
	wg.Wait()
}

func mockBundle() artifact.Bundle {
	return artifact.Bundle{
		Manifest:     artifact.Manifest{RunID: "run-123", Template: template.ID{Name: "code-change", Version: 1}, Repository: "https://example.test/repo.git", BaseSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40)},
		ChangesPatch: []byte("patch"), AgentOutput: []byte("agent output"), ExecutionMetadata: json.RawMessage(`{"completed":true,"status":"done"}`), Preflight: map[string]string{"base_sha": "abc"},
	}
}
