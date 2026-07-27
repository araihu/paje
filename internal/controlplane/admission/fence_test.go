package admission_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
	"github.com/araihu/paje/internal/controlplane/journal"
)

func TestFenceObservationIsEffectFreeAndLateResultIsRejected(t *testing.T) {
	store := newStore(t)
	observer := &fakeObserver{facts: map[string]admission.ProviderFact{
		"target-action": {
			Status: admission.ProviderEffectObserved, ReceiptID: "provider-receipt",
			SubjectDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}}
	service, err := admission.New(admission.Dependencies{
		Store: store, Policy: generousPolicy(), Clock: fixedClock(time.Unix(100, 0)), Observer: observer,
		ScannerAuthority: "scanner-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery := admission.RecoveryIdentity{
		ControlRunID: "run-a", ActionID: "target-action", Generation: 2,
		SubjectDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	start := admission.RecoveryRequest{
		Identity: identity("observe-start", "observe-start-key", "observe-start-outcome"),
		Recovery: recovery,
	}
	if _, err := service.StartObservation(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	before, _, err := readFeed(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := service.Observe(context.Background(), recovery)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := readFeed(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || fact.Fact.Status != admission.ProviderEffectObserved {
		t.Fatalf("effect-free Observe changed feed or fact: before=%d after=%d fact=%#v", len(before), len(after), fact)
	}
	fence := admission.FenceRequest{
		Identity: identity("fence-action", "fence-key", "fence-outcome"), Recovery: recovery,
		Proof: admission.FenceProof{Status: admission.ProviderFenced, ReceiptID: "fence-receipt"},
	}
	if _, err := service.CancelOrFence(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	apply := admission.ScannerApplyRequest{
		Identity: identity("apply-action", "apply-key", "apply-outcome"), Recovery: recovery,
		ScannerAuthority: "scanner-a", Fact: fact.Fact,
	}
	if _, err := service.ScannerApply(context.Background(), apply); !errors.Is(err, admission.ErrFenced) {
		t.Fatalf("ScannerApply(late fact) error = %v, want ErrFenced", err)
	}
}

func TestFenceNotPerformedAllowsOneScannerOwnedRetryGeneration(t *testing.T) {
	store := newStore(t)
	observer := &fakeObserver{facts: map[string]admission.ProviderFact{
		"target-action": {
			Status: admission.ProviderNotPerformed, ReceiptID: "not-performed-receipt",
			SubjectDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		},
	}}
	service, err := admission.New(admission.Dependencies{
		Store: store, Policy: generousPolicy(), Clock: fixedClock(time.Unix(100, 0)), Observer: observer,
		ScannerAuthority: "scanner-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery := admission.RecoveryIdentity{
		ControlRunID: "run-a", ActionID: "target-action", Generation: 9,
		SubjectDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
	if _, err := service.StartObservation(context.Background(), admission.RecoveryRequest{
		Identity: identity("start-action", "start-key", "start-outcome"), Recovery: recovery,
	}); err != nil {
		t.Fatal(err)
	}
	fact, err := service.Observe(context.Background(), recovery)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := service.ScannerApply(context.Background(), admission.ScannerApplyRequest{
		Identity: identity("apply-action", "apply-key", "apply-outcome"), Recovery: recovery,
		ScannerAuthority: "scanner-a", Fact: fact.Fact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !apply.RetryAllowed || apply.Recovery.State != admission.RecoveryNotPerformed {
		t.Fatalf("apply = %#v, want retry allowed after not-performed", apply)
	}
	replayed, err := service.ScannerApply(context.Background(), admission.ScannerApplyRequest{
		Identity: identity("apply-action", "apply-key", "apply-outcome"), Recovery: recovery,
		ScannerAuthority: "scanner-a", Fact: fact.Fact,
	})
	if err != nil || replayed.Created || replayed.Commit.Outcome != apply.Commit.Outcome {
		t.Fatalf("scanner replay = %#v, %v, want immutable %#v", replayed, err, apply)
	}
	wrongScanner := admission.ScannerApplyRequest{
		Identity: identity("wrong-action", "wrong-key", "wrong-outcome"), Recovery: recovery,
		ScannerAuthority: "foreign-scanner", Fact: fact.Fact,
	}
	if _, err := service.ScannerApply(context.Background(), wrongScanner); !errors.Is(err, admission.ErrUnauthorized) {
		t.Fatalf("foreign scanner error = %v, want ErrUnauthorized", err)
	}
}

func TestFenceScannerApplyBindsFactAndRebuildsObservedEffect(t *testing.T) {
	store := newStore(t)
	service, err := admission.New(admission.Dependencies{
		Store: store, Policy: generousPolicy(), Clock: fixedClock(time.Unix(100, 0)),
		ScannerAuthority: "scanner-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery := admission.RecoveryIdentity{
		ControlRunID: "run-a", ActionID: "target-action", Generation: 3,
		SubjectDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}
	if _, err := service.StartObservation(context.Background(), admission.RecoveryRequest{
		Identity: identity("start-action", "start-key", "start-outcome"), Recovery: recovery,
	}); err != nil {
		t.Fatal(err)
	}
	apply := admission.ScannerApplyRequest{
		Identity: identity("apply-action", "apply-key", "apply-outcome"), Recovery: recovery,
		ScannerAuthority: "scanner-a", Fact: admission.ProviderFact{
			Status: admission.ProviderEffectObserved, ReceiptID: "provider-receipt",
			SubjectDigest: recovery.SubjectDigest,
		},
	}
	first, err := service.ScannerApply(context.Background(), apply)
	if err != nil {
		t.Fatal(err)
	}
	if first.Recovery.State != admission.RecoveryApplied {
		t.Fatalf("ScannerApply() state = %q, want applied", first.Recovery.State)
	}
	replayed, err := service.ScannerApply(context.Background(), apply)
	if err != nil || replayed.Created || replayed.Commit.Outcome != first.Commit.Outcome {
		t.Fatalf("ScannerApply(replay) = %#v, %v, want immutable %#v", replayed, err, first)
	}
	changed := apply
	changed.Fact.ReceiptID = "changed-provider-receipt"
	if _, err := service.ScannerApply(context.Background(), changed); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("ScannerApply(changed fact) error = %v, want ErrConflict", err)
	}
}

func TestFenceCancelReplayBindsProof(t *testing.T) {
	store := newStore(t)
	service, err := admission.New(admission.Dependencies{
		Store: store, Policy: generousPolicy(), Clock: fixedClock(time.Unix(100, 0)),
		ScannerAuthority: "scanner-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery := admission.RecoveryIdentity{
		ControlRunID: "run-a", ActionID: "target-action", Generation: 4,
		SubjectDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	}
	if _, err := service.StartObservation(context.Background(), admission.RecoveryRequest{
		Identity: identity("start-action", "start-key", "start-outcome"), Recovery: recovery,
	}); err != nil {
		t.Fatal(err)
	}
	cancel := admission.FenceRequest{
		Identity: identity("cancel-action", "cancel-key", "cancel-outcome"), Recovery: recovery,
		Proof: admission.FenceProof{Status: admission.ProviderCanceled, ReceiptID: "cancel-receipt"},
	}
	if _, err := service.CancelOrFence(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	changed := cancel
	changed.Proof.ReceiptID = "changed-cancel-receipt"
	if _, err := service.CancelOrFence(context.Background(), changed); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("CancelOrFence(changed proof) error = %v, want ErrConflict", err)
	}
}

func TestFenceRecoveryGenerationCannotOverlapAndRejectsLateFacts(t *testing.T) {
	store := newStore(t)
	service, err := admission.New(admission.Dependencies{
		Store: store, Policy: generousPolicy(), Clock: fixedClock(time.Unix(100, 0)),
		Observer: &fakeObserver{facts: map[string]admission.ProviderFact{}}, ScannerAuthority: "scanner-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := admission.RecoveryIdentity{
		ControlRunID: "run-a", ActionID: "target-action", Generation: 1,
		SubjectDigest: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	if _, err := service.StartObservation(context.Background(), admission.RecoveryRequest{
		Identity: identity("start-1", "start-key-1", "start-outcome-1"), Recovery: first,
	}); err != nil {
		t.Fatal(err)
	}
	before, _, err := readFeed(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous, err := service.Observe(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := readFeed(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous.Authoritative || ambiguous.Fact.Status != admission.ProviderAmbiguous || len(after) != len(before) {
		t.Fatalf("ambiguous Observe = %#v, feed before=%d after=%d", ambiguous, len(before), len(after))
	}
	second := first
	second.Generation++
	if _, err := service.StartObservation(context.Background(), admission.RecoveryRequest{
		Identity: identity("start-2-early", "start-key-2-early", "start-outcome-2-early"), Recovery: second,
	}); !errors.Is(err, admission.ErrConflict) {
		t.Fatalf("StartObservation(overlap) error = %v, want ErrConflict", err)
	}
	notPerformed := admission.ScannerApplyRequest{
		Identity: identity("apply-1", "apply-key-1", "apply-outcome-1"), Recovery: first,
		ScannerAuthority: "scanner-a", Fact: admission.ProviderFact{
			Status: admission.ProviderNotPerformed, ReceiptID: "not-performed-1",
			SubjectDigest: first.SubjectDigest,
		},
	}
	if _, err := service.ScannerApply(context.Background(), notPerformed); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartObservation(context.Background(), admission.RecoveryRequest{
		Identity: identity("start-2", "start-key-2", "start-outcome-2"), Recovery: second,
	}); err != nil {
		t.Fatalf("StartObservation(after not-performed) error = %v", err)
	}
	late := notPerformed
	late.Identity = identity("late-apply-1", "late-apply-key-1", "late-apply-outcome-1")
	late.Fact = admission.ProviderFact{
		Status: admission.ProviderEffectObserved, ReceiptID: "late-provider-receipt",
		SubjectDigest: first.SubjectDigest,
	}
	if _, err := service.ScannerApply(context.Background(), late); !errors.Is(err, admission.ErrFenced) {
		t.Fatalf("ScannerApply(late generation) error = %v, want ErrFenced", err)
	}
}

func TestFenceTerminalProofStopsSameGenerationAndPreservesRestart(t *testing.T) {
	tests := []struct {
		name  string
		proof admission.ProviderStatus
		state admission.RecoveryState
	}{
		{name: "canceled", proof: admission.ProviderCanceled, state: admission.RecoveryCanceled},
		{name: "not performed", proof: admission.ProviderNotPerformed, state: admission.RecoveryNotPerformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newStore(t)
			observer := &fakeObserver{facts: map[string]admission.ProviderFact{
				"target-action": {
					Status: admission.ProviderEffectObserved, ReceiptID: "contradictory-provider-receipt",
					SubjectDigest: "sha256:abababababababababababababababababababababababababababababababab",
				},
			}}
			service, err := admission.New(admission.Dependencies{
				Store: store, Policy: generousPolicy(), Clock: fixedClock(time.Unix(100, 0)),
				Observer: observer, ScannerAuthority: "scanner-a",
			})
			if err != nil {
				t.Fatal(err)
			}
			recovery := admission.RecoveryIdentity{
				ControlRunID: "run-a", ActionID: "target-action", Generation: 7,
				SubjectDigest: "sha256:abababababababababababababababababababababababababababababababab",
			}
			if _, err := service.StartObservation(context.Background(), admission.RecoveryRequest{
				Identity: identity("start-action", "start-key", "start-outcome"), Recovery: recovery,
			}); err != nil {
				t.Fatal(err)
			}
			cancel := admission.FenceRequest{
				Identity: identity("cancel-action", "cancel-key", "cancel-outcome"), Recovery: recovery,
				Proof: admission.FenceProof{Status: test.proof, ReceiptID: "terminal-proof-receipt"},
			}
			terminal, err := service.CancelOrFence(context.Background(), cancel)
			if err != nil {
				t.Fatal(err)
			}
			if terminal.Recovery.State != test.state {
				t.Fatalf("CancelOrFence() state = %q, want %q", terminal.Recovery.State, test.state)
			}
			before := snapshotAuthoritativeFeed(t, store)

			if _, err := service.Observe(context.Background(), recovery); !errors.Is(err, admission.ErrFenced) {
				t.Errorf("Observe(terminal generation) error = %v, want ErrFenced", err)
			}
			if calls := observer.calls.Load(); calls != 0 {
				t.Errorf("Observe(terminal generation) provider calls = %d, want 0", calls)
			}
			late := admission.ScannerApplyRequest{
				Identity: identity("late-action", "late-key", "late-outcome"), Recovery: recovery,
				ScannerAuthority: "scanner-a", Fact: observer.facts[recovery.ActionID],
			}
			if _, err := service.ScannerApply(context.Background(), late); !errors.Is(err, admission.ErrFenced) {
				t.Errorf("ScannerApply(contradictory late result) error = %v, want ErrFenced", err)
			}
			replayed, err := service.CancelOrFence(context.Background(), cancel)
			if err != nil || replayed.Created || !reflect.DeepEqual(replayed.Recovery, terminal.Recovery) ||
				replayed.Commit.Outcome != terminal.Commit.Outcome {
				t.Fatalf("CancelOrFence(replay) = %#v, %v, want immutable %#v", replayed, err, terminal)
			}
			if after := snapshotAuthoritativeFeed(t, store); !reflect.DeepEqual(after, before) {
				t.Errorf("rejected same-generation work changed authoritative feed:\nbefore=%#v\nafter=%#v", before, after)
			}

			restartObserver := &fakeObserver{facts: observer.facts}
			restarted, err := admission.New(admission.Dependencies{
				Store: store, Policy: generousPolicy(), Clock: fixedClock(time.Unix(100, 0)),
				Observer: restartObserver, ScannerAuthority: "scanner-a",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.Observe(context.Background(), recovery); !errors.Is(err, admission.ErrFenced) {
				t.Fatalf("Observe(restarted terminal generation) error = %v, want ErrFenced", err)
			}
			if calls := restartObserver.calls.Load(); calls != 0 {
				t.Fatalf("Observe(restarted terminal generation) provider calls = %d, want 0", calls)
			}
			late.Identity = identity("restart-late-action", "restart-late-key", "restart-late-outcome")
			if _, err := restarted.ScannerApply(context.Background(), late); !errors.Is(err, admission.ErrFenced) {
				t.Fatalf("ScannerApply(restarted contradictory result) error = %v, want ErrFenced", err)
			}
			rebuilt, err := restarted.CancelOrFence(context.Background(), cancel)
			if err != nil || rebuilt.Created || !reflect.DeepEqual(rebuilt.Recovery, terminal.Recovery) ||
				rebuilt.Commit.Outcome != terminal.Commit.Outcome {
				t.Fatalf("CancelOrFence(restarted replay) = %#v, %v, want immutable %#v", rebuilt, err, terminal)
			}
			if after := snapshotAuthoritativeFeed(t, store); !reflect.DeepEqual(after, before) {
				t.Fatalf("restart or rejected late result changed authoritative feed:\nbefore=%#v\nafter=%#v", before, after)
			}

			successor := recovery
			successor.Generation++
			if _, err := restarted.StartObservation(context.Background(), admission.RecoveryRequest{
				Identity: identity("successor-action", "successor-key", "successor-outcome"), Recovery: successor,
			}); err != nil {
				t.Fatalf("StartObservation(explicit successor) error = %v", err)
			}
		})
	}
}

type authoritativeFeedSnapshot struct {
	events          []journal.Event
	cursor          journal.GlobalCursor
	requestPayloads [][]byte
	outcomePayloads [][]byte
}

func snapshotAuthoritativeFeed(t *testing.T, store *journal.MemoryStore) authoritativeFeedSnapshot {
	t.Helper()
	ctx := context.Background()
	events, cursor, err := readFeed(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := authoritativeFeedSnapshot{events: events, cursor: cursor}
	for _, event := range events {
		if !journal.IsOutcome(event.Kind) {
			continue
		}
		action, err := store.Reservation(ctx, event.ControlRunID, event.ActionID)
		if err != nil {
			t.Fatal(err)
		}
		requestPayload, err := store.Payload(ctx, action.CanonicalRequestDigest)
		if err != nil {
			t.Fatal(err)
		}
		outcomePayload, err := store.Payload(ctx, event.PayloadDigest)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.requestPayloads = append(snapshot.requestPayloads, append([]byte(nil), requestPayload...))
		snapshot.outcomePayloads = append(snapshot.outcomePayloads, append([]byte(nil), outcomePayload...))
	}
	return snapshot
}

type fakeObserver struct {
	facts map[string]admission.ProviderFact
	calls atomic.Int64
}

func (o *fakeObserver) Observe(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
	o.calls.Add(1)
	fact, ok := o.facts[identity.ActionID]
	if !ok {
		return admission.ProviderFact{Status: admission.ProviderAmbiguous, SubjectDigest: identity.SubjectDigest}, nil
	}
	return fact, nil
}
