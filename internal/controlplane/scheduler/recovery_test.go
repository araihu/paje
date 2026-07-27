package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
	"github.com/araihu/paje/internal/controlplane/journal"
)

func TestRecoveryPageCapsAtOneHundred(t *testing.T) {
	projection := newRecoveryProjection(schedulerSnapshot{installationID: "scheduler-page"})
	for index := range 150 {
		entry := observationEntry(fmt.Sprintf("entry-%03d", index), fmt.Sprintf("run-%03d", index), time.Unix(1, 0))
		projection.entries[recoveryEntryKey(entry)] = registeredRecovery{Entry: entry, Sequence: uint64(index + 1)}
	}
	page := recoveryPage(projection, MaximumScanEntries)
	if len(page) != 100 || page[0].Entry.ID != "entry-000" || page[99].Entry.ID != "entry-099" {
		t.Fatalf("page = %d [%s..%s], want 100 [entry-000..entry-099]", len(page), page[0].Entry.ID, page[99].Entry.ID)
	}
}

func TestRecoveryFairnessSeesDueRunBeyondCurrentPage(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	entries := make(map[string]registeredRecovery)
	for index := range 100 {
		entry := observationEntry(fmt.Sprintf("entry-%03d", index), "run-a", now)
		entries[recoveryEntryKey(entry)] = registeredRecovery{Entry: entry, Sequence: uint64(index + 1)}
	}
	other := observationEntry("entry-100", "run-b", now)
	entries[recoveryEntryKey(other)] = registeredRecovery{Entry: other, Sequence: 101}
	runs := dueRecoveryRuns(entries, now)
	if len(runs) != 2 {
		t.Fatalf("due runs = %v, want both run-a and run-b despite 100-entry page", runs)
	}
}

func TestRecoveryJournalCheckpointPersistsCompletedPrefixAndRestartsAtThird(t *testing.T) {
	semanticNow := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	base, err := journal.NewMemoryStore("scheduler-prefix")
	if err != nil {
		t.Fatal(err)
	}
	observer := newBlockingThirdObserver()
	authority := newRecoveryAuthority(t, base, func() time.Time { return semanticNow }, observer)
	setup := newJournalScheduler(t, authority, base, func() time.Time { return semanticNow })
	for index := range 100 {
		entry := observationEntry(
			fmt.Sprintf("entry-%03d", index), fmt.Sprintf("run-%03d", index), semanticNow,
		)
		if _, err := setup.ScheduleRecovery(context.Background(), entry); err != nil {
			t.Fatalf("schedule recovery: %v (cause: %v)", err, errors.Unwrap(err))
		}
	}
	scanAuthority := &directObservationAuthority{Authority: authority, observer: observer}
	service := newJournalScheduler(t, scanAuthority, base, func() time.Time { return semanticNow })

	started := time.Now()
	first, err := service.ScanRecovery(context.Background())
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("scan elapsed = %s, want hard wall bound", elapsed)
	}
	if got := resultIDs(first.Results); fmt.Sprint(got) != fmt.Sprint([]string{"entry-000", "entry-001"}) {
		t.Fatalf("completed prefix = %v, want first two", got)
	}
	if first.NextCursor == "" || !first.Persisted {
		t.Fatalf("first checkpoint = %#v", first)
	}

	close(observer.releaseThird)
	restarted := newJournalScheduler(t, scanAuthority, base, func() time.Time { return semanticNow })
	second, err := restarted.ScanRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Results) == 0 || second.Results[0].EntryID != "entry-002" || second.PreviousCursor != first.NextCursor {
		t.Fatalf("restart result = %#v, want resume at entry-002 after cursor %q", second, first.NextCursor)
	}
	snapshot, err := restarted.rebuildJournal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, recovery := range snapshot.recoveries {
		if recovery.State != admission.RecoveryObservationStarted {
			t.Fatalf("late callback mutated recovery to %s", recovery.State)
		}
	}
}

