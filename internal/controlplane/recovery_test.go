package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/agentharness"
	agentmock "github.com/araihu/paje/internal/agentharness/mock"
	"github.com/araihu/paje/internal/controlplane"
	controlmock "github.com/araihu/paje/internal/controlplane/mock"
)

func TestRestartDoesNotRepeatKnownActionsAndNeverRegressesCursor(t *testing.T) {
	t.Parallel()

	store := controlmock.NewStore()
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskReady
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	attempt, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	action, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("dispatch"))
	if err != nil {
		t.Fatal(err)
	}

	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	reused, err := restarted.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID != action.ID {
		t.Fatalf("PrepareAction() after restart = %q, want %q", reused.ID, action.ID)
	}
	if _, err := restarted.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("changed")); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("PrepareAction(changed digest) error = %v, want ErrActionConflict", err)
	}
	if _, err := restarted.CompleteAction(context.Background(), run.ID, action.ID, agentharness.ActionResult{
		ActionID: action.ID, RuntimeWorkIDs: []string{"runtime-child-1"}, Cursor: "cursor-2", CursorSequence: 2,
		ResultDigest: digest("result"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AdvanceCursor(context.Background(), run.ID, attempt.ID, "cursor-3", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AdvanceCursor(context.Background(), run.ID, attempt.ID, "cursor-2", 2); !errors.Is(err, controlplane.ErrCursorRegression) {
		t.Fatalf("AdvanceCursor(regression) error = %v, want ErrCursorRegression", err)
	}
}

func TestCompleteActionRejectsUntrustedResultEventAndCursorData(t *testing.T) {
	t.Run("result digest", func(t *testing.T) {
		_, service, runID, attempt := persistentService(t)
		action, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-invalid-result"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CompleteAction(context.Background(), runID, action.ID, agentharness.ActionResult{
			ActionID: action.ID, RuntimeWorkIDs: []string{"runtime-invalid-result"}, ResultDigest: "not-a-digest",
		}); !errors.Is(err, controlplane.ErrActionConflict) {
			t.Fatalf("CompleteAction(invalid result digest) error = %v, want ErrActionConflict", err)
		}
	})

	t.Run("terminal event identity and digest", func(t *testing.T) {
		_, service, runID, attempt := persistentService(t)
		dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-invalid-event"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
			ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-invalid-event"}, ResultDigest: digest("dispatch-result"),
		}); err != nil {
			t.Fatal(err)
		}
		wait, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionWait, digest("wait-invalid-event"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CompleteAction(context.Background(), runID, wait.ID, agentharness.ActionResult{
			ActionID: wait.ID, RuntimeWorkIDs: []string{"runtime-invalid-event"}, ResultDigest: digest("wait-result"),
			Events: []agentharness.WorkEvent{{Kind: "completed", Terminal: true, ResultDigest: "invalid"}},
		}); !errors.Is(err, controlplane.ErrActionConflict) {
			t.Fatalf("CompleteAction(invalid terminal event) error = %v, want ErrActionConflict", err)
		}
	})

	t.Run("persistent terminal event without monotonic cursor", func(t *testing.T) {
		_, service, runID, attempt := persistentService(t)
		dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-missing-event-cursor"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
			ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-missing-event-cursor"}, ResultDigest: digest("dispatch-result"),
		}); err != nil {
			t.Fatal(err)
		}
		wait, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionWait, digest("wait-missing-event-cursor"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CompleteAction(context.Background(), runID, wait.ID, agentharness.ActionResult{
			ActionID: wait.ID, RuntimeWorkIDs: []string{"runtime-missing-event-cursor"}, ResultDigest: digest("wait-result"),
			Events: []agentharness.WorkEvent{{
				ID: "terminal-no-cursor", RuntimeWorkID: "runtime-missing-event-cursor",
				Kind: "completed", ResultDigest: digest("terminal-no-cursor"), Terminal: true,
			}},
		}); !errors.Is(err, controlplane.ErrActionConflict) {
			t.Fatalf("CompleteAction(persistent event without cursor) error = %v, want ErrActionConflict", err)
		}
	})

	t.Run("repeated cursor", func(t *testing.T) {
		_, service, runID, attempt := persistentService(t)
		dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-cursor"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
			ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-cursor"}, Cursor: "cursor-1", CursorSequence: 1, ResultDigest: digest("dispatch-result"),
		}); err != nil {
			t.Fatal(err)
		}
		observe, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionObserve, digest("observe-cursor"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CompleteAction(context.Background(), runID, observe.ID, agentharness.ActionResult{
			ActionID: observe.ID, Cursor: "cursor-1", CursorSequence: 1, ResultDigest: digest("observe-result"),
		}); !errors.Is(err, controlplane.ErrCursorRegression) {
			t.Fatalf("CompleteAction(repeated cursor) error = %v, want ErrCursorRegression", err)
		}
	})
}

