package admission_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
)

func TestHandoffIssueGrantAcknowledgeAndDisclosure(t *testing.T) {
	store := newStore(t)
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	subject := handoffSubject()
	issueRequest := admission.HandoffRequest{
		ControlRunID: "run-a", Identity: identity("issue-action", "issue-key", "issue-outcome"),
		Subject: subject,
	}
	issueRequest.Identity.GraphRevision = subject.GraphRevision
	issued, err := service.IssueHandoff(context.Background(), issueRequest)
	if err != nil {
		t.Fatal(err)
	}
	grantRequest := admission.HandoffRequest{
		ControlRunID: "run-a", Identity: identity("grant-action", "grant-key", "grant-outcome"),
		Subject: subject, PredecessorReceiptID: issued.Handoff.ReceiptID,
	}
	grantRequest.Identity.GraphRevision = subject.GraphRevision
	granted, err := service.GrantHandoff(context.Background(), grantRequest)
	if err != nil {
		t.Fatal(err)
	}
	ackRequest := admission.HandoffRequest{
		ControlRunID: "run-a", Identity: identity("ack-action", "ack-key", "ack-outcome"),
		Subject: subject, PredecessorReceiptID: granted.Handoff.ReceiptID,
	}
	ackRequest.Identity.GraphRevision = subject.GraphRevision
	acknowledged, err := service.AcknowledgeHandoff(context.Background(), ackRequest)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Operation != admission.OperationHandoffIssue ||
		granted.Operation != admission.OperationHandoffGrant ||
		acknowledged.Operation != admission.OperationHandoffAcknowledge ||
		issued.Commit.Outcome.JournalPosition+2 != granted.Commit.Outcome.JournalPosition ||
		granted.Commit.Outcome.JournalPosition+2 != acknowledged.Commit.Outcome.JournalPosition {
		t.Fatalf("handoff receipts = %#v %#v %#v", issued, granted, acknowledged)
	}
	disclosure, err := service.EvidenceDisclosure(context.Background(), "run-a", acknowledged.Handoff.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disclosure.Authoritative || disclosure.State != admission.HandoffAcknowledged {
		t.Fatalf("disclosure = %#v", disclosure)
	}
	disclosure.State = admission.HandoffIssued
	again, err := service.EvidenceDisclosure(context.Background(), "run-a", acknowledged.Handoff.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.State != admission.HandoffAcknowledged {
		t.Fatalf("editing disclosure mutated authority: %#v", again)
	}
}

func TestHandoffRejectsFabricationMutationAndCrossScope(t *testing.T) {
	store := newStore(t)
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	subject := handoffSubject()
	issue := admission.HandoffRequest{
		ControlRunID: "run-a", Identity: identity("same-action", "same-key", "same-outcome"),
		Subject: subject,
	}
	issue.Identity.GraphRevision = subject.GraphRevision
	issued, err := service.IssueHandoff(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	fabricated := admission.HandoffRequest{
		ControlRunID: "run-a", Identity: identity("grant-action", "grant-key", "grant-outcome"),
		Subject: subject, PredecessorReceiptID: "fabricated",
	}
	fabricated.Identity.GraphRevision = subject.GraphRevision
	if _, err := service.GrantHandoff(context.Background(), fabricated); !errors.Is(err, admission.ErrNotFound) && !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("fabricated grant error = %v, want fail-closed not-found/conflict", err)
	}
	changed := issue
	changed.Subject.EdgeID = "changed-edge"
	if _, err := service.IssueHandoff(context.Background(), changed); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("changed issue error = %v, want ErrConflict", err)
	}
	crossRun := admission.HandoffRequest{
		ControlRunID: "run-b", Identity: identity("same-action", "same-key", "same-outcome"),
		Subject: subject,
	}
	crossRun.Identity.GraphRevision = subject.GraphRevision
	if _, err := service.IssueHandoff(context.Background(), crossRun); err != nil {
		t.Fatalf("same external IDs in another run error = %v", err)
	}
	crossEdge := admission.HandoffRequest{
		ControlRunID: "run-a", Identity: identity("grant-2", "grant-key-2", "grant-outcome-2"),
		Subject: subject, PredecessorReceiptID: issued.Handoff.ReceiptID,
	}
	crossEdge.Identity.GraphRevision = subject.GraphRevision
	crossEdge.Subject.EdgeID = "other-edge"
	if _, err := service.GrantHandoff(context.Background(), crossEdge); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("cross-edge grant error = %v, want ErrConflict", err)
	}
	mismatchedRevision := admission.HandoffRequest{
		ControlRunID: "run-c", Identity: identity("issue-c", "issue-key-c", "issue-outcome-c"),
		Subject: subject,
	}
	if _, err := service.IssueHandoff(context.Background(), mismatchedRevision); !errors.Is(err, admission.ErrInvalidRecord) {
		t.Fatalf("mismatched graph revision error = %v, want ErrInvalidRecord", err)
	}
}

func handoffSubject() admission.EvidenceHandoffSubject {
	return admission.EvidenceHandoffSubject{
		GraphRevision:  7,
		EdgeID:         "edge-a",
		EvidenceDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Producer: admission.EvidenceEndpoint{
			ProjectID: "project-a", TaskID: "task-a", AttemptID: "attempt-a",
			ActionID: "producer-action", Generation: 3,
		},
		Consumer: admission.EvidenceEndpoint{
			ProjectID: "project-b", TaskID: "task-b", AttemptID: "attempt-b",
			ActionID: "consumer-action", Generation: 4,
		},
	}
}