func TestRecoveryScanRebuildsAuthoritativeJournalOnce(t *testing.T) {
	semanticNow := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	base, err := journal.NewMemoryStore("scheduler-single-replay")
	if err != nil {
		t.Fatal(err)
	}
	observer := observerFunc(func(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
		return admission.ProviderFact{
			Status: admission.ProviderAmbiguous, SubjectDigest: identity.SubjectDigest,
		}, nil
	})
	authority := newRecoveryAuthority(t, base, func() time.Time { return semanticNow }, observer)
	setup := newJournalScheduler(t, authority, base, func() time.Time { return semanticNow })
	if _, err := setup.ScheduleRecovery(
		ctx, observationEntry("single-replay", "run", semanticNow),
	); err != nil {
		t.Fatal(err)
	}

	singleReplay := &stallSecondReplayJournal{AuthoritativeStore: base}
	service := newJournalScheduler(t, authority, singleReplay, func() time.Time { return semanticNow })
	result, err := service.ScanRecovery(ctx)
	if err != nil {
		t.Fatalf("single-replay scan: %v", err)
	}
	if !result.Persisted || len(result.Results) != 1 || result.Results[0].EntryID != "single-replay" ||
		result.Results[0].Outcome != OutcomeAmbiguous {
		t.Fatalf("single-replay result = %#v", result)
	}
}

func TestRecoveryScanReplaysDeterministicSchedulerMembershipWithoutLookup(t *testing.T) {
	semanticNow := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	base, err := journal.NewMemoryStore("scheduler-event-membership")
	if err != nil {
		t.Fatal(err)
	}
	observer := observerFunc(func(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
		return admission.ProviderFact{
			Status: admission.ProviderAmbiguous, SubjectDigest: identity.SubjectDigest,
		}, nil
	})
	authority := newRecoveryAuthority(t, base, func() time.Time { return semanticNow }, observer)
	setup := newJournalScheduler(t, authority, base, func() time.Time { return semanticNow })
	if _, err := setup.ScheduleRecovery(
		ctx, observationEntry("event-membership", "run", semanticNow),
	); err != nil {
		t.Fatal(err)
	}

	journalWithBlockedLookup := &stallSchedulerReservationJournal{AuthoritativeStore: base}
	service := newJournalScheduler(t, authority, journalWithBlockedLookup, func() time.Time { return semanticNow })
	result, err := service.ScanRecovery(ctx)
	if err != nil {
		t.Fatalf("event-membership scan: %v", err)
	}
	if !result.Persisted || len(result.Results) != 1 || result.Results[0].EntryID != "event-membership" ||
		result.Results[0].Outcome != OutcomeAmbiguous {
		t.Fatalf("event-membership result = %#v", result)
	}
}

func TestRecoveryHardDeadlineContainsNonCooperativeJournalRead(t *testing.T) {
	semanticNow := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	base, authority := newAdmissionAuthority(t, "scheduler-blocked-feed", semanticNow, generousAdmissionPolicy())
	blocked := &blockingFeedJournal{AuthoritativeStore: base, entered: make(chan struct{}), release: make(chan struct{})}
	service := newJournalScheduler(t, authority, blocked, func() time.Time { return semanticNow })

	started := time.Now()
	_, err := service.ScanRecovery(context.Background())
	elapsed := time.Since(started)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("blocked feed error = %v, want budget", err)
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("blocked feed elapsed = %s, want <= 250ms plus scheduling slack", elapsed)
	}
	close(blocked.release)
}