func TestRestartAmbiguousDispatchBlocksInsteadOfCreatingDuplicateWork(t *testing.T) {
	t.Parallel()

	store := controlmock.NewStore()
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskReady
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	attempt, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	action, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkAmbiguous(context.Background(), run.ID, action.ID); !errors.Is(err, controlplane.ErrAmbiguousDispatch) {
		t.Fatalf("MarkAmbiguous() error = %v, want ErrAmbiguousDispatch", err)
	}
	snapshot, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Attempts[attempt.ID].State != controlplane.AttemptBlocked ||
		snapshot.Run.Status != controlplane.StatusBlocked {
		t.Fatalf("ambiguous state = run %q attempt %q", snapshot.Run.Status, snapshot.Attempts[attempt.ID].State)
	}
}

func TestRestartReconcilesExternallyCompletedDispatchByStableAction(t *testing.T) {
	t.Parallel()

	store := controlmock.NewStore()
	harness := agentmock.New(completeCapabilities())
	registry, err := agentharness.NewRegistry(map[string]agentharness.AgentHarness{"codex": harness})
	if err != nil {
		t.Fatal(err)
	}
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock), controlplane.WithHarnessRegistry(registry))
	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskReady
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	attempt, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	action, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-fingerprint"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Dispatch(context.Background(), agentharness.DispatchWorkRequest{
		ActionID: action.ID, RequestDigest: action.RequestDigest,
		ControlRunID: run.ID, TaskID: "task-a", AttemptID: attempt.ID,
		Primitive: agentharness.PersistentSession, ProjectRefIDs: []string{"project-a"},
		PromptDigest: digest("prompt"), OwnershipDigest: digest("ownership"),
		FrozenInputDigest: digest("frozen"), Timeout: time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock), controlplane.WithHarnessRegistry(registry))
	recovered, err := restarted.Recover(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Actions[action.ID].Completed || len(recovered.Sessions) != 1 {
		t.Fatalf("Recover() did not persist reconciled create: %#v", recovered)
	}
	dispatchCalls := 0
	for _, call := range harness.Calls() {
		if call == "dispatch" {
			dispatchCalls++
		}
	}
	if dispatchCalls != 1 {
		t.Fatalf("dispatch calls after recovery = %d, want 1", dispatchCalls)
	}
}

func TestRestartPersistsAmbiguousCreateWhenReconciliationUnavailable(t *testing.T) {
	t.Parallel()

	store := controlmock.NewStore()
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskReady
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	attempt, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	action, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-ambiguous-create"))
	if err != nil {
		t.Fatal(err)
	}

	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	blocked, err := restarted.Recover(context.Background(), run.ID)
	if !errors.Is(err, controlplane.ErrAmbiguousCreate) {
		t.Fatalf("Recover(unavailable reconciliation) error = %v, want ErrAmbiguousCreate", err)
	}
	if blocked.Actions[action.ID].AmbiguityCode != "ambiguous_create" ||
		blocked.Attempts[attempt.ID].BlockCode != "ambiguous_create" ||
		blocked.Run.Status != controlplane.StatusBlocked {
		t.Fatalf("ambiguous create was not durably classified: %#v", blocked)
	}
	version := blocked.Version
	again, err := restarted.Recover(context.Background(), run.ID)
	if !errors.Is(err, controlplane.ErrAmbiguousCreate) || again.Version != version {
		t.Fatalf("Recover(classified) = version %d, %v; want stable version %d", again.Version, err, version)
	}
}

