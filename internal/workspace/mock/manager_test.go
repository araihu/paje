package mock_test

import (
	"context"
	"testing"

	"github.com/araihu/paje/internal/workspace/mock"
)

func TestManagerPreparesUniqueWorkspacesAndCleansIdempotently(t *testing.T) {
	t.Parallel()

	manager := mock.NewManager("/mock-root")

	first, err := manager.Prepare(context.Background(), "https://example.test/repo.git", "main")
	if err != nil {
		t.Fatalf("Prepare(first) error = %v", err)
	}
	second, err := manager.Prepare(context.Background(), "https://example.test/repo.git", "feature")
	if err != nil {
		t.Fatalf("Prepare(second) error = %v", err)
	}
	if first.Path() == second.Path() {
		t.Fatalf("Prepare() returned duplicate path %q", first.Path())
	}

	if err := first.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup(first) error = %v", err)
	}
	if err := first.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup(first, again) error = %v", err)
	}

	prepared := manager.Prepared()
	if len(prepared) != 2 {
		t.Fatalf("Prepared() returned %d records, want 2", len(prepared))
	}
	if prepared[0].RepoURI != "https://example.test/repo.git" || prepared[0].Branch != "main" {
		t.Errorf("first preparation = %#v", prepared[0])
	}
	if prepared[0].CleanupCount != 1 {
		t.Errorf("first cleanup count = %d, want 1", prepared[0].CleanupCount)
	}

	prepared[0].RepoURI = "mutated"
	if manager.Prepared()[0].RepoURI == "mutated" {
		t.Error("Prepared() exposed internal state")
	}
}