func TestRecoveryHardDeadlineContainsNonCooperativeCheckpointStore(t *testing.T) {
	semanticNow := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	base, err := journal.NewMemoryStore("scheduler-blocked-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	observer := observerFunc(func(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
		return admission.ProviderFact{Status: admission.ProviderAmbiguous, SubjectDigest: identity.SubjectDigest}, nil
	})
	authority := newRecoveryAuthority(t, base, func() time.Time { return semanticNow }, observer)
	setup := newJournalScheduler(t, authority, base, func() time.Time { return semanticNow })
	if _, err := setup.ScheduleRecovery(context.Background(), observationEntry("blocked", "run", semanticNow)); err != nil {
		t.Fatal(err)
	}
	blocked := &blockingCheckpointJournal{
		AuthoritativeStore: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	service := newJournalScheduler(t, authority, blocked, func() time.Time { return semanticNow })

	started := time.Now()
	result, err := service.ScanRecovery(context.Background())
	elapsed := time.Since(started)
	if !errors.Is(err, ErrBudget) || result.Persisted {
		t.Fatalf("blocked checkpoint result/error = %#v / %v", result, err)
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("blocked checkpoint elapsed = %s, want hard wall bound", elapsed)
	}
	close(blocked.release)
	restarted := newJournalScheduler(t, authority, base, func() time.Time { return semanticNow })
	replayed, err := restarted.ScanRecovery(context.Background())
	if err != nil || len(replayed.Results) != 1 || replayed.Results[0].EntryID != "blocked" {
		t.Fatalf("restart after blocked persistence = %#v / %v", replayed, err)
	}
}

func TestRecoveryScanProcessesAtMostOneActionPerRunAndFailureDoesNotBlockOtherRun(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	observer := &recordingObserver{failRun: "run-a"}
	base, err := journal.NewMemoryStore("scheduler-run-fairness")
	if err != nil {
		t.Fatal(err)
	}
	authority := newRecoveryAuthority(t, base, func() time.Time { return now }, observer)
	service := newJournalScheduler(t, authority, base, func() time.Time { return now })
	for _, entry := range []RecoveryEntry{
		observationEntry("a-1", "run-a", now),
		observationEntry("a-2", "run-a", now),
		observationEntry("b-1", "run-b", now),
	} {
		if _, err := service.ScheduleRecovery(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}

	result, err := service.ScanRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := observer.Calls(); fmt.Sprint(got) != fmt.Sprint([]string{"run-a", "run-b"}) {
		t.Fatalf("observer calls = %v, want one per due run", got)
	}
	if len(result.Results) != 3 || result.Results[0].Backoff == nil ||
		result.Results[0].Backoff.Code != RetryAmbiguous || result.Results[1].Outcome != OutcomeDeferred ||
		result.Results[2].Outcome != OutcomeAmbiguous {
		t.Fatalf("results = %#v", result.Results)
	}
	if contains(fmt.Sprint(result.Results), "secret") {
		t.Fatalf("unsafe result = %#v", result.Results)
	}
}

func TestRecoveryFailingRunDoesNotBlockUnrelatedDueLease(t *testing.T) {
	boundary := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	semanticNow := boundary.Add(-time.Second)
	base, err := journal.NewMemoryStore("scheduler-failure-and-lease")
	if err != nil {
		t.Fatal(err)
	}
	observer := &recordingObserver{failRun: "run-a"}
	authority := newRecoveryAuthority(t, base, func() time.Time { return semanticNow }, observer)
	service := newJournalScheduler(t, authority, base, func() time.Time { return semanticNow })
	if _, err := service.ScheduleRecovery(
		context.Background(), observationEntry("failed-observation", "run-a", boundary),
	); err != nil {
		t.Fatal(err)
	}
	lease := leaseRequest("run-b", "lease-b", "unrelated", boundary, "lease-acquire")
	if _, err := authority.AcquireLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	expire := lease
	expire.Identity = transitionIdentity("lease-expire", 1)
	if _, err := service.ScheduleRecovery(context.Background(), RecoveryEntry{
		ID: "due-lease", ControlRunID: "run-b", DueAt: boundary,
		Kind: RecoveryLeaseExpiry, LeaseExpiry: &expire,
	}); err != nil {
		t.Fatal(err)
	}
	semanticNow = boundary

	result, err := service.ScanRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 || result.Results[0].Outcome != OutcomeAmbiguous ||
		result.Results[1].Outcome != OutcomeLeaseExpired {
		t.Fatalf("failure/lease results = %#v", result.Results)
	}
}

func TestRecoveryScanHandlesAmbiguityProofAndFencingWithoutOverlap(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		status          admission.ProviderStatus
		wantOutcome     RecoveryOutcome
		wantRescan      bool
		wantActionRetry bool
	}{
		{name: "ambiguous", status: admission.ProviderAmbiguous, wantOutcome: OutcomeAmbiguous, wantRescan: true},
		{name: "not performed", status: admission.ProviderNotPerformed, wantOutcome: OutcomeRetryAllowed, wantActionRetry: true},
		{name: "canceled", status: admission.ProviderCanceled, wantOutcome: OutcomeRetryAllowed, wantActionRetry: true},
		{name: "fenced", status: admission.ProviderFenced, wantOutcome: OutcomeFenced},
		{name: "effect observed", status: admission.ProviderEffectObserved, wantOutcome: OutcomeApplied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, err := journal.NewMemoryStore("scheduler-outcome-" + test.name)
			if err != nil {
				t.Fatal(err)
			}
			observer := observerFunc(func(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
				fact := admission.ProviderFact{Status: test.status, SubjectDigest: identity.SubjectDigest}
				if test.status != admission.ProviderAmbiguous {
					fact.ReceiptID = "provider-proof"
				}
				return fact, nil
			})
			authority := newRecoveryAuthority(t, base, func() time.Time { return now }, observer)
			service := newJournalScheduler(t, authority, base, func() time.Time { return now })
			if _, err := service.ScheduleRecovery(context.Background(), observationEntry("entry", "run", now)); err != nil {
				t.Fatal(err)
			}

			result, err := service.ScanRecovery(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Results) != 1 || result.Results[0].Outcome != test.wantOutcome {
				t.Fatalf("results = %#v, want %s", result.Results, test.wantOutcome)
			}
			gotRescan := result.Results[0].Backoff != nil && result.Results[0].Backoff.RescanAllowed
			gotActionRetry := result.Results[0].Backoff != nil && result.Results[0].Backoff.ActionRetryAllowed
			if gotRescan != test.wantRescan || gotActionRetry != test.wantActionRetry {
				t.Fatalf("rescan/action retry = %v/%v, want %v/%v", gotRescan, gotActionRetry, test.wantRescan, test.wantActionRetry)
			}
		})
	}
}