func TestRestartAfterPersistenceFailureReusesReservationAndActionIdentity(t *testing.T) {
	t.Parallel()

	saveFailure := errors.New("injected durable write failure")
	store := controlmock.NewStore()
	store.FailSave(1, saveFailure)
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskReady
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities()); !errors.Is(err, saveFailure) {
		t.Fatalf("CreateAttempt(injected failure) error = %v, want injected failure", err)
	}
	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	attempt, err := restarted.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Attempts) != 1 {
		t.Fatalf("restart created %d attempts, want 1", len(snapshot.Attempts))
	}
	action, err := restarted.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("dispatch"))
	if err != nil {
		t.Fatal(err)
	}

	// CreateAttempt was save 2 and PrepareAction was save 3.
	store.FailSave(4, saveFailure)
	if _, err := restarted.CompleteAction(context.Background(), run.ID, action.ID, agentharness.ActionResult{
		ActionID: action.ID, RuntimeWorkIDs: []string{"runtime-child-1"},
		ResultDigest: digest("dispatch-result"),
	}); !errors.Is(err, saveFailure) {
		t.Fatalf("CompleteAction(injected failure) error = %v, want injected failure", err)
	}
	restarted = controlplane.NewService(store, controlplane.WithClock(fixedClock))
	reused, err := restarted.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID != action.ID {
		t.Fatalf("recovered action ID = %q, want %q", reused.ID, action.ID)
	}
	if _, err := restarted.CompleteAction(context.Background(), run.ID, action.ID, agentharness.ActionResult{
		ActionID: action.ID, RuntimeWorkIDs: []string{"runtime-child-1"},
		ResultDigest: digest("dispatch-result"),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Attempts) != 1 || len(snapshot.Sessions) != 1 {
		t.Fatalf("restart duplicated lifecycle: attempts=%d sessions=%d", len(snapshot.Attempts), len(snapshot.Sessions))
	}
}

