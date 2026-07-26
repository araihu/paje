package journal_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
	controlplanemock "github.com/araihu/paje/internal/controlplane/mock"
)

func TestJournalConcurrentExactReservationCreatesOneActionAndEvent(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	action := testAction("run-a", "action-a", "key-a", "request-a")
	type result struct {
		action  journal.Action
		created bool
		err     error
	}
	results := make(chan result, 32)
	var ready sync.WaitGroup
	ready.Add(32)
	start := make(chan struct{})
	for range 32 {
		go func() {
			ready.Done()
			<-start
			got, created, reserveErr := store.Reserve(context.Background(), action)
			results <- result{action: got, created: created, err: reserveErr}
		}()
	}
	ready.Wait()
	close(start)

	created := 0
	for range 32 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.action != action {
			t.Fatalf("Reserve() = %#v, want %#v", got.action, action)
		}
		if got.created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created reservations = %d, want 1", created)
	}
	feed, cursor, err := store.Feed(context.Background(), journal.NewGlobalCursor("installation-a"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 1 || feed[0].Kind != journal.EventActionReserved ||
		feed[0].ActionID != action.ID || cursor.JournalPosition != 1 {
		t.Fatalf("reservation feed = %#v cursor %#v", feed, cursor)
	}
}

func TestJournalReservationRejectsEveryStableKeyRebinding(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	original := testAction("run-a", "action-a", "key-a", "request-a")
	if _, _, err := store.Reserve(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*journal.Action)
	}{
		{"request", func(a *journal.Action) { a.CanonicalRequestDigest = digest("changed") }},
		{"graph", func(a *journal.Action) { a.GraphRevision++ }},
		{"projection", func(a *journal.Action) { a.ExpectedProjection++ }},
		{"task", func(a *journal.Action) { a.TaskID = "task-b" }},
		{"run", func(a *journal.Action) { a.ControlRunID = "run-b" }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := original
			test.mutate(&changed)
			if _, _, err := store.Reserve(context.Background(), changed); !errors.Is(err, journal.ErrConflict) {
				t.Fatalf("Reserve(rebound %s) error = %v, want ErrConflict", test.name, err)
			}
		})
	}
}

func TestJournalReservationBindsExactlyOneOutcomeAndRejectsCrossRunBinding(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	first := testAction("run-a", "action-a", "key-a", "request-a")
	second := testAction("run-b", "action-b", "key-b", "request-b")
	if _, _, err := store.Reserve(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Reserve(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	outcome := testEvent("run-a", "result-a", first.ID, journal.EventActionResult, "result-a")
	appended, err := store.Append(context.Background(), "run-a", 1, outcome)
	if err != nil {
		t.Fatal(err)
	}
	if appended.RunSequence != 2 || appended.JournalPosition != 3 {
		t.Fatalf("Append(result) positions = run %d global %d, want 2/3", appended.RunSequence, appended.JournalPosition)
	}
	competing := testEvent("run-a", "not-performed-a", first.ID, journal.EventActionNotPerformed, "not-performed-a")
	if _, err := store.Append(context.Background(), "run-a", 2, competing); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("Append(competing outcome) error = %v, want ErrConflict", err)
	}
	foreign := testEvent("run-b", "foreign-result", first.ID, journal.EventActionResult, "foreign")
	if _, err := store.Append(context.Background(), "run-b", 1, foreign); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("Append(cross-run result) error = %v, want ErrConflict", err)
	}
}

