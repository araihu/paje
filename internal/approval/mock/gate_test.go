package mock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/approval/mock"
)

func TestGateRecordsRequestsAndReturnsConfiguredDecision(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("approval unavailable")
	gate := mock.NewGate(true, wantErr)
	req := approval.Request{
		TaskID:      "task-1",
		Description: "Apply the patch",
		Diff:        "diff --git a/a b/a",
	}

	approved, err := gate.RequestApproval(context.Background(), req)
	if !approved {
		t.Error("RequestApproval() approved = false, want true")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("RequestApproval() error = %v, want %v", err, wantErr)
	}

	requests := gate.Requests()
	if len(requests) != 1 || requests[0] != req {
		t.Fatalf("Requests() = %#v, want %#v", requests, []approval.Request{req})
	}
	requests[0].TaskID = "mutated"
	if gate.Requests()[0].TaskID != "task-1" {
		t.Error("Requests() exposed internal state")
	}
}