func TestRestartPreservesImmutableIntegratedEvidence(t *testing.T) {
	t.Parallel()

	store, service, runID, attempt := persistentService(t)
	evidence := controlplane.Evidence{
		ID: "evidence-1", TaskID: "task-a", AttemptID: attempt.ID,
		Branch: "codex/task-a", BaseSHA: digest("base-a"), HeadSHA: digest("head"),
		OwnedPathsDigest: digest("paths"),
		Tests: []controlplane.TestEvidence{{
			CommandDigest: digest("command"), ResultDigest: digest("pass"), Passed: true,
		}},
	}
	if _, err := service.RecordEvidence(context.Background(), runID, evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetDisposition(context.Background(), runID, attempt.ID, controlplane.Disposition{
		Kind: controlplane.DispositionIntegrated, EvidenceID: evidence.ID,
	}); err != nil {
		t.Fatal(err)
	}
	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	changed := evidence
	changed.HeadSHA = digest("different")
	if _, err := restarted.RecordEvidence(context.Background(), runID, changed); !errors.Is(err, controlplane.ErrEvidenceImmutable) {
		t.Fatalf("RecordEvidence(changed integrated evidence) error = %v, want ErrEvidenceImmutable", err)
	}
}

func TestAttachTerminalEvidenceRejectsMismatchedDigest(t *testing.T) {
	t.Parallel()

	_, service, runID, attempt := persistentService(t)
	evidence := controlplane.Evidence{
		ID: "terminal-evidence-binding", TaskID: "task-a", AttemptID: attempt.ID,
		BaseSHA: digest("base-a"), HeadSHA: digest("terminal-head"), OwnedPathsDigest: digest("terminal-paths"),
		Tests: []controlplane.TestEvidence{{CommandDigest: digest("terminal-command"), ResultDigest: digest("terminal-result"), Passed: true}},
	}
	if _, err := service.RecordEvidence(context.Background(), runID, evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AttachTerminalEvidence(context.Background(), runID, attempt.ID, controlplane.EvidenceRef{
		ID: evidence.ID, Digest: digest("wrong-evidence-binding"),
	}); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("AttachTerminalEvidence(mismatched digest) error = %v, want ErrActionConflict", err)
	}
}

func TestRestartAfterArchiveResultDoesNotLoseOrRepeatClose(t *testing.T) {
	t.Parallel()

	store, service, runID, attempt := persistentService(t)
	dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-archive"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-archive"},
		ResultDigest: digest("dispatch-result"),
	}); err != nil {
		t.Fatal(err)
	}
	acknowledgeRuntimeWithLedger(t, service, runID, attempt.ID, "runtime-archive")
	recordCallbackWithLedger(t, service, runID, attempt.ID, controlplane.CompletionCallback{
		AttemptID: attempt.ID, RuntimeChildID: "runtime-archive", Status: controlplane.CallbackDone,
		BaseSHA: digest("base-a"), HeadSHA: digest("head"), OwnedPathsDigest: digest("paths"),
		TestsDigest: digest("tests"), RecommendedParentAction: "verify and integrate",
	})
	wait, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionWait, digest("wait-archive"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, wait.ID, agentharness.ActionResult{
		ActionID: wait.ID, RuntimeWorkIDs: []string{"runtime-archive"},
		Cursor: "cursor-archive-terminal", CursorSequence: 1, ResultDigest: digest("wait-result"),
		Events: []agentharness.WorkEvent{{
			ID: "terminal", RuntimeWorkID: "runtime-archive",
			Cursor: "cursor-archive-terminal", CursorSequence: 1,
			Kind: "completed", Terminal: true, ResultDigest: digest("terminal"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	closeAction, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionClose, digest("close-archive"))
	if err != nil {
		t.Fatal(err)
	}
	closeResult := agentharness.ActionResult{
		ActionID: closeAction.ID, RuntimeWorkIDs: []string{"runtime-archive"},
		ResultDigest: digest("close-result"),
		CloseEvidence: agentharness.CloseEvidence{
			Kind: agentharness.CloseArchive, Receipt: "archive-receipt", Digest: digest("archive"),
		},
	}
	saveFailure := errors.New("archive receipt checkpoint failed")
	store.FailNextSave(saveFailure)
	if _, err := service.CompleteAction(context.Background(), runID, closeAction.ID, closeResult); !errors.Is(err, saveFailure) {
		t.Fatalf("CompleteAction(archive checkpoint failure) error = %v, want injected failure", err)
	}
	beforeRestart, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range beforeRestart.Sessions {
		if session.ArchiveReceipt != "" {
			t.Fatalf("failed checkpoint installed unpersisted archive receipt: %#v", session)
		}
	}
	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	if _, err := restarted.CompleteAction(context.Background(), runID, closeAction.ID, closeResult); err != nil {
		t.Fatal(err)
	}
	afterRestart, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRestart.Sessions) != 1 {
		t.Fatalf("restart produced %d sessions, want 1", len(afterRestart.Sessions))
	}
	for _, session := range afterRestart.Sessions {
		if session.ArchiveReceipt != "archive-receipt" {
			t.Fatalf("archive receipt after restart = %#v", session)
		}
	}
}

func TestCloseRequiresPrimitiveSpecificEvidenceDispositionAndZeroTypedGate(t *testing.T) {
	t.Parallel()

	state := lifecycleCompleteSnapshot()
	gate := controlplane.PendingWork(state)
	if gate != (controlplane.PendingWorkGate{}) {
		t.Fatalf("PendingWork(complete) = %#v, want zero", gate)
	}
	closed, err := controlplane.CloseSnapshot(state, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if closed.Run.Status != controlplane.StatusClosed || !closed.Run.Close.CombinedGatesPassed {
		t.Fatalf("CloseSnapshot() = %#v", closed.Run)
	}

	tests := []struct {
		name   string
		mutate func(*controlplane.Snapshot)
		field  func(controlplane.PendingWorkGate) int
	}{
		{"persistent unarchived", func(s *controlplane.Snapshot) {
			session := s.Sessions["session-1"]
			session.ArchiveReceipt = ""
			s.Sessions["session-1"] = session
		}, func(g controlplane.PendingWorkGate) int { return g.PersistentSessionsUnarchived }},
		{"ephemeral open", func(s *controlplane.Snapshot) {
			attempt := s.Attempts["attempt-ephemeral"]
			attempt.CloseEvidence = controlplane.WorkCloseEvidence{}
			s.Attempts[attempt.ID] = attempt
		}, func(g controlplane.PendingWorkGate) int { return g.EphemeralAttemptsOpen }},
		{"native unaggregated", func(s *controlplane.Snapshot) {
			attempt := s.Attempts["attempt-native"]
			attempt.CloseEvidence = controlplane.WorkCloseEvidence{}
			s.Attempts[attempt.ID] = attempt
		}, func(g controlplane.PendingWorkGate) int { return g.NativeFanoutsUnaggregated }},
		{"local active", func(s *controlplane.Snapshot) {
			attempt := s.Attempts["attempt-local"]
			attempt.CloseEvidence = controlplane.WorkCloseEvidence{}
			s.Attempts[attempt.ID] = attempt
		}, func(g controlplane.PendingWorkGate) int { return g.LocalAttemptsActive }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := controlplane.CloneSnapshot(state)
			test.mutate(&candidate)
			if _, err := controlplane.CloseSnapshot(candidate, fixedClock()); !errors.Is(err, controlplane.ErrCleanupIncomplete) {
				t.Fatalf("CloseSnapshot() error = %v, want ErrCleanupIncomplete", err)
			}
			gate := controlplane.PendingWork(candidate)
			if test.field(gate) != 1 || gate.TotalPendingWork != 1 {
				t.Fatalf("PendingWork() = %#v", gate)
			}
		})
	}
}

func TestCloseRejectsTerminalTaskWithoutAttemptAndEmptyCombinedGates(t *testing.T) {
	t.Parallel()

	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskCompleted
	graph.CombinedGates[0].Passed = true
	graph.CombinedGates[0].Evidence = digest("combined-pass")
	snapshot, err := controlplane.NewSnapshot(validRun(graph), graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlplane.CloseSnapshot(snapshot, fixedClock()); !errors.Is(err, controlplane.ErrClosePrecondition) {
		t.Fatalf("CloseSnapshot(no attempts) error = %v, want ErrClosePrecondition", err)
	}

	complete := lifecycleCompleteSnapshot()
	complete.Graph.CombinedGates = nil
	if _, err := controlplane.CloseSnapshot(complete, fixedClock()); !errors.Is(err, controlplane.ErrInvalidGraph) {
		t.Fatalf("CloseSnapshot(no combined gates) error = %v, want ErrInvalidGraph", err)
	}
	complete = lifecycleCompleteSnapshot()
	complete.Graph.CombinedGates[0].Evidence = ""
	if _, err := controlplane.CloseSnapshot(complete, fixedClock()); !errors.Is(err, controlplane.ErrInvalidGraph) {
		t.Fatalf("CloseSnapshot(combined gate without evidence) error = %v, want ErrInvalidGraph", err)
	}
}

func validRun(graph controlplane.TaskGraph) controlplane.ControlRun {
	return controlplane.ControlRun{
		SchemaVersion: controlplane.SchemaVersion,
		ID:            graph.ControlRunID, PrincipalID: "principal-1", GoalDigest: digest("goal"),
		GraphRevision: graph.Revision, Status: controlplane.StatusOpen,
		CreatedAt: fixedClock(), UpdatedAt: fixedClock(),
	}
}

func lifecycleCompleteSnapshot() controlplane.Snapshot {
	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskCompleted
	graph.CombinedGates[0].Passed = true
	graph.CombinedGates[0].Evidence = digest("combined-pass")
	state, err := controlplane.NewSnapshot(validRun(graph), graph)
	if err != nil {
		panic(err)
	}
	state.Attempts = map[string]controlplane.PlacementAttempt{
		"attempt-persistent": completedAttempt("attempt-persistent", agentharness.PersistentSession, controlplane.WorkCloseEvidence{Kind: controlplane.CloseArchive, Receipt: "archive-1", Digest: digest("archive-1")}),
		"attempt-ephemeral":  completedAttempt("attempt-ephemeral", agentharness.EphemeralSubagent, controlplane.WorkCloseEvidence{Kind: controlplane.CloseRuntime, Receipt: "runtime-close-1", Digest: digest("runtime-close-1")}),
		"attempt-native":     completedAttempt("attempt-native", agentharness.HarnessNativeParallel, controlplane.WorkCloseEvidence{Kind: controlplane.CloseAggregate, Receipt: "aggregate-1", Digest: digest("aggregate-1")}),
		"attempt-local":      completedAttempt("attempt-local", agentharness.LocalSequential, controlplane.WorkCloseEvidence{Kind: controlplane.CloseInactive, Receipt: "inactive-1", Digest: digest("inactive-1")}),
	}
	registrationDigest := controlplane.RegistrationMessageDigest(state.Run.ID, "attempt-persistent", "runtime-child-1")
	state.Sessions = map[string]controlplane.AgentSession{
		"session-1": {
			ID: "session-1", AttemptID: "attempt-persistent", TaskID: "task-a",
			HarnessID: "codex", RuntimeChildID: "runtime-child-1",
			RuntimeIDAcknowledged: true, RegistrationMessageDigest: registrationDigest,
			State:          controlplane.SessionCompleted,
			Disposition:    controlplane.Disposition{Kind: controlplane.DispositionIntegrated, EvidenceID: "evidence-persistent"},
			ArchiveReceipt: "archive-1",
		},
	}
	state.Evidence = map[string]controlplane.Evidence{
		"evidence-persistent": completedEvidence("evidence-persistent", "attempt-persistent"),
		"evidence-ephemeral":  completedEvidence("evidence-ephemeral", "attempt-ephemeral"),
		"evidence-native":     completedEvidence("evidence-native", "attempt-native"),
		"evidence-local":      completedEvidence("evidence-local", "attempt-local"),
	}
	for id, attempt := range state.Attempts {
		evidence := state.Evidence[attempt.TerminalEvidence.ID]
		attempt.TerminalEvidence.Digest = controlplane.EvidenceDigest(evidence)
		state.Attempts[id] = attempt
	}
	for id, attempt := range state.Attempts {
		if attempt.Primitive == agentharness.LocalSequential {
			continue
		}
		actionID := "action-close-" + id
		action := controlplane.LifecycleAction{
			ID: actionID, AttemptID: id, Kind: agentharness.ActionClose,
			RequestDigest: digest("request-" + actionID), Completed: true,
			Result: agentharness.ActionResult{
				ActionID: actionID, RuntimeWorkIDs: append([]string(nil), attempt.RuntimeWorkIDs...),
				ResultDigest: digest("result-" + actionID), CloseEvidence: harnessCloseEvidence(attempt.CloseEvidence),
			},
		}
		state.Actions[actionID] = action
		attempt.ActionIDs = append(attempt.ActionIDs, actionID)
		state.Attempts[id] = attempt
	}
	acknowledgeID := "action-ack-attempt-persistent"
	state.Actions[acknowledgeID] = controlplane.LifecycleAction{
		ID: acknowledgeID, AttemptID: "attempt-persistent", Kind: agentharness.ActionAcknowledge,
		RequestDigest: registrationDigest, Completed: true,
		Result: agentharness.ActionResult{
			ActionID: acknowledgeID, RuntimeWorkIDs: []string{"runtime-child-1"},
			ResultDigest: registrationDigest, MessageReceipt: "registration-message-receipt",
		},
	}
	persistent := state.Attempts["attempt-persistent"]
	persistent.ActionIDs = append(persistent.ActionIDs, acknowledgeID)
	state.Attempts[persistent.ID] = persistent
	return state
}

func completedAttempt(id string, primitive agentharness.Primitive, close controlplane.WorkCloseEvidence) controlplane.PlacementAttempt {
	evidenceID := strings.Replace(id, "attempt-", "evidence-", 1)
	var runtimeWorkIDs []string
	switch primitive {
	case agentharness.PersistentSession:
		runtimeWorkIDs = []string{"runtime-child-1"}
	case agentharness.EphemeralSubagent:
		runtimeWorkIDs = []string{"runtime-ephemeral-1"}
	}
	return controlplane.PlacementAttempt{
		ID: id, TaskID: "task-a", Primitive: primitive, LifecycleOwner: "parent",
		CapabilitySnapshot: completeCapabilities(),
		State:              controlplane.AttemptCompleted,
		TerminalEvidence:   controlplane.EvidenceRef{ID: evidenceID, Digest: digest(evidenceID)},
		Disposition:        controlplane.Disposition{Kind: controlplane.DispositionIntegrated, EvidenceID: evidenceID},
		CloseEvidence:      close,
		RuntimeWorkIDs:     runtimeWorkIDs,
		TerminalObserved:   true,
		ActionIDs:          []string{},
		ObservedEvents:     map[string]string{},
	}
}

func harnessCloseEvidence(evidence controlplane.WorkCloseEvidence) agentharness.CloseEvidence {
	kind := agentharness.CloseInactive
	switch evidence.Kind {
	case controlplane.CloseArchive:
		kind = agentharness.CloseArchive
	case controlplane.CloseRuntime:
		kind = agentharness.CloseRuntime
	case controlplane.CloseAggregate:
		kind = agentharness.CloseAggregate
	case controlplane.CloseCanceled:
		kind = agentharness.CloseCanceled
	}
	return agentharness.CloseEvidence{Kind: kind, Receipt: evidence.Receipt, Digest: evidence.Digest}
}

func completedEvidence(id, attemptID string) controlplane.Evidence {
	return controlplane.Evidence{
		ID: id, TaskID: "task-a", AttemptID: attemptID,
		Branch: "codex/task-a", BaseSHA: digest("base-a"), HeadSHA: digest("head-" + attemptID),
		OwnedPathsDigest: digest("paths-" + attemptID),
		Tests: []controlplane.TestEvidence{{
			CommandDigest: digest("command-" + attemptID),
			ResultDigest:  digest("pass-" + attemptID),
			Passed:        true,
		}},
	}
}

func fixedClock() time.Time {
	return time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
}