func TestJournalAmbiguityReconcilesAndNotPerformedMayBeSuperseded(t *testing.T) {
	t.Parallel()

	t.Run("ambiguity resolves exactly once", func(t *testing.T) {
		store, err := journal.NewMemoryStore("installation-a")
		if err != nil {
			t.Fatal(err)
		}
		action := testAction("run-a", "action-a", "key-a", "request-a")
		if _, _, err := store.Reserve(context.Background(), action); err != nil {
			t.Fatal(err)
		}
		ambiguous := testEvent("run-a", "ambiguous-a", action.ID, journal.EventActionAmbiguous, "ambiguous-a")
		if _, err := store.Append(context.Background(), "run-a", 1, ambiguous); err != nil {
			t.Fatal(err)
		}
		resolved := testEvent("run-a", "resolved-a", action.ID, journal.EventActionResult, "resolved-a")
		if _, err := store.Append(context.Background(), "run-a", 2, resolved); err != nil {
			t.Fatal(err)
		}
		competing := testEvent("run-a", "competing-a", action.ID, journal.EventActionNotPerformed, "competing-a")
		if _, err := store.Append(context.Background(), "run-a", 3, competing); !errors.Is(err, journal.ErrConflict) {
			t.Fatalf("Append(competing resolution) error = %v, want ErrConflict", err)
		}
	})

	t.Run("not performed permits one supersession", func(t *testing.T) {
		store, err := journal.NewMemoryStore("installation-a")
		if err != nil {
			t.Fatal(err)
		}
		action := testAction("run-a", "action-a", "key-a", "request-a")
		if _, _, err := store.Reserve(context.Background(), action); err != nil {
			t.Fatal(err)
		}
		notPerformed := testEvent("run-a", "not-performed-a", action.ID, journal.EventActionNotPerformed, "not-performed-a")
		if _, err := store.Append(context.Background(), "run-a", 1, notPerformed); err != nil {
			t.Fatal(err)
		}
		superseded := testEvent("run-a", "superseded-a", action.ID, journal.EventActionSuperseded, "superseded-a")
		if _, err := store.Append(context.Background(), "run-a", 2, superseded); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(
			context.Background(), "run-a", 3,
			testEvent("run-a", "duplicate-supersession", action.ID, journal.EventActionSuperseded, "duplicate"),
		); !errors.Is(err, journal.ErrConflict) {
			t.Fatalf("Append(duplicate supersession) error = %v, want ErrConflict", err)
		}
	})
}