func TestRecoveryRejectsRevokedGenerationBeforeLateApply(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	base, err := journal.NewMemoryStore("scheduler-revoked")
	if err != nil {
		t.Fatal(err)
	}
	observer := observerFunc(func(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
		return admission.ProviderFact{
			Status: admission.ProviderEffectObserved, ReceiptID: "late", SubjectDigest: identity.SubjectDigest,
		}, nil
	})
	authority := newRecoveryAuthority(t, base, func() time.Time { return now }, observer)
	service := newJournalScheduler(t, authority, base, func() time.Time { return now })
	scheduled, err := service.ScheduleRecovery(context.Background(), observationEntry("entry", "run", now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.CancelOrFence(context.Background(), admission.FenceRequest{
		Identity: scheduled.Observation.FenceIdentity, Recovery: scheduled.Observation.Recovery,
		Proof: admission.FenceProof{Status: admission.ProviderFenced, ReceiptID: "revoked"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := service.ScanRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Outcome != OutcomeFenced {
		t.Fatalf("revoked result = %#v", result.Results)
	}
}

func TestRecoveryRebuildsAuthoritativeAppliedOutcomeWithoutAnotherCallback(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	base, err := journal.NewMemoryStore("scheduler-applied-rebuild")
	if err != nil {
		t.Fatal(err)
	}
	observer := &countingObserver{}
	authority := newRecoveryAuthority(t, base, func() time.Time { return now }, observer)
	service := newJournalScheduler(t, authority, base, func() time.Time { return now })
	scheduled, err := service.ScheduleRecovery(context.Background(), observationEntry("entry", "run", now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ScannerApply(context.Background(), admission.ScannerApplyRequest{
		Identity: scheduled.Observation.ApplyIdentity, Recovery: scheduled.Observation.Recovery,
		ScannerAuthority: "scheduler-scanner", Fact: admission.ProviderFact{
			Status: admission.ProviderEffectObserved, ReceiptID: "already-applied",
			SubjectDigest: scheduled.Observation.Recovery.SubjectDigest,
		},
	}); err != nil {
		t.Fatal(err)
	}

	restarted := newJournalScheduler(t, authority, base, func() time.Time { return now })
	result, err := restarted.ScanRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Outcome != OutcomeApplied || observer.Count() != 0 {
		t.Fatalf("rebuilt terminal result/callbacks = %#v / %d", result.Results, observer.Count())
	}
}

func TestRecoveryScanExpiresLeaseAtExactBoundary(t *testing.T) {
	expiresAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	semanticNow := expiresAt.Add(-time.Second)
	base, err := journal.NewMemoryStore("scheduler-expiry")
	if err != nil {
		t.Fatal(err)
	}
	authority := newRecoveryAuthority(t, base, func() time.Time { return semanticNow }, nil)
	service := newJournalScheduler(t, authority, base, func() time.Time { return semanticNow })
	request := leaseRequest("run", "lease", "resource", expiresAt, "acquire")
	if _, err := authority.AcquireLease(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	expire := request
	expire.Identity = transitionIdentity("expire", 1)
	entry := RecoveryEntry{
		ID: "expiry", ControlRunID: "run", DueAt: expiresAt,
		Kind: RecoveryLeaseExpiry, LeaseExpiry: &expire,
	}
	if _, err := service.ScheduleRecovery(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	semanticNow = expiresAt

	result, err := service.ScanRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Outcome != OutcomeLeaseExpired {
		t.Fatalf("expiry result = %#v", result.Results)
	}
}

func TestRecoveryResponseLossRebuildsCheckpointAndOutcomeMembership(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	base, err := journal.NewMemoryStore("scheduler-response-loss")
	if err != nil {
		t.Fatal(err)
	}
	observer := &countingObserver{}
	authority := newRecoveryAuthority(t, base, func() time.Time { return now }, observer)
	lossy := &responseLossStore{AuthoritativeStore: base, lost: make(map[string]bool)}
	service := newJournalScheduler(t, authority, lossy, func() time.Time { return now })
	if _, err := service.ScheduleRecovery(context.Background(), observationEntry("response-loss", "run", now)); err != nil {
		t.Fatal(err)
	}
	first, err := service.ScanRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Results) != 1 || first.Results[0].Outcome != OutcomeApplied || !first.Persisted {
		t.Fatalf("first results = %#v", first)
	}
	before := journalLength(t, base, "scheduler-response-loss")

	restarted := newJournalScheduler(t, authority, base, func() time.Time { return now })
	second, err := restarted.ScanRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Results) != 0 || second.PreviousCursor != first.NextCursor {
		t.Fatalf("rebuilt checkpoint = %#v", second)
	}
	if after := journalLength(t, base, "scheduler-response-loss"); after != before {
		t.Fatalf("journal events after rebuild = %d, want %d", after, before)
	}
	if observer.Count() != 1 {
		t.Fatalf("effect-free observations = %d, want one", observer.Count())
	}
}

func TestRecoveryRejectsStaleChangedCheckpointBeforeJournalCommit(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	base, err := journal.NewMemoryStore("scheduler-stale-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	authority := newRecoveryAuthority(t, base, func() time.Time { return now }, nil)
	service := newJournalScheduler(t, authority, base, func() time.Time { return now })
	if _, err := service.ScheduleRecovery(context.Background(), observationEntry("entry", "run", now)); err != nil {
		t.Fatal(err)
	}
	projection, err := service.rebuildRecoveryJournal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	page := recoveryPage(projection, MaximumScanEntries)
	fabricated := []RecoveryResult{{
		EntryID: "entry", ControlRunID: "run", Outcome: OutcomeApplied,
	}}
	if err := service.persistRecoveryCheckpoint(context.Background(), projection, fabricated, page); !errors.Is(err, ErrCursor) {
		t.Fatalf("fabricated terminal checkpoint error = %v, want cursor conflict", err)
	}
	firstBackoff := NextBackoff(now, RetryState{}, RetryAmbiguous, DefaultPolicy())
	first := []RecoveryResult{{
		EntryID: "entry", ControlRunID: "run", Outcome: OutcomeAmbiguous, Backoff: &firstBackoff,
	}}
	if err := service.persistRecoveryCheckpoint(context.Background(), projection, first, page); err != nil {
		t.Fatal(err)
	}
	changedBackoff := NextBackoff(now, RetryState{Step: 1, NextAttemptAt: firstBackoff.NextAttemptAt}, RetryAmbiguous, DefaultPolicy())
	changed := []RecoveryResult{{
		EntryID: "entry", ControlRunID: "run", Outcome: OutcomeAmbiguous, Backoff: &changedBackoff,
	}}
	if err := service.persistRecoveryCheckpoint(context.Background(), projection, changed, page); !errors.Is(err, ErrCursor) {
		t.Fatalf("stale changed checkpoint error = %v, want cursor conflict", err)
	}
	rebuilt, err := service.rebuildRecoveryJournal(context.Background())
	if err != nil {
		t.Fatalf("stale checkpoint corrupted journal: %v", err)
	}
	registered := rebuilt.entries[recoveryEntryKey(page[0].Entry)]
	if registered.Entry.Retry.Step != 1 {
		t.Fatalf("retry step = %d, want committed first checkpoint", registered.Entry.Retry.Step)
	}
}

func TestNextBackoffSaturatesStepAndDelay(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	backoff := NextBackoff(now, RetryState{Step: math.MaxUint64}, RetryTransient, DefaultPolicy())
	if backoff.Step != math.MaxUint64 || backoff.Delay != 5*time.Minute || !backoff.NextAttemptAt.Equal(now.Add(5*time.Minute)) ||
		!backoff.RescanAllowed || backoff.ActionRetryAllowed {
		t.Fatalf("backoff = %#v", backoff)
	}
}

func observationEntry(id, runID string, dueAt time.Time) RecoveryEntry {
	digest := fmt.Sprintf("sha256:%064x", len(id)+len(runID))
	recovery := admission.RecoveryIdentity{
		ControlRunID: runID, ActionID: id + "-action", Generation: 1, SubjectDigest: digest,
	}
	return RecoveryEntry{
		ID: id, ControlRunID: runID, DueAt: dueAt, Kind: RecoveryObservation,
		Observation: &ObservationWork{
			Start: admission.RecoveryRequest{
				Identity: transitionIdentity(id+"-start", 1), Recovery: recovery,
			},
			FenceIdentity: transitionIdentity(id+"-fence", 1),
			ApplyIdentity: transitionIdentity(id+"-apply", 1),
		},
	}
}

func generousAdmissionPolicy() admission.Policy {
	return admission.Policy{
		Version: 1, InstallationLimit: 1000, PrincipalLimit: 1000,
		RunLimit: 1000, ProjectLimit: 1000, PrimitiveLimit: 1000,
	}
}

func newRecoveryAuthority(
	t *testing.T,
	store journal.AuthoritativeStore,
	clock func() time.Time,
	observer admission.Observer,
) *admission.Service {
	t.Helper()
	authority, err := admission.New(admission.Dependencies{
		Store: store, Policy: generousAdmissionPolicy(), Clock: clock,
		Observer: observer, ScannerAuthority: "scheduler-scanner",
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

type observerFunc func(context.Context, admission.RecoveryIdentity) (admission.ProviderFact, error)

func (function observerFunc) Observe(ctx context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
	return function(ctx, identity)
}

type directObservationAuthority struct {
	Authority
	observer admission.Observer
}

func (a *directObservationAuthority) Observe(
	ctx context.Context,
	identity admission.RecoveryIdentity,
) (admission.ObservationReceipt, error) {
	fact, err := a.observer.Observe(ctx, identity)
	if err != nil {
		return admission.ObservationReceipt{}, err
	}
	return admission.ObservationReceipt{Recovery: identity, Fact: fact}, nil
}

func resultIDs(results []RecoveryResult) []string {
	ids := make([]string, len(results))
	for index := range results {
		ids[index] = results[index].EntryID
	}
	return ids
}

func contains(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}

type blockingThirdObserver struct {
	mu           sync.Mutex
	calls        int
	releaseThird chan struct{}
}

func newBlockingThirdObserver() *blockingThirdObserver {
	return &blockingThirdObserver{releaseThird: make(chan struct{})}
}

func (o *blockingThirdObserver) Observe(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
	o.mu.Lock()
	o.calls++
	call := o.calls
	o.mu.Unlock()
	if call == 3 {
		<-o.releaseThird
	}
	return admission.ProviderFact{
		Status: admission.ProviderAmbiguous, SubjectDigest: identity.SubjectDigest,
	}, nil
}

type recordingObserver struct {
	mu      sync.Mutex
	calls   []string
	failRun string
}

func (o *recordingObserver) Observe(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
	o.mu.Lock()
	o.calls = append(o.calls, identity.ControlRunID)
	o.mu.Unlock()
	if identity.ControlRunID == o.failRun {
		return admission.ProviderFact{}, errors.New("raw secret provider failure")
	}
	return admission.ProviderFact{Status: admission.ProviderAmbiguous, SubjectDigest: identity.SubjectDigest}, nil
}

func (o *recordingObserver) Calls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

type countingObserver struct {
	mu    sync.Mutex
	count int
}

func (o *countingObserver) Observe(_ context.Context, identity admission.RecoveryIdentity) (admission.ProviderFact, error) {
	o.mu.Lock()
	o.count++
	o.mu.Unlock()
	return admission.ProviderFact{
		Status: admission.ProviderEffectObserved, ReceiptID: "provider-observed",
		SubjectDigest: identity.SubjectDigest,
	}, nil
}

func (o *countingObserver) Count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.count
}

type blockingFeedJournal struct {
	journal.AuthoritativeStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

type stallSecondReplayJournal struct {
	journal.AuthoritativeStore
	mu      sync.Mutex
	replays int
}

type stallSchedulerReservationJournal struct {
	journal.AuthoritativeStore
}

func (s *stallSchedulerReservationJournal) Reservation(
	ctx context.Context,
	controlRunID string,
	actionID string,
) (journal.Action, error) {
	if strings.HasPrefix(actionID, schedulerRecoveryActionPrefix) {
		<-ctx.Done()
		return journal.Action{}, ctx.Err()
	}
	return s.AuthoritativeStore.Reservation(ctx, controlRunID, actionID)
}

func (s *stallSecondReplayJournal) Feed(
	ctx context.Context,
	cursor journal.GlobalCursor,
	limit int,
) ([]journal.Event, journal.GlobalCursor, error) {
	if cursor.JournalPosition == 0 {
		s.mu.Lock()
		s.replays++
		second := s.replays == 2
		s.mu.Unlock()
		if second {
			<-ctx.Done()
			return nil, cursor, ctx.Err()
		}
	}
	return s.AuthoritativeStore.Feed(ctx, cursor, limit)
}

func (s *blockingFeedJournal) Feed(context.Context, journal.GlobalCursor, int) ([]journal.Event, journal.GlobalCursor, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil, journal.GlobalCursor{}, errors.New("released blocked feed")
}

type blockingCheckpointJournal struct {
	journal.AuthoritativeStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *blockingCheckpointJournal) Commit(ctx context.Context, request journal.CommitRequest) (journal.CommitReceipt, error) {
	if request.Action.ControlRunID == schedulerRecoveryRunID {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.AuthoritativeStore.Commit(ctx, request)
}

type responseLossStore struct {
	journal.AuthoritativeStore
	mu   sync.Mutex
	lost map[string]bool
}

func (s *responseLossStore) Commit(ctx context.Context, request journal.CommitRequest) (journal.CommitReceipt, error) {
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

func journalLength(t *testing.T, store *journal.MemoryStore, installationID string) int {
	t.Helper()
	events, _, err := store.Feed(context.Background(), journal.GlobalCursor{
		InstallationID: installationID, SchemaVersion: journal.SchemaVersion,
	}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}
