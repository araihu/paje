package codechangehatchet

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

const approvalPayload = `{
  "run_id": "run-123",
  "artifact_digest": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "approved": true,
  "actor": "reviewer@example.test",
  "decided_at": "2026-07-24T12:00:00Z"
}`

func TestDurableApprovalGateWaitsDecodesAndValidatesBinding(t *testing.T) {
	waiter := &fakeEventWaiter{event: rawEvent{payload: []byte(approvalPayload)}}
	gate := newDurableApprovalGate(waiter)

	got, err := gate.RequestApproval(context.Background(), validApprovalRequest())
	if err != nil {
		t.Fatalf("request approval: %v", err)
	}
	if waiter.key != "paje:approval:run-123" || waiter.expression != "" {
		t.Fatalf("wait = (%q, %q)", waiter.key, waiter.expression)
	}
	if got.RunID != "run-123" || got.ArtifactDigest != strings.Repeat("b", 64) ||
		!got.Approved || got.Actor != "reviewer@example.test" ||
		!got.DecidedAt.Equal(time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("decision = %#v", got)
	}
}

func TestDurableApprovalGateRejectsMalformedEvent(t *testing.T) {
	gate := newDurableApprovalGate(&fakeEventWaiter{event: rawEvent{payload: []byte(`{"run_id":`)}})

	_, err := gate.RequestApproval(context.Background(), validApprovalRequest())
	if err == nil || !strings.Contains(err.Error(), "decode approval event") {
		t.Fatalf("error = %v, want decode failure", err)
	}
}

func TestDurableApprovalGateReturnsBoundDecline(t *testing.T) {
	payload := `{
  "run_id": "run-123",
  "artifact_digest": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "approved": false,
  "actor": "reviewer@example.test",
  "decided_at": "2026-07-24T12:00:00Z",
  "reason": "needs changes"
}`
	gate := newDurableApprovalGate(&fakeEventWaiter{event: rawEvent{payload: []byte(payload)}})

	got, err := gate.RequestApproval(context.Background(), validApprovalRequest())
	if err != nil {
		t.Fatalf("decline: %v", err)
	}
	if got.Approved || got.Reason != "needs changes" {
		t.Fatalf("decision = %#v", got)
	}
}

func TestDurableApprovalGateRejectsMismatchedDecision(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantError string
	}{
		{
			name:      "run ID",
			payload:   strings.Replace(approvalPayload, `"run-123"`, `"run-456"`, 1),
			wantError: "run ID does not match request",
		},
		{
			name:      "artifact digest",
			payload:   strings.Replace(approvalPayload, strings.Repeat("b", 64), strings.Repeat("c", 64), 1),
			wantError: "artifact digest does not match request",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gate := newDurableApprovalGate(&fakeEventWaiter{event: rawEvent{payload: []byte(test.payload)}})

			_, err := gate.RequestApproval(context.Background(), validApprovalRequest())
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestDurableApprovalGatePreservesCanceledWait(t *testing.T) {
	gate := newDurableApprovalGate(&fakeEventWaiter{err: context.Canceled})

	_, err := gate.RequestApproval(context.Background(), validApprovalRequest())
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "wait for approval event") {
		t.Fatalf("error = %v, want wrapped cancellation", err)
	}
}

func validApprovalRequest() approval.Request {
	return approval.Request{
		RunID: "run-123", TemplateID: "code-change@v1",
		Repository: "https://github.com/araihu/paje.git",
		BaseSHA:    strings.Repeat("a", 40), TargetBranch: "main",
		PublicationMode: "pull_request", PublicationBranch: "paje/code-change/run-123",
		ArtifactDigest: strings.Repeat("b", 64), Description: "change it",
	}
}

type fakeEventWaiter struct {
	event      hatchet.EventUnmarshaller
	err        error
	key        string
	expression string
}

func (w *fakeEventWaiter) WaitForEvent(key, expression string) (hatchet.EventUnmarshaller, error) {
	w.key = key
	w.expression = expression
	return w.event, w.err
}

type rawEvent struct{ payload []byte }

func (e rawEvent) Unmarshal(destination any) error {
	return json.Unmarshal(e.payload, destination)
}