func TestJournalActiveRunsUsesInstallationBoundStablePagination(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-c", "run-a", "run-b"} {
		if _, err := store.Append(
			context.Background(), runID, 0,
			testEvent(runID, "event-"+runID, "", journal.EventProjectionUpdated, runID),
		); err != nil {
			t.Fatal(err)
		}
	}
	first, cursor, err := store.ActiveRuns(context.Background(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first) != "[run-a run-b]" || cursor == "" {
		t.Fatalf("ActiveRuns(first) = %v cursor %q", first, cursor)
	}
	second, next, err := store.ActiveRuns(context.Background(), cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(second) != "[run-c]" || next != "" {
		t.Fatalf("ActiveRuns(second) = %v cursor %q", second, next)
	}
	if _, _, err := store.ActiveRuns(context.Background(), "foreign:run-b", 2); !errors.Is(err, journal.ErrCursor) {
		t.Fatalf("ActiveRuns(foreign cursor) error = %v, want ErrCursor", err)
	}
}

func TestActiveRunsResumesAfterBoundaryDeactivationInMemory(t *testing.T) {
	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-c", "run-a", "run-b"} {
		if _, err := store.Append(
			context.Background(), runID, 0,
			testEvent(runID, "event-"+runID, "", journal.EventProjectionUpdated, runID),
		); err != nil {
			t.Fatal(err)
		}
	}
	first, cursor, err := store.ActiveRuns(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first) != "[run-a]" || cursor != "installation-a:run-a" {
		t.Fatalf("ActiveRuns(first) = %v cursor %q", first, cursor)
	}
	store.SetRunActive("run-a", false)
	second, cursor, err := store.ActiveRuns(context.Background(), cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(second) != "[run-b]" || cursor != "installation-a:run-b" {
		t.Fatalf("ActiveRuns(after deactivation) = %v cursor %q", second, cursor)
	}
	third, cursor, err := store.ActiveRuns(context.Background(), cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(third) != "[run-c]" || cursor != "" {
		t.Fatalf("ActiveRuns(final page) = %v cursor %q", third, cursor)
	}
	if _, _, err := store.ActiveRuns(context.Background(), "installation-a:", 1); !errors.Is(err, journal.ErrCursor) {
		t.Fatalf("ActiveRuns(malformed cursor) error = %v, want ErrCursor", err)
	}
}

func TestUnknownRunCursorIsIndependentOfUnrelatedMemoryEvents(t *testing.T) {
	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	cursor := journal.NewRunCursor("installation-a", "run-unknown")
	assertUnknownRunPage := func(label string) {
		t.Helper()
		events, next, err := store.RunEvents(context.Background(), cursor, 10)
		if err != nil {
			t.Fatalf("RunEvents(%s) error = %v", label, err)
		}
		if len(events) != 0 || next != cursor {
			t.Fatalf("RunEvents(%s) = %#v cursor %#v, want empty and %#v", label, events, next, cursor)
		}
	}
	assertUnknownRunPage("before unrelated append")
	if _, err := store.Append(
		context.Background(), "run-related", 0,
		testEvent("run-related", "related-event", "", journal.EventProjectionUpdated, "related"),
	); err != nil {
		t.Fatal(err)
	}
	assertUnknownRunPage("after unrelated append")
	future := cursor
	future.RunSequence = 1
	if _, _, err := store.RunEvents(context.Background(), future, 10); !errors.Is(err, journal.ErrCursor) {
		t.Fatalf("RunEvents(unknown positive sequence) error = %v, want ErrCursor", err)
	}
}

func TestJournalInterleavedTwentyRunFeedIsGloballyContiguous(t *testing.T) {
	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	const runs = 20
	const perRun = 50
	var group sync.WaitGroup
	group.Add(runs)
	errs := make(chan error, runs)
	for runIndex := range runs {
		go func() {
			defer group.Done()
			runID := fmt.Sprintf("run-%02d", runIndex)
			for sequence := range perRun {
				event := testEvent(runID, fmt.Sprintf("%s-%03d", runID, sequence+1), "", journal.EventProjectionUpdated, fmt.Sprintf("%s-%03d", runID, sequence+1))
				if _, appendErr := store.Append(context.Background(), runID, uint64(sequence), event); appendErr != nil {
					errs <- appendErr
					return
				}
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	feed, cursor, err := store.Feed(context.Background(), journal.NewGlobalCursor("installation-a"), runs*perRun)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != runs*perRun || cursor.JournalPosition != runs*perRun {
		t.Fatalf("Feed() = %d events cursor %d, want %d/%d", len(feed), cursor.JournalPosition, runs*perRun, runs*perRun)
	}
	perRunSequence := make(map[string]uint64, runs)
	for index, event := range feed {
		if event.JournalPosition != journal.JournalPosition(index+1) {
			t.Fatalf("feed[%d] position = %d, want %d", index, event.JournalPosition, index+1)
		}
		perRunSequence[event.ControlRunID]++
		if event.RunSequence != perRunSequence[event.ControlRunID] {
			t.Fatalf("%s sequence = %d, want %d", event.ControlRunID, event.RunSequence, perRunSequence[event.ControlRunID])
		}
	}
}

func TestJournalRejectsForeignFutureAndRegressiveCheckpointCursors(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	event := testEvent("run-a", "event-a", "", journal.EventProjectionUpdated, "event-a")
	if _, err := store.Append(context.Background(), "run-a", 0, event); err != nil {
		t.Fatal(err)
	}

	badRun := []journal.RunCursor{
		{InstallationID: "foreign", ControlRunID: "run-a", SchemaVersion: journal.SchemaVersion},
		{InstallationID: "installation-a", ControlRunID: "run-a", SchemaVersion: journal.SchemaVersion + 1},
		{InstallationID: "installation-a", ControlRunID: "run-b", SchemaVersion: journal.SchemaVersion, RunSequence: 1},
		{InstallationID: "installation-a", ControlRunID: "run-a", SchemaVersion: journal.SchemaVersion, RunSequence: 2},
	}
	for _, cursor := range badRun {
		if _, _, err := store.RunEvents(context.Background(), cursor, 10); !errors.Is(err, journal.ErrCursor) {
			t.Fatalf("RunEvents(%#v) error = %v, want ErrCursor", cursor, err)
		}
	}
	badGlobal := []journal.GlobalCursor{
		{InstallationID: "foreign", SchemaVersion: journal.SchemaVersion},
		{InstallationID: "installation-a", SchemaVersion: journal.SchemaVersion + 1},
		{InstallationID: "installation-a", SchemaVersion: journal.SchemaVersion, JournalPosition: 2},
	}
	for _, cursor := range badGlobal {
		if _, _, err := store.Feed(context.Background(), cursor, 10); !errors.Is(err, journal.ErrCursor) {
			t.Fatalf("Feed(%#v) error = %v, want ErrCursor", cursor, err)
		}
	}

	runCursor := journal.RunCursor{InstallationID: "installation-a", ControlRunID: "run-a", SchemaVersion: journal.SchemaVersion, RunSequence: 1}
	globalCursor := journal.GlobalCursor{InstallationID: "installation-a", SchemaVersion: journal.SchemaVersion, JournalPosition: 1}
	if err := store.Checkpoint(context.Background(), runCursor, globalCursor, []byte("one")); err != nil {
		t.Fatal(err)
	}
	runCursor.RunSequence = 0
	if err := store.Checkpoint(context.Background(), runCursor, globalCursor, []byte("regressed")); !errors.Is(err, journal.ErrCursor) {
		t.Fatalf("Checkpoint(regression) error = %v, want ErrCursor", err)
	}
}

func TestJournalZeroReadCursorBootstrapsImmutableInstallationBinding(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	event := testEvent("run-a", "event-a", "", journal.EventProjectionUpdated, "event-a")
	if _, err := store.Append(context.Background(), "run-a", 0, event); err != nil {
		t.Fatal(err)
	}
	feed, global, err := store.Feed(context.Background(), journal.GlobalCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 1 || global.InstallationID != "installation-a" ||
		global.SchemaVersion != journal.SchemaVersion || global.JournalPosition != 1 {
		t.Fatalf("Feed(zero cursor) = %#v cursor %#v", feed, global)
	}
	runEvents, run, err := store.RunEvents(
		context.Background(), journal.RunCursor{ControlRunID: "run-a"}, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runEvents) != 1 || run.InstallationID != "installation-a" ||
		run.SchemaVersion != journal.SchemaVersion || run.RunSequence != 1 {
		t.Fatalf("RunEvents(zero cursor) = %#v cursor %#v", runEvents, run)
	}
}

func TestAuthoritativeCommitAtomicallyBindsPayloadsAndReplaysWithStaleCursors(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	request := authoritativeCommitRequest(t, "installation-a", "run-a", "action-a", "key-a")
	first, err := store.Commit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Action != request.Action || first.Outcome.RunSequence != 2 ||
		first.Outcome.JournalPosition != 2 || first.Reservation.RunSequence != 1 ||
		first.Reservation.JournalPosition != 1 {
		t.Fatalf("Commit() receipt = %#v", first)
	}
	assertPayloadBytes(t, store, request.Action.CanonicalRequestDigest, request.RequestPayload)
	assertPayloadBytes(t, store, request.Outcome.PayloadDigest, request.OutcomePayload)

	replayed, err := store.Commit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Created || replayed.Action != first.Action ||
		replayed.Reservation != first.Reservation || replayed.Outcome != first.Outcome {
		t.Fatalf("Commit(replay) = %#v, want immutable %#v with Created=false", replayed, first)
	}
	feed, cursor, err := store.Feed(context.Background(), journal.NewGlobalCursor("installation-a"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 || feed[0] != first.Reservation || feed[1] != first.Outcome ||
		cursor.JournalPosition != 2 {
		t.Fatalf("Feed() = %#v cursor %#v", feed, cursor)
	}
}

func TestAuthoritativeCommitRejectsChangedReplayAndStaleGlobalCASWithoutMutation(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	original := authoritativeCommitRequest(t, "installation-a", "run-a", "action-a", "key-a")
	if _, err := store.Commit(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*journal.CommitRequest)
	}{
		{
			name: "request payload",
			mutate: func(request *journal.CommitRequest) {
				request.RequestPayload = canonicalPayload(t, map[string]any{"request": "changed"})
			},
		},
		{
			name: "outcome",
			mutate: func(request *journal.CommitRequest) {
				request.Outcome.ProviderReceipt = "changed-receipt"
			},
		},
		{
			name: "outcome payload",
			mutate: func(request *journal.CommitRequest) {
				request.OutcomePayload = canonicalPayload(t, map[string]any{"decision": "defer"})
			},
		},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := original
			changed.RequestPayload = append([]byte(nil), original.RequestPayload...)
			changed.OutcomePayload = append([]byte(nil), original.OutcomePayload...)
			test.mutate(&changed)
			if _, err := store.Commit(context.Background(), changed); !errors.Is(err, journal.ErrConflict) {
				t.Fatalf("Commit(changed replay) error = %v, want ErrConflict", err)
			}
		})
	}

	stale := authoritativeCommitRequest(t, "installation-a", "run-b", "action-b", "key-b")
	if _, err := store.Commit(context.Background(), stale); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("Commit(stale global cursor) error = %v, want ErrConflict", err)
	}
	if _, err := store.Payload(context.Background(), stale.Action.CanonicalRequestDigest); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("Payload(rejected request) error = %v, want ErrNotFound", err)
	}
	feed, cursor, err := store.Feed(context.Background(), journal.NewGlobalCursor("installation-a"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 || cursor.JournalPosition != 2 {
		t.Fatalf("rejected commits changed feed: %#v cursor %#v", feed, cursor)
	}
}

func TestAuthoritativeCommitGlobalCursorAllowsExactlyOneConcurrentRun(t *testing.T) {
	t.Parallel()

	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	requests := []journal.CommitRequest{
		authoritativeCommitRequest(t, "installation-a", "run-a", "action-a", "key-a"),
		authoritativeCommitRequest(t, "installation-a", "run-b", "action-b", "key-b"),
	}
	type result struct {
		index   int
		receipt journal.CommitReceipt
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, len(requests))
	for index := range requests {
		go func() {
			<-start
			receipt, commitErr := store.Commit(context.Background(), requests[index])
			results <- result{index: index, receipt: receipt, err: commitErr}
		}()
	}
	close(start)
	winner := -1
	for range requests {
		got := <-results
		switch {
		case got.err == nil:
			if winner != -1 || !got.receipt.Created {
				t.Fatalf("unexpected successful receipt %#v after winner %d", got.receipt, winner)
			}
			winner = got.index
		case !errors.Is(got.err, journal.ErrConflict):
			t.Fatalf("Commit(concurrent) error = %v, want ErrConflict", got.err)
		}
	}
	if winner == -1 {
		t.Fatal("no concurrent commit won the global cursor CAS")
	}
	loser := 1 - winner
	if _, err := store.Payload(context.Background(), requests[loser].Action.CanonicalRequestDigest); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("Payload(loser) error = %v, want ErrNotFound", err)
	}
	feed, cursor, err := store.Feed(context.Background(), journal.NewGlobalCursor("installation-a"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 || cursor.JournalPosition != 2 {
		t.Fatalf("Feed() = %#v cursor %#v, want one atomic two-event commit", feed, cursor)
	}
}

func TestAuthoritativeCommitRejectsUnsafePayloadsBeforeMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*journal.CommitRequest)
	}{
		{
			name: "malformed request",
			mutate: func(request *journal.CommitRequest) {
				request.RequestPayload = []byte(`{"request":`)
				request.Action.CanonicalRequestDigest = digestBytes(request.RequestPayload)
			},
		},
		{
			name: "noncanonical request",
			mutate: func(request *journal.CommitRequest) {
				request.RequestPayload = []byte("{\"request\":\"admit\"}")
				request.Action.CanonicalRequestDigest = digestBytes(request.RequestPayload)
			},
		},
		{
			name: "mismatched request digest",
			mutate: func(request *journal.CommitRequest) {
				request.Action.CanonicalRequestDigest = digest("different")
			},
		},
		{
			name: "noncanonical outcome",
			mutate: func(request *journal.CommitRequest) {
				request.OutcomePayload = []byte(" {\"decision\":\"admit\"}\n")
				request.Outcome.PayloadDigest = digestBytes(request.OutcomePayload)
			},
		},
		{
			name: "oversized outcome",
			mutate: func(request *journal.CommitRequest) {
				request.OutcomePayload = []byte("\"" + strings.Repeat("x", journal.MaxPayloadBytes) + "\"\n")
				request.Outcome.PayloadDigest = digestBytes(request.OutcomePayload)
			},
		},
		{
			name: "oversized cursor identity",
			mutate: func(request *journal.CommitRequest) {
				identity := strings.Repeat("i", 129)
				request.ExpectedRun.InstallationID = identity
				request.ExpectedGlobal.InstallationID = identity
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := journal.NewMemoryStore("installation-a")
			if err != nil {
				t.Fatal(err)
			}
			request := authoritativeCommitRequest(t, "installation-a", "run-a", "action-a", "key-a")
			test.mutate(&request)
			if _, err := store.Commit(context.Background(), request); !errors.Is(err, journal.ErrInvalidRecord) {
				t.Fatalf("Commit(unsafe payload) error = %v, want ErrInvalidRecord", err)
			}
			feed, cursor, err := store.Feed(context.Background(), journal.NewGlobalCursor("installation-a"), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(feed) != 0 || cursor.JournalPosition != 0 {
				t.Fatalf("unsafe payload changed feed: %#v cursor %#v", feed, cursor)
			}
		})
	}
}

func TestAuthoritativeMockStoreCommitsAndReturnsImmutablePayload(t *testing.T) {
	t.Parallel()

	store := controlplanemock.NewStore()
	request := authoritativeCommitRequest(t, store.InstallationID(), "run-a", "action-a", "key-a")
	receipt, err := store.Commit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Created || receipt.Action != request.Action {
		t.Fatalf("Commit() = %#v", receipt)
	}
	assertPayloadBytes(t, store, request.Action.CanonicalRequestDigest, request.RequestPayload)
}

func authoritativeCommitRequest(
	t *testing.T,
	installationID, runID, actionID, key string,
) journal.CommitRequest {
	t.Helper()
	requestPayload := canonicalPayload(t, map[string]any{
		"action_id":      actionID,
		"control_run_id": runID,
		"principal":      "principal-a",
		"project":        "project-a",
		"request":        "admit",
	})
	outcomePayload := canonicalPayload(t, map[string]any{
		"decision": "admit",
		"weight":   1,
	})
	action := testAction(runID, actionID, key, "placeholder")
	action.CanonicalRequestDigest = digestBytes(requestPayload)
	return journal.CommitRequest{
		Action:         action,
		ExpectedRun:    journal.NewRunCursor(installationID, runID),
		ExpectedGlobal: journal.NewGlobalCursor(installationID),
		RequestPayload: requestPayload,
		Outcome: journal.Event{
			ID: "outcome-" + actionID, ControlRunID: runID, ActionID: actionID,
			Kind: journal.EventActionResult, PayloadDigest: digestBytes(outcomePayload),
			OccurredAt: time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC),
		},
		OutcomePayload: outcomePayload,
	}
}

func canonicalPayload(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := journal.CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func digestBytes(value []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(value))
}

func assertPayloadBytes(t *testing.T, store journal.AuthoritativeStore, digest string, want []byte) {
	t.Helper()
	got, err := store.Payload(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Payload(%q) = %q, want %q", digest, got, want)
	}
	if len(got) != 0 {
		got[0] ^= 0xff
	}
	again, err := store.Payload(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, want) {
		t.Fatalf("Payload(%q) was mutable: %q, want %q", digest, again, want)
	}
}

func testAction(runID, actionID, key, request string) journal.Action {
	return journal.Action{
		ID: actionID, ControlRunID: runID, TaskID: "task-a", AttemptID: "attempt-a",
		Kind: journal.KindDispatch, GraphRevision: 1, ExpectedProjection: 1,
		CanonicalRequestDigest: digest(request), IdempotencyKey: key,
	}
}

func testEvent(runID, eventID, actionID string, kind journal.EventKind, payload string) journal.Event {
	return journal.Event{
		ID: eventID, ControlRunID: runID, ActionID: actionID, Kind: kind,
		PayloadDigest: digest(payload), OccurredAt: time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC),
	}
}

func digest(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}
