package mock_test

import (
	"context"
	"testing"

	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/memory/mock"
)

func TestStoreSaveAndSearch(t *testing.T) {
	t.Parallel()

	store := mock.NewStore([]memory.Memory{{
		ID:       "seed",
		Content:  "Unrelated context",
		Metadata: map[string]string{"app_id": "other"},
	}})
	tags := map[string]string{"app_id": "paje", "kind": "result"}

	if err := store.Save(context.Background(), "Agent finished successfully", tags); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	tags["app_id"] = "changed"

	got, err := store.Search(
		context.Background(),
		"FINISHED",
		10,
		map[string]string{"app_id": "paje"},
	)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search() returned %d memories, want 1", len(got))
	}
	if got[0].Content != "Agent finished successfully" {
		t.Errorf("Content = %q, want saved content", got[0].Content)
	}
	if got[0].Metadata["app_id"] != "paje" {
		t.Errorf("Metadata was not copied defensively: %#v", got[0].Metadata)
	}

	got[0].Metadata["kind"] = "mutated"
	snapshot := store.Memories()
	if snapshot[1].Metadata["kind"] != "result" {
		t.Errorf("Memories() exposed internal metadata: %#v", snapshot[1].Metadata)
	}
}

func TestStoreSearchHonorsLimit(t *testing.T) {
	t.Parallel()

	store := mock.NewStore(nil)
	for _, content := range []string{"one task", "two tasks", "three tasks"} {
		if err := store.Save(context.Background(), content, map[string]string{"app_id": "paje"}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	got, err := store.Search(context.Background(), "task", 2, map[string]string{"app_id": "paje"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Search() returned %d memories, want 2", len(got))
	}
}
