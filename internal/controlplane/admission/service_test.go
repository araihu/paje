package admission_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
	"github.com/araihu/paje/internal/controlplane/journal"
)

func TestJournalOnlyRebuildAndTwoServiceQuotaCAS(t *testing.T) {
	store := newStore(t)
	policy := admission.Policy{
		Version:           1,
		InstallationLimit: 1,
		PrincipalLimit:    2,
		RunLimit:          2,
		ProjectLimit:      2,
		PrimitiveLimit:    2,
	}
	first := newService(t, store, policy, fixedClock(time.Unix(100, 0)))
	second := newService(t, store, policy, fixedClock(time.Unix(100, 0)))

	a := admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "a")
	b := admissionRequest("run-b", "admission-b", "principal-b", "project-b", "work-b", "b")
	for _, fixture := range []struct {
		service *admission.Service
		request admission.AdmissionRequest
	}{{first, a}, {second, b}} {
		if _, err := fixture.service.Reserve(context.Background(), fixture.request); err != nil {
			t.Fatal(err)
		}
		fixture.request.Identity = nextIdentity(fixture.request.Identity, "enqueue")
		if _, err := fixture.service.Enqueue(context.Background(), fixture.request); err != nil {
			t.Fatal(err)
		}
	}
	a.Identity = nextIdentity(a.Identity, "admit")
	b.Identity = nextIdentity(b.Identity, "admit")

	start := make(chan struct{})
	results := make(chan admission.AdmissionReceipt, 2)
	errorsCh := make(chan error, 2)
	for _, fixture := range []struct {
		service *admission.Service
		request admission.AdmissionRequest
	}{{first, a}, {second, b}} {
		go func() {
			<-start
			receipt, err := fixture.service.Admit(context.Background(), fixture.request)
			results <- receipt
			errorsCh <- err
		}()
	}
	close(start)
	states := map[admission.AdmissionState]int{}
	limiters := map[admission.QuotaKind]int{}
	for range 2 {
		receipt := <-results
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
		states[receipt.Admission.State]++
		limiters[receipt.Admission.LimitingQuota]++
		if receipt.Admission.State == admission.AdmissionDeferred {
			if receipt.Backpressure == nil ||
				receipt.Backpressure.LimitingQuota != admission.QuotaInstallation ||
				receipt.Backpressure.NextEligibility != admission.EligibilityQuotaAvailable {
				t.Fatalf("backpressure = %#v, want typed installation quota condition", receipt.Backpressure)
			}
		}
	}
	if states[admission.AdmissionAdmitted] != 1 || states[admission.AdmissionDeferred] != 1 {
		t.Fatalf("states = %#v, want one admitted and one deferred", states)
	}
	if limiters[admission.QuotaInstallation] != 1 {
		t.Fatalf("limiters = %#v, want installation limiter", limiters)
	}

	restarted := newService(t, store, policy, fixedClock(time.Unix(100, 0)))
	for _, request := range []admission.AdmissionRequest{a, b} {
		got, err := restarted.Admission(context.Background(), request.ControlRunID, request.Subject.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != admission.AdmissionAdmitted && got.State != admission.AdmissionDeferred {
			t.Fatalf("rebuilt state = %q", got.State)
		}
	}
}

func TestReplayBindsVersionedAdmissionPolicy(t *testing.T) {
	store := newStore(t)
	firstPolicy := generousPolicy()
	secondPolicy := generousPolicy()
	secondPolicy.Version++
	first := newService(t, store, firstPolicy, fixedClock(time.Unix(100, 0)))
	second := newService(t, store, secondPolicy, fixedClock(time.Unix(100, 0)))
	request := admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "policy")
	receipt, err := first.Reserve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Admission.PolicyVersion != firstPolicy.Version {
		t.Fatalf("policy version = %d, want %d", receipt.Admission.PolicyVersion, firstPolicy.Version)
	}
	if _, err := second.Reserve(context.Background(), request); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("Reserve(changed policy version) error = %v, want ErrConflict", err)
	}
}

