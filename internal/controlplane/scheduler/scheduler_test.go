package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
	"github.com/araihu/paje/internal/controlplane/journal"
)

func TestRankReadyUsesSaturatingVirtualFinishAgingAndStableTieBreakers(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	items := []ReadyItem{
		readyItem("item-z", "run-b", 1024, 9, 2, now.Add(-59*time.Second)),
		readyItem("item-b", "run-a", 1023, math.MaxUint64, 1, now.Add(-60*time.Second)),
		readyItem("item-a", "run-a", 1024, 10, 1, now.Add(-301*60*time.Second)),
	}

	ranked, err := rankReady(items, now, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ranked[0].Item.ID, "item-a"; got != want {
		t.Fatalf("first item = %q, want %q", got, want)
	}
	if ranked[0].AgeCredit != 300 || ranked[0].EffectiveVirtualFinish != 0 {
		t.Fatalf("aged rank = %#v, want capped credit and no underflow", ranked[0])
	}
	if got, want := ranked[1].Item.ID, "item-z"; got != want {
		t.Fatalf("second item = %q, want %q", got, want)
	}
	if ranked[1].VirtualFinish != 10 || ranked[1].AgeCredit != 0 {
		t.Fatalf("unaged rank = %#v", ranked[1])
	}
	if got, want := ranked[2].Item.ID, "item-b"; got != want {
		t.Fatalf("last item = %q, want %q", got, want)
	}
	if ranked[2].VirtualFinish != math.MaxUint64 {
		t.Fatalf("virtual finish = %d, want saturation", ranked[2].VirtualFinish)
	}
	if ranked[2].AgeCredit != 1 {
		t.Fatalf("age credit at 60s = %d, want 1", ranked[2].AgeCredit)
	}

	preciseStart := uint64(1<<53 + 1)
	precise, err := rankReady([]ReadyItem{
		readyItem("precise", "run-precise", 1023, preciseStart, 1<<53+1, now),
	}, now, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := precise[0].VirtualFinish, preciseStart+2; got != want {
		t.Fatalf("lossless ceil virtual finish = %d, want %d", got, want)
	}

	ties := []ReadyItem{
		readyItem("item-b", "run-a", 1024, 5, 7, now),
		readyItem("item-a", "run-a", 1024, 5, 7, now),
		readyItem("item-a", "run-b", 1024, 5, 7, now),
	}
	ranked, err = rankReady(ties, now, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	got := []string{
		ranked[0].Item.ControlRunID + "/" + ranked[0].Item.ID,
		ranked[1].Item.ControlRunID + "/" + ranked[1].Item.ID,
		ranked[2].Item.ControlRunID + "/" + ranked[2].Item.ID,
	}
	want := []string{"run-a/item-a", "run-a/item-b", "run-b/item-a"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tie order = %v, want %v", got, want)
	}

	invalid := readyItem("zero", "run-zero", 0, 0, 1, now)
	if _, err := rankReady([]ReadyItem{invalid}, now, DefaultPolicy()); !errors.Is(err, admission.ErrInvalidPolicy) {
		t.Fatalf("zero weight error = %v, want invalid policy", err)
	}
}

func TestSelectReadyCapsConsecutiveAdmissionsAtExactlyTwo(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	items := []ReadyItem{
		readyItem("a-next", "run-a", 1024, 0, 1, now),
		readyItem("b-next", "run-b", 1, 0, 2, now),
	}

	selected, state, err := selectReady(items, FairnessState{
		LastRunID: "run-a", ConsecutiveAdmissions: 2,
	}, now, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if selected.Item.ControlRunID != "run-b" {
		t.Fatalf("selected run = %q, want forced alternate run-b", selected.Item.ControlRunID)
	}
	if state.LastRunID != "run-b" || state.ConsecutiveAdmissions != 1 {
		t.Fatalf("state = %#v, want run-b count 1", state)
	}

	selected, state, err = selectReady(items[:1], FairnessState{
		LastRunID: "run-a", ConsecutiveAdmissions: math.MaxUint64,
	}, now, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if selected.Item.ControlRunID != "run-a" || state.ConsecutiveAdmissions != math.MaxUint64 {
		t.Fatalf("single-run saturation = %#v %#v", selected, state)
	}
}

func TestSelectReadyPreventsStarvationAcrossOneHundredUnequalContinuousRuns(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	items := make([]ReadyItem, 100)
	for index := range items {
		weight := uint64(1)
		if index == 0 {
			weight = 1024
		}
		runID := fmt.Sprintf("run-%03d", index)
		items[index] = readyItem("item-"+runID, runID, weight, 0, uint64(index+1), now)
	}

	seen := make(map[string]bool, len(items))
	state := FairnessState{}
	for sequence := 0; sequence < 300 && len(seen) < len(items); sequence++ {
		selected, next, err := selectReady(items, state, now, DefaultPolicy())
		if err != nil {
			t.Fatal(err)
		}
		seen[selected.Item.ControlRunID] = true
		state = next
		for index := range items {
			if items[index].ControlRunID == selected.Item.ControlRunID {
				items[index].VirtualStart = selected.VirtualFinish
				items[index].EnqueueSequence = uint64(100 + sequence)
				break
			}
		}
	}
	if len(seen) != 100 {
		t.Fatalf("runs admitted = %d, want 100 without starvation", len(seen))
	}
}

func TestFairnessRebuildsOnlyFromAdmissionJournalAndPreservesBurstAcrossRestart(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store, authority := newAdmissionAuthority(t, "scheduler-fair-restart", now, admission.Policy{
		Version: 1, InstallationLimit: 100, PrincipalLimit: 100,
		RunLimit: 100, ProjectLimit: 100, PrimitiveLimit: 100,
	})
	service := newJournalScheduler(t, authority, store, func() time.Time { return now })

	for _, fixture := range []struct{ itemID, runID string }{
		{"a-1", "run-a"}, {"a-2", "run-a"}, {"b-1", "run-b"},
	} {
		queueAdmission(t, authority, service, fixture.itemID, fixture.runID)
	}
	published, err := service.Ready(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	published[0].Item.Weight = math.MaxUint64
	published[0].Item.VirtualStart = math.MaxUint64
	published[0].Item.EnqueueSequence = math.MaxUint64
	published[0].Item.EnqueuedAt = now.Add(-24 * time.Hour)
	published[0].Item.QueueReceipt.Admission.Subject.ID = "caller-rebound"

	first, err := service.AdmitNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.AdmitNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Item.Item.ID != "a-1" || second.Item.Item.ID != "a-2" {
		t.Fatalf("first admissions = %s/%s, want a-1/a-2", first.Item.Item.ID, second.Item.Item.ID)
	}

	restarted := newJournalScheduler(t, authority, store, func() time.Time { return now })
	third, err := restarted.AdmitNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.Item.Item.ID != "b-1" || third.State.LastRunID != "run-b" || third.State.ConsecutiveAdmissions != 1 {
		t.Fatalf("restart decision = %#v, want forced run-b", third)
	}

	tampered := &tamperingJournal{AuthoritativeStore: store}
	broken := newJournalScheduler(t, authority, tampered, func() time.Time { return now })
	if _, err := broken.Ready(context.Background()); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("tampered journal error = %v, want fail-closed invalid record", err)
	}
}

func TestAdmitNextSkipsQuotaBackpressureWithoutHeadOfLineBlocking(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store, authority := newAdmissionAuthority(t, "scheduler-quota", now, admission.Policy{
		Version: 1, InstallationLimit: 100, PrincipalLimit: 100,
		RunLimit: 1, ProjectLimit: 100, PrimitiveLimit: 100,
	})
	service := newJournalScheduler(t, authority, store, func() time.Time { return now })
	queueAdmission(t, authority, service, "item-a0", "run-a")
	if _, err := service.AdmitNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	queueAdmission(t, authority, service, "item-a1", "run-a")
	queueAdmission(t, authority, service, "item-b", "run-b")

	decision, err := service.AdmitNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Admitted || decision.Item.Item.ControlRunID != "run-b" {
		t.Fatalf("decision = %#v, want run-b admitted", decision)
	}
	if len(decision.Backpressure) != 1 || decision.Backpressure[0].ControlRunID != "run-a" {
		t.Fatalf("backpressure = %#v, want run-a receipt", decision.Backpressure)
	}
}

func TestResourceLocksSerializeOnlyTheExactResourceKey(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store, err := journal.NewMemoryStore("scheduler-locks")
	if err != nil {
		t.Fatal(err)
	}
	authority, err := admission.New(admission.Dependencies{
		Store: store,
		Policy: admission.Policy{
			Version: 1, InstallationLimit: 100, PrincipalLimit: 100,
			RunLimit: 100, ProjectLimit: 100, PrimitiveLimit: 100,
		},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	service := newScheduler(t, authority, nil, func() time.Time { return now })

	first := leaseRequest("run-a", "lease-a", "shared-target", now.Add(time.Hour), "a")
	if _, err := service.AcquireResource(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	contender := leaseRequest("run-b", "lease-b", "shared-target", now.Add(time.Hour), "b")
	if _, err := service.AcquireResource(context.Background(), contender); !errors.Is(err, admission.ErrLeaseBusy) {
		t.Fatalf("same-key error = %v, want lease busy", err)
	}
	unrelated := leaseRequest("run-c", "lease-c", "other-target", now.Add(time.Hour), "c")
	if _, err := service.AcquireResource(context.Background(), unrelated); err != nil {
		t.Fatalf("unrelated key blocked: %v", err)
	}
}

func TestSlowResourceAuthorityDoesNotBlockUnrelatedSchedulerCall(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	blocked := make(chan struct{})
	release := make(chan struct{})
	authority := &fakeAuthority{}
	authority.acquire = func(ctx context.Context, request admission.LeaseRequest) (admission.LeaseReceipt, error) {
		if request.Subject.Resource.Name == "blocked-target" {
			close(blocked)
			select {
			case <-release:
			case <-ctx.Done():
				return admission.LeaseReceipt{}, ctx.Err()
			}
		}
		return admission.LeaseReceipt{Lease: admission.Lease{Subject: request.Subject}}, nil
	}
	service := newScheduler(t, authority, nil, func() time.Time { return now })
	done := make(chan error, 1)
	go func() {
		_, err := service.AcquireResource(context.Background(), leaseRequest(
			"run-a", "lease-a", "blocked-target", now.Add(time.Hour), "blocked",
		))
		done <- err
	}()
	<-blocked

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := service.AcquireResource(ctx, leaseRequest(
		"run-b", "lease-b", "unrelated-target", now.Add(time.Hour), "unrelated",
	)); err != nil {
		t.Fatalf("unrelated acquire blocked: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func readyItem(
	itemID, runID string,
	weight, virtualStart, enqueueSequence uint64,
	enqueuedAt time.Time,
) ReadyItem {
	return ReadyItem{
		ID: itemID, ControlRunID: runID, Weight: weight,
		VirtualStart: virtualStart, EnqueueSequence: enqueueSequence, EnqueuedAt: enqueuedAt,
		Admission: admission.AdmissionRequest{
			ControlRunID: runID,
			Identity:     transitionIdentity("admit-"+itemID, 1),
			Subject: admission.AdmissionSubject{
				ID: itemID, PrincipalID: "principal", ProjectID: "project-" + runID,
				Primitive: "persistent_session", WorkID: itemID,
			},
		},
	}
}

func leaseRequest(runID, leaseID, resource string, expiresAt time.Time, suffix string) admission.LeaseRequest {
	return admission.LeaseRequest{
		ControlRunID: runID,
		Identity:     transitionIdentity("lease-"+suffix, 1),
		Subject: admission.LeaseSubject{
			ID: leaseID,
			Resource: admission.ResourceKey{
				Namespace: "project", ProjectID: "project-shared", Name: resource,
			},
			Mode: admission.LeaseExclusive, HolderID: "holder-" + runID,
		},
		ExpiresAt: expiresAt,
	}
}

func transitionIdentity(prefix string, generation uint64) admission.TransitionIdentity {
	return admission.TransitionIdentity{
		ActionID: prefix + "-action", IdempotencyKey: prefix + "-key",
		OutcomeEventID: prefix + "-outcome", OutcomeKind: journal.EventActionResult,
		GraphRevision: 1, Generation: generation, TaskID: "task", AttemptID: "attempt",
	}
}

func newScheduler(
	t *testing.T,
	authority Authority,
	store journal.AuthoritativeStore,
	clock func() time.Time,
) *Service {
	t.Helper()
	if store == nil {
		var err error
		store, err = journal.NewMemoryStore("scheduler-test")
		if err != nil {
			t.Fatal(err)
		}
	}
	service, err := New(Dependencies{
		Authority: authority, Journal: store, Clock: clock,
		Policy: DefaultPolicy(), ScannerAuthority: "scheduler-scanner",
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func newAdmissionAuthority(
	t *testing.T,
	installationID string,
	now time.Time,
	policy admission.Policy,
) (*journal.MemoryStore, *admission.Service) {
	t.Helper()
	store, err := journal.NewMemoryStore(installationID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := admission.New(admission.Dependencies{
		Store: store, Policy: policy, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, authority
}

func newJournalScheduler(
	t *testing.T,
	authority Authority,
	store journal.AuthoritativeStore,
	clock func() time.Time,
) *Service {
	t.Helper()
	service, err := New(Dependencies{
		Authority: authority, Journal: store, Clock: clock,
		Policy: DefaultPolicy(), ScannerAuthority: "scheduler-scanner",
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func queueAdmission(
	t *testing.T,
	authority *admission.Service,
	service *Service,
	itemID, runID string,
) {
	t.Helper()
	request := readyItem(itemID, runID, 1, 0, 1, time.Unix(1, 0).UTC()).Admission
	if runID == "run-a" {
		request.Subject.Primitive = "local_sequential"
	}
	request.Identity = transitionIdentity("reserve-"+itemID, 1)
	if _, err := authority.Reserve(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Identity = transitionIdentity("enqueue-"+itemID, 1)
	if _, err := service.Enqueue(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

type tamperingJournal struct {
	journal.AuthoritativeStore
}

func (s *tamperingJournal) Payload(ctx context.Context, digest string) ([]byte, error) {
	payload, err := s.AuthoritativeStore.Payload(ctx, digest)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(payload, []byte(`"semantic_operation":"queue_enqueue"`)) {
		return bytes.Replace(payload, []byte("queue_enqueue"), []byte("queue_enqueuX"), 1), nil
	}
	return payload, nil
}

type fakeAuthority struct {
	mu sync.Mutex

	calls []string

	admit            func(context.Context, admission.AdmissionRequest) (admission.AdmissionReceipt, error)
	enqueue          func(context.Context, admission.AdmissionRequest) (admission.AdmissionReceipt, error)
	acquire          func(context.Context, admission.LeaseRequest) (admission.LeaseReceipt, error)
	release          func(context.Context, admission.LeaseRequest) (admission.LeaseReceipt, error)
	expire           func(context.Context, admission.LeaseRequest) (admission.LeaseReceipt, error)
	startObservation func(context.Context, admission.RecoveryRequest) (admission.RecoveryReceipt, error)
	observe          func(context.Context, admission.RecoveryIdentity) (admission.ObservationReceipt, error)
	cancelOrFence    func(context.Context, admission.FenceRequest) (admission.RecoveryReceipt, error)
	scannerApply     func(context.Context, admission.ScannerApplyRequest) (admission.RecoveryReceipt, error)
}

func (f *fakeAuthority) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeAuthority) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAuthority) Admit(ctx context.Context, request admission.AdmissionRequest) (admission.AdmissionReceipt, error) {
	if f.admit != nil {
		return f.admit(ctx, request)
	}
	return admission.AdmissionReceipt{Admission: admission.RunAdmission{
		ControlRunID: request.ControlRunID, Subject: request.Subject, State: admission.AdmissionAdmitted,
	}}, nil
}

func (f *fakeAuthority) Enqueue(ctx context.Context, request admission.AdmissionRequest) (admission.AdmissionReceipt, error) {
	if f.enqueue != nil {
		return f.enqueue(ctx, request)
	}
	return admission.AdmissionReceipt{Admission: admission.RunAdmission{
		ControlRunID: request.ControlRunID, Subject: request.Subject, State: admission.AdmissionQueued,
	}}, nil
}

func (f *fakeAuthority) AcquireLease(ctx context.Context, request admission.LeaseRequest) (admission.LeaseReceipt, error) {
	if f.acquire != nil {
		return f.acquire(ctx, request)
	}
	return admission.LeaseReceipt{Lease: admission.Lease{ControlRunID: request.ControlRunID, Subject: request.Subject}}, nil
}

func (f *fakeAuthority) ReleaseLease(ctx context.Context, request admission.LeaseRequest) (admission.LeaseReceipt, error) {
	if f.release != nil {
		return f.release(ctx, request)
	}
	return admission.LeaseReceipt{}, nil
}

func (f *fakeAuthority) ExpireLease(ctx context.Context, request admission.LeaseRequest) (admission.LeaseReceipt, error) {
	if f.expire != nil {
		return f.expire(ctx, request)
	}
	return admission.LeaseReceipt{}, nil
}

func (f *fakeAuthority) StartObservation(ctx context.Context, request admission.RecoveryRequest) (admission.RecoveryReceipt, error) {
	if f.startObservation != nil {
		return f.startObservation(ctx, request)
	}
	bound := request.Recovery
	bound.InstallationID = "test-installation"
	return admission.RecoveryReceipt{Recovery: admission.RecoveryRecord{
		Identity: bound, State: admission.RecoveryObservationStarted,
	}}, nil
}

func (f *fakeAuthority) Observe(ctx context.Context, identity admission.RecoveryIdentity) (admission.ObservationReceipt, error) {
	if f.observe != nil {
		return f.observe(ctx, identity)
	}
	return admission.ObservationReceipt{}, nil
}

func (f *fakeAuthority) CancelOrFence(ctx context.Context, request admission.FenceRequest) (admission.RecoveryReceipt, error) {
	if f.cancelOrFence != nil {
		return f.cancelOrFence(ctx, request)
	}
	return admission.RecoveryReceipt{}, nil
}

func (f *fakeAuthority) ScannerApply(ctx context.Context, request admission.ScannerApplyRequest) (admission.RecoveryReceipt, error) {
	if f.scannerApply != nil {
		return f.scannerApply(ctx, request)
	}
	return admission.RecoveryReceipt{}, nil
}
