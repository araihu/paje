package mock

import (
	"context"
	"errors"
	"testing"

	"github.com/araihu/paje/internal/workerprofile"
)

func TestRegistryRecordsRequestsAndReturnsDefensiveCopies(t *testing.T) {
	snapshot, err := workerprofile.Canonicalize(validProfile())
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(snapshot)

	got, err := registry.Resolve(context.Background(), snapshot.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	got.Tools[0].Probe.Args[0] = "mutated"
	again, err := registry.Resolve(context.Background(), snapshot.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if again.Tools[0].Probe.Args[0] == "mutated" {
		t.Fatal("mock returned aliased snapshot")
	}
	requests := registry.Requests()
	if len(requests) != 2 || requests[0] != snapshot.Metadata || requests[1] != snapshot.Metadata {
		t.Fatalf("requests = %#v", requests)
	}
	requests[0] = workerprofile.ProfileID{}
	if registry.Requests()[0] != snapshot.Metadata {
		t.Fatal("Requests returned aliased state")
	}
}

func TestRegistrySupportsConfiguredErrors(t *testing.T) {
	snapshot, err := workerprofile.Canonicalize(validProfile())
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(snapshot)
	want := errors.New("unavailable")
	registry.SetError(snapshot.Metadata, want)
	if _, err := registry.Resolve(context.Background(), snapshot.Metadata); !errors.Is(err, want) {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func validProfile() workerprofile.Snapshot {
	return workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "host-dev", Revision: 1},
		Runtime:    workerprofile.Runtime{Kind: workerprofile.RuntimeHost},
		Harness:    workerprofile.Harness{ID: "codex", Version: "0.144.5"},
		Tools: []workerprofile.Tool{{
			Name: "git", Version: "2.52.0",
			Probe: workerprofile.Probe{Executable: "git", Args: []string{"--version"}, OutputContains: "git version"},
		}},
	}
}