func TestReplayRecoversLostResponseAndChangedReplayConflicts(t *testing.T) {
	base := newStore(t)
	lossy := &responseLossStore{AuthoritativeStore: base}
	service := newService(t, lossy, generousPolicy(), fixedClock(time.Unix(100, 0)))
	request := admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "replay")

	first, err := service.Reserve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Created {
		t.Fatal("response-loss recovery must report an immutable replay")
	}
	second, err := service.Reserve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Commit.Action != first.Commit.Action ||
		second.Commit.Reservation != first.Commit.Reservation || second.Commit.Outcome != first.Commit.Outcome {
		t.Fatalf("replay = %#v, want immutable %#v", second, first)
	}

	changed := request
	changed.Subject.WorkID = "changed-work"
	if _, err := service.Reserve(context.Background(), changed); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("changed replay error = %v, want ErrConflict", err)
	}
}

func TestReplayRecoversResponseLossAcrossEveryAuthoritativeTransition(t *testing.T) {
	base := newStore(t)
	lossy := &responseLossEveryCommitStore{
		AuthoritativeStore: base,
		lost:               make(map[string]bool),
	}
	clock := &testClock{now: time.Unix(100, 0).UTC()}
	policy := generousPolicy()
	policy.InstallationLimit = 1
	service, err := admission.New(admission.Dependencies{
		Store: lossy, Policy: policy, Clock: clock.Now, ScannerAuthority: "scanner-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertReplay := func(created bool, operation admission.SemanticOperation) {
		t.Helper()
		if created {
			t.Fatalf("%s response-loss recovery reported a new receipt", operation)
		}
	}

	first := admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "loss-a")
	receipt, err := service.Reserve(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(receipt.Created, receipt.Operation)
	first.Identity = nextIdentity(first.Identity, "enqueue")
	receipt, err = service.Enqueue(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(receipt.Created, receipt.Operation)
	first.Identity = nextIdentity(first.Identity, "admit")
	receipt, err = service.Admit(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(receipt.Created, receipt.Operation)

	second := admissionRequest("run-b", "admission-b", "principal-b", "project-b", "work-b", "loss-b")
	for _, transition := range []func(context.Context, admission.AdmissionRequest) (admission.AdmissionReceipt, error){
		service.Reserve, service.Enqueue, service.Admit,
	} {
		receipt, err = transition(context.Background(), second)
		if err != nil {
			t.Fatal(err)
		}
		assertReplay(receipt.Created, receipt.Operation)
		second.Identity = nextIdentity(second.Identity, "next")
	}
	if receipt.Operation != admission.OperationBackpressureDefer {
		t.Fatalf("second admission operation = %s, want backpressure_defer", receipt.Operation)
	}
	receipt, err = service.Release(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(receipt.Created, receipt.Operation)
	if receipt.Operation != admission.OperationQueueRelease {
		t.Fatalf("deferred release operation = %s, want queue_release", receipt.Operation)
	}
	first.Identity = nextIdentity(first.Identity, "release")
	receipt, err = service.Release(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(receipt.Created, receipt.Operation)

	lease := leaseRequest("run-a", "lease-a", "resource-a", time.Unix(200, 0), "loss-lease")
	leaseReceipt, err := service.AcquireLease(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(leaseReceipt.Created, leaseReceipt.Operation)
	lease.Identity = nextIdentity(lease.Identity, "renew")
	lease.Identity.Generation++
	lease.ExpiresAt = time.Unix(300, 0).UTC()
	leaseReceipt, err = service.RenewLease(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(leaseReceipt.Created, leaseReceipt.Operation)
	lease.Identity = nextIdentity(lease.Identity, "release")
	leaseReceipt, err = service.ReleaseLease(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(leaseReceipt.Created, leaseReceipt.Operation)

	expiring := leaseRequest("run-b", "lease-b", "resource-b", time.Unix(150, 0), "loss-expiry")
	leaseReceipt, err = service.AcquireLease(context.Background(), expiring)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(leaseReceipt.Created, leaseReceipt.Operation)
	clock.Set(expiring.ExpiresAt)
	expiring.Identity = nextIdentity(expiring.Identity, "expire")
	leaseReceipt, err = service.ExpireLease(context.Background(), expiring)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(leaseReceipt.Created, leaseReceipt.Operation)

	handoff := admission.HandoffRequest{
		ControlRunID: "run-a", Identity: identity("loss-issue", "loss-issue-key", "loss-issue-outcome"),
		Subject: handoffSubject(),
	}
	handoff.Identity.GraphRevision = handoff.Subject.GraphRevision
	issued, err := service.IssueHandoff(context.Background(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(issued.Created, issued.Operation)
	handoff.Identity = nextIdentity(handoff.Identity, "grant")
	handoff.PredecessorReceiptID = issued.Handoff.ReceiptID
	granted, err := service.GrantHandoff(context.Background(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(granted.Created, granted.Operation)
	handoff.Identity = nextIdentity(handoff.Identity, "ack")
	handoff.PredecessorReceiptID = granted.Handoff.ReceiptID
	acknowledged, err := service.AcknowledgeHandoff(context.Background(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(acknowledged.Created, acknowledged.Operation)

	recovery := admission.RecoveryIdentity{
		ControlRunID: "run-a", ActionID: "loss-target", Generation: 1,
		SubjectDigest: "sha256:abababababababababababababababababababababababababababababababab",
	}
	started, err := service.StartObservation(context.Background(), admission.RecoveryRequest{
		Identity: identity("loss-start", "loss-start-key", "loss-start-outcome"), Recovery: recovery,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(started.Created, started.Operation)
	canceled, err := service.CancelOrFence(context.Background(), admission.FenceRequest{
		Identity: identity("loss-cancel", "loss-cancel-key", "loss-cancel-outcome"), Recovery: recovery,
		Proof: admission.FenceProof{Status: admission.ProviderCanceled, ReceiptID: "loss-cancel-receipt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(canceled.Created, canceled.Operation)
	recovery.Generation++
	started, err = service.StartObservation(context.Background(), admission.RecoveryRequest{
		Identity: identity("loss-start-2", "loss-start-key-2", "loss-start-outcome-2"), Recovery: recovery,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(started.Created, started.Operation)
	applied, err := service.ScannerApply(context.Background(), admission.ScannerApplyRequest{
		Identity: identity("loss-apply", "loss-apply-key", "loss-apply-outcome"), Recovery: recovery,
		ScannerAuthority: "scanner-a", Fact: admission.ProviderFact{
			Status: admission.ProviderEffectObserved, ReceiptID: "loss-provider-receipt",
			SubjectDigest: recovery.SubjectDigest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertReplay(applied.Created, applied.Operation)
}

func TestReplayBindsEverySemanticIdentityField(t *testing.T) {
	store := newStore(t)
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	original := admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "binding")
	if _, err := service.Reserve(context.Background(), original); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*admission.AdmissionRequest)
	}{
		{"action", func(r *admission.AdmissionRequest) { r.Identity.ActionID += "-changed" }},
		{"idempotency", func(r *admission.AdmissionRequest) { r.Identity.IdempotencyKey += "-changed" }},
		{"outcome id", func(r *admission.AdmissionRequest) { r.Identity.OutcomeEventID += "-changed" }},
		{"outcome kind", func(r *admission.AdmissionRequest) { r.Identity.OutcomeKind = journal.EventActionAmbiguous }},
		{"subject", func(r *admission.AdmissionRequest) { r.Subject.WorkID = "changed" }},
		{"graph", func(r *admission.AdmissionRequest) { r.Identity.GraphRevision++ }},
		{"generation", func(r *admission.AdmissionRequest) { r.Identity.Generation++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := original
			test.mutate(&changed)
			if _, err := service.Reserve(context.Background(), changed); !errors.Is(err, admission.ErrConflict) {
				t.Fatalf("Reserve(changed) error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestReplayRejectsChangedLeaseExpiryAndNonResultOutcome(t *testing.T) {
	store := newStore(t)
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	request := leaseRequest("run-a", "lease-a", "resource-a", time.Unix(200, 0), "lease-binding")
	if _, err := service.AcquireLease(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.ExpiresAt = changed.ExpiresAt.Add(time.Second)
	if _, err := service.AcquireLease(context.Background(), changed); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("AcquireLease(changed expiry) error = %v, want ErrConflict", err)
	}

	ambiguous := admissionRequest("run-b", "admission-b", "principal-b", "project-b", "work-b", "ambiguous")
	ambiguous.Identity.OutcomeKind = journal.EventActionAmbiguous
	if _, err := service.Reserve(context.Background(), ambiguous); !errors.Is(err, admission.ErrInvalidRecord) {
		t.Fatalf("Reserve(ambiguous outcome) error = %v, want ErrInvalidRecord", err)
	}
}

func TestTombstoneRejectsTerminalLeaseRequestWithChangedExpiry(t *testing.T) {
	store := newStore(t)
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	request := leaseRequest("run-a", "lease-a", "resource-a", time.Unix(200, 0), "lease-terminal")
	if _, err := service.AcquireLease(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Identity = nextIdentity(changed.Identity, "release")
	changed.ExpiresAt = changed.ExpiresAt.Add(time.Second)
	if _, err := service.ReleaseLease(context.Background(), changed); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("ReleaseLease(changed expiry) error = %v, want ErrConflict", err)
	}
}

func TestTombstoneLeaseDecisionUsesOneDeterministicClockSample(t *testing.T) {
	store := newStore(t)
	clock := &sequenceClock{values: []time.Time{
		time.Unix(99, 0).UTC(),
		time.Unix(100, 0).UTC(),
	}}
	service := newService(t, store, generousPolicy(), clock.Now)
	request := leaseRequest("run-a", "lease-a", "resource-a", time.Unix(100, 0).UTC(), "clock")
	receipt, err := service.AcquireLease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Lease.IssuedAt.Equal(time.Unix(99, 0).UTC()) {
		t.Fatalf("IssuedAt = %s, want the admission decision sample", receipt.Lease.IssuedAt)
	}
}

func TestJournalMalformedAdmissionSchemaFailsClosed(t *testing.T) {
	store := newStore(t)
	requestPayload := canonical(t, map[string]any{
		"component":          "admission",
		"schema_version":     1,
		"semantic_operation": "admission_reserve",
	})
	outcomePayload := canonical(t, map[string]any{
		"component":          "admission",
		"schema_version":     1,
		"semantic_operation": "admission_reserve",
		"unexpected":         "must fail strict decode",
	})
	action := journal.Action{
		ID: "malformed-action", ControlRunID: "run-a", Kind: journal.KindAllocateResource,
		GraphRevision: 1, ExpectedProjection: 0, CanonicalRequestDigest: digest(requestPayload),
		IdempotencyKey: "malformed-key",
	}
	_, err := store.Commit(context.Background(), journal.CommitRequest{
		Action: action, ExpectedRun: journal.NewRunCursor("installation-a", "run-a"),
		ExpectedGlobal: journal.NewGlobalCursor("installation-a"), RequestPayload: requestPayload,
		Outcome: journal.Event{
			ID: "malformed-outcome", ControlRunID: "run-a", ActionID: action.ID,
			Kind: journal.EventActionResult, PayloadDigest: digest(outcomePayload),
			OccurredAt: time.Unix(100, 0).UTC(),
		}, OutcomePayload: outcomePayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	if _, err := service.Admission(context.Background(), "run-a", "anything"); !errors.Is(err, admission.ErrInvalidRecord) {
		t.Fatalf("Admission() error = %v, want ErrInvalidRecord", err)
	}
}

func TestCASAssignsSequenceOnlyToWinningCommit(t *testing.T) {
	store := newStore(t)
	services := []*admission.Service{
		newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0))),
		newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0))),
	}
	requests := []admission.AdmissionRequest{
		admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "cas-a"),
		admissionRequest("run-b", "admission-b", "principal-b", "project-b", "work-b", "cas-b"),
	}
	start := make(chan struct{})
	receipts := make(chan admission.AdmissionReceipt, 2)
	for i := range services {
		go func() {
			<-start
			receipt, err := services[i].Reserve(context.Background(), requests[i])
			if err != nil {
				t.Errorf("Reserve() error = %v", err)
			}
			receipts <- receipt
		}()
	}
	close(start)
	positions := map[journal.JournalPosition]bool{}
	for range 2 {
		receipt := <-receipts
		positions[receipt.Commit.Reservation.JournalPosition] = true
		positions[receipt.Commit.Outcome.JournalPosition] = true
		if receipt.Admission.Sequence != uint64(receipt.Commit.Outcome.JournalPosition) {
			t.Fatalf("sequence = %d, outcome position = %d", receipt.Admission.Sequence, receipt.Commit.Outcome.JournalPosition)
		}
	}
	for position := journal.JournalPosition(1); position <= 4; position++ {
		if !positions[position] {
			t.Fatalf("missing assigned position %d in %#v", position, positions)
		}
	}
}

func TestNumericLosslessAndSaturatingBoundaries(t *testing.T) {
	if got := admission.SaturatingAdd(math.MaxUint64, 1); got != math.MaxUint64 {
		t.Fatalf("SaturatingAdd() = %d", got)
	}
	if got := admission.SaturatingSub(0, 1); got != 0 {
		t.Fatalf("SaturatingSub() = %d", got)
	}
	if got, err := admission.CeilQuantum(3); err != nil || got != 342 {
		t.Fatalf("CeilQuantum(3) = %d, %v, want 342", got, err)
	}
	if _, err := admission.CeilQuantum(0); !errors.Is(err, admission.ErrInvalidPolicy) {
		t.Fatalf("CeilQuantum(0) error = %v, want ErrInvalidPolicy", err)
	}
	if admission.MaxConsecutiveAdmissions != 2 {
		t.Fatalf("MaxConsecutiveAdmissions = %d", admission.MaxConsecutiveAdmissions)
	}

	store := newStore(t)
	policy := generousPolicy()
	policy.Version = math.MaxUint64
	service := newService(t, store, policy, fixedClock(time.Unix(100, 0)))
	request := admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "numeric")
	request.Identity.GraphRevision = math.MaxUint64
	request.Identity.Generation = 1<<53 + 1
	if _, err := service.Reserve(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := service.Admission(context.Background(), request.ControlRunID, request.Subject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.GraphRevision != math.MaxUint64 || rebuilt.Generation != 1<<53+1 ||
		rebuilt.PolicyVersion != math.MaxUint64 {
		t.Fatalf("lossy rebuild = %#v", rebuilt)
	}
}

func TestTombstoneExpiresAtBoundaryAndRetainsOriginalIdentity(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	clock := &testClock{now: now}
	store := newStore(t)
	service := newService(t, store, generousPolicy(), clock.Now)
	request := leaseRequest("run-a", "lease-a", "resource-a", now.Add(time.Second), "lease")
	acquired, err := service.AcquireLease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	clock.Set(request.ExpiresAt)
	expire := request
	expire.Identity = nextIdentity(request.Identity, "expire")
	expired, err := service.ExpireLease(context.Background(), expire)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Tombstone == nil || expired.Tombstone.State != admission.LeaseExpired ||
		expired.Tombstone.OriginalRequestDigest != acquired.Lease.OriginalRequestDigest ||
		expired.Tombstone.Subject != request.Subject {
		t.Fatalf("expired tombstone = %#v, acquired = %#v", expired.Tombstone, acquired.Lease)
	}
	restarted := newService(t, store, generousPolicy(), clock.Now)
	rebuiltLease, rebuiltTombstone, err := restarted.Lease(context.Background(), request.ControlRunID, request.Subject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebuiltLease != expired.Lease || rebuiltTombstone == nil || *rebuiltTombstone != *expired.Tombstone {
		t.Fatalf("rebuilt terminal lease = %#v %#v, want %#v %#v", rebuiltLease, rebuiltTombstone, expired.Lease, expired.Tombstone)
	}
	replayed, err := service.ExpireLease(context.Background(), expire)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Created || replayed.Commit.Outcome != expired.Commit.Outcome {
		t.Fatalf("expiry replay = %#v, want %#v", replayed, expired)
	}
	renew := request
	renew.Identity = nextIdentity(request.Identity, "renew")
	renew.Identity.Generation++
	renew.ExpiresAt = now.Add(time.Hour)
	if _, err := service.RenewLease(context.Background(), renew); !errors.Is(err, admission.ErrTerminal) {
		t.Fatalf("RenewLease(expired) error = %v, want ErrTerminal", err)
	}
	changed := expire
	changed.Subject.Mode = admission.LeaseShared
	if _, err := service.ExpireLease(context.Background(), changed); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("ExpireLease(changed replay) error = %v, want ErrConflict", err)
	}
}

func TestTombstoneRequiredBeforeAnExpiredResourceCanBeReacquired(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	clock := &testClock{now: now}
	store := newStore(t)
	service := newService(t, store, generousPolicy(), clock.Now)
	first := leaseRequest("run-a", "lease-a", "resource-a", now.Add(time.Second), "first")
	if _, err := service.AcquireLease(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	clock.Set(first.ExpiresAt)
	second := leaseRequest("run-b", "lease-b", "resource-a", now.Add(time.Hour), "second")
	if _, err := service.AcquireLease(context.Background(), second); !errors.Is(err, admission.ErrLeaseBusy) {
		t.Fatalf("AcquireLease(before expiry tombstone) error = %v, want ErrLeaseBusy", err)
	}
	expire := first
	expire.Identity = nextIdentity(first.Identity, "expire")
	if _, err := service.ExpireLease(context.Background(), expire); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcquireLease(context.Background(), second); err != nil {
		t.Fatalf("AcquireLease(after expiry tombstone) error = %v", err)
	}
}

func TestJournalQueueReleaseUsesQueueReleaseOperation(t *testing.T) {
	store := newStore(t)
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	request := admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "queue-release")
	if _, err := service.Reserve(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Identity = nextIdentity(request.Identity, "enqueue")
	if _, err := service.Enqueue(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Identity = nextIdentity(request.Identity, "release")
	request.Identity.GraphRevision++
	request.Identity.Generation++
	receipt, err := service.Release(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Operation != admission.OperationQueueRelease || receipt.Commit.Action.Kind != journal.KindDisposeResource {
		t.Fatalf("Release(queued) = %#v, want queue_release/dispose_resource", receipt)
	}
	rebuilt, err := service.Admission(context.Background(), request.ControlRunID, request.Subject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != receipt.Admission {
		t.Fatalf("rebuilt release = %#v, want receipt %#v", rebuilt, receipt.Admission)
	}
}

func TestLifetimeHistoryExceedsOneMiBWithoutOversizedDelta(t *testing.T) {
	store := newStore(t)
	policy := generousPolicy()
	policy.InstallationLimit = 4096
	service := newService(t, store, policy, fixedClock(time.Unix(100, 0)))
	for index := range 900 {
		request := admissionRequest(
			fmt.Sprintf("run-%04d", index), fmt.Sprintf("admission-%04d", index),
			fmt.Sprintf("principal-%04d", index), fmt.Sprintf("project-%04d", index),
			fmt.Sprintf("work-%04d-%080d", index, index), fmt.Sprintf("history-%04d", index),
		)
		if _, err := service.Reserve(context.Background(), request); err != nil {
			t.Fatalf("Reserve(%d) error = %v", index, err)
		}
	}
	events, cursor, err := readFeed(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var payloadBytes int
	for _, event := range events {
		if event.Kind != journal.EventActionResult {
			continue
		}
		payload, err := store.Payload(context.Background(), event.PayloadDigest)
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) > journal.MaxPayloadBytes {
			t.Fatalf("payload at %d has %d bytes", event.JournalPosition, len(payload))
		}
		payloadBytes += len(payload)
	}
	if payloadBytes <= journal.MaxPayloadBytes {
		t.Fatalf("lifetime payload bytes = %d, want > %d", payloadBytes, journal.MaxPayloadBytes)
	}
	if cursor.JournalPosition != journal.JournalPosition(len(events)) {
		t.Fatalf("cursor = %#v, events = %d", cursor, len(events))
	}
	restarted := newService(t, store, policy, fixedClock(time.Unix(100, 0)))
	if _, err := restarted.Admission(context.Background(), "run-0899", "admission-0899"); err != nil {
		t.Fatal(err)
	}
}

func TestResourceKeySpecificProgressHasNoJournalWideServiceMutex(t *testing.T) {
	base := newStore(t)
	blocking := &blockingStore{
		AuthoritativeStore: base,
		blockedRun:         "run-slow",
		started:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	service := newService(t, blocking, generousPolicy(), fixedClock(time.Unix(100, 0)))
	slow := leaseRequest("run-slow", "lease-slow", "resource-slow", time.Unix(200, 0), "slow")
	fast := leaseRequest("run-fast", "lease-fast", "resource-fast", time.Unix(200, 0), "fast")
	slowDone := make(chan error, 1)
	go func() {
		_, err := service.AcquireLease(context.Background(), slow)
		slowDone <- err
	}()
	<-blocking.started
	fastDone := make(chan error, 1)
	go func() {
		_, err := service.AcquireLease(context.Background(), fast)
		fastDone <- err
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated ResourceKey was blocked by slow journal I/O")
	}
	close(blocking.release)
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
}

func TestJournalCrossRunEqualExternalIDsRemainIsolated(t *testing.T) {
	store := newStore(t)
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	for _, runID := range []string{"run-a", "run-b"} {
		request := admissionRequest(runID, "same-admission", "principal", "project", "work", "same")
		if _, err := service.Reserve(context.Background(), request); err != nil {
			t.Fatalf("Reserve(%s) error = %v", runID, err)
		}
	}
	for _, runID := range []string{"run-a", "run-b"} {
		got, err := service.Admission(context.Background(), runID, "same-admission")
		if err != nil || got.ControlRunID != runID {
			t.Fatalf("Admission(%s) = %#v, %v", runID, got, err)
		}
	}
}

func TestJournalDiagnosticsAreTypedBoundedAndSecretSafe(t *testing.T) {
	store := newStore(t)
	service := newService(t, store, generousPolicy(), fixedClock(time.Unix(100, 0)))
	request := admissionRequest("run-a", "admission-a", "principal-a", "project-a", "work-a", "safe")
	request.Subject.WorkID = "secret-token-value-that-must-not-appear"
	request.Identity.ActionID = ""
	_, err := service.Reserve(context.Background(), request)
	var safe *admission.Error
	if !errors.As(err, &safe) || safe.Code != admission.CodeInvalidRequest {
		t.Fatalf("error = %#v, want typed invalid request", err)
	}
	if len(err.Error()) > 160 || contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe diagnostic = %q", err)
	}
}

func newStore(t *testing.T) *journal.MemoryStore {
	t.Helper()
	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newService(
	t *testing.T,
	store journal.AuthoritativeStore,
	policy admission.Policy,
	clock func() time.Time,
) *admission.Service {
	t.Helper()
	service, err := admission.New(admission.Dependencies{Store: store, Policy: policy, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func generousPolicy() admission.Policy {
	return admission.Policy{
		Version:           1,
		InstallationLimit: 4096,
		PrincipalLimit:    4096,
		RunLimit:          4096,
		ProjectLimit:      4096,
		PrimitiveLimit:    4096,
	}
}

func admissionRequest(runID, admissionID, principalID, projectID, workID, suffix string) admission.AdmissionRequest {
	return admission.AdmissionRequest{
		ControlRunID: runID,
		Identity:     identity("action-"+suffix, "key-"+suffix, "outcome-"+suffix),
		Subject: admission.AdmissionSubject{
			ID: admissionID, PrincipalID: principalID, ProjectID: projectID,
			Primitive: "persistent_session", WorkID: workID,
		},
	}
}

func leaseRequest(runID, leaseID, resourceName string, expiresAt time.Time, suffix string) admission.LeaseRequest {
	return admission.LeaseRequest{
		ControlRunID: runID,
		Identity:     identity("action-"+suffix, "key-"+suffix, "outcome-"+suffix),
		Subject: admission.LeaseSubject{
			ID:       leaseID,
			Resource: admission.ResourceKey{Namespace: "project", ProjectID: "project-a", Name: resourceName},
			Mode:     admission.LeaseExclusive, HolderID: "holder-a",
		},
		ExpiresAt: expiresAt,
	}
}

func identity(actionID, key, outcomeID string) admission.TransitionIdentity {
	return admission.TransitionIdentity{
		ActionID: actionID, IdempotencyKey: key, OutcomeEventID: outcomeID,
		OutcomeKind: journal.EventActionResult, GraphRevision: 1, Generation: 1,
		TaskID: "task-a", AttemptID: "attempt-a",
	}
}

func nextIdentity(prior admission.TransitionIdentity, suffix string) admission.TransitionIdentity {
	prior.ActionID += "-" + suffix
	prior.IdempotencyKey += "-" + suffix
	prior.OutcomeEventID += "-" + suffix
	return prior
}

func fixedClock(now time.Time) func() time.Time { return func() time.Time { return now.UTC() } }

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

type sequenceClock struct {
	mu     sync.Mutex
	values []time.Time
	index  int
}

func (c *sequenceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	index := min(c.index, len(c.values)-1)
	value := c.values[index]
	if c.index < len(c.values)-1 {
		c.index++
	}
	return value
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now.UTC()
}

type responseLossStore struct {
	journal.AuthoritativeStore
	once sync.Once
}

type responseLossEveryCommitStore struct {
	journal.AuthoritativeStore
	mu   sync.Mutex
	lost map[string]bool
}

func (s *responseLossEveryCommitStore) Commit(
	ctx context.Context,
	request journal.CommitRequest,
) (journal.CommitReceipt, error) {
	receipt, err := s.AuthoritativeStore.Commit(ctx, request)
	if err != nil {
		return receipt, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lost[request.Action.ID] {
		s.lost[request.Action.ID] = true
		return journal.CommitReceipt{}, errors.New("raw secret response loss")
	}
	return receipt, nil
}

func (s *responseLossStore) Commit(ctx context.Context, request journal.CommitRequest) (journal.CommitReceipt, error) {
	receipt, err := s.AuthoritativeStore.Commit(ctx, request)
	if err != nil {
		return receipt, err
	}
	lost := false
	s.once.Do(func() { lost = true })
	if lost {
		return journal.CommitReceipt{}, errors.New("raw secret provider response loss")
	}
	return receipt, nil
}

type blockingStore struct {
	journal.AuthoritativeStore
	blockedRun string
	started    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (s *blockingStore) Commit(ctx context.Context, request journal.CommitRequest) (journal.CommitReceipt, error) {
	if request.Action.ControlRunID == s.blockedRun {
		s.once.Do(func() { close(s.started) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return journal.CommitReceipt{}, ctx.Err()
		}
	}
	return s.AuthoritativeStore.Commit(ctx, request)
}

func canonical(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := journal.CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func digest(value []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(value)) }

func readFeed(ctx context.Context, store journal.AuthoritativeStore) ([]journal.Event, journal.GlobalCursor, error) {
	cursor := journal.GlobalCursor{}
	var result []journal.Event
	for {
		events, next, err := store.Feed(ctx, cursor, 1000)
		if err != nil {
			return nil, cursor, err
		}
		result = append(result, events...)
		cursor = next
		if len(events) == 0 {
			return result, cursor, nil
		}
	}
}

func contains(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
