package controlplane_test

import (
	"context"
	"errors"
	"testing"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane"
	controlmock "github.com/araihu/paje/internal/controlplane/mock"
)

func TestPersistentLifecycleRequiresExactAcknowledgedRuntimeIDAndScopedCallback(t *testing.T) {
	t.Parallel()

	store, service, runID, attempt := persistentService(t)
	dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-child-1"},
		Cursor: "cursor-1", CursorSequence: 1, ResultDigest: digest("dispatch-result"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcknowledgeRuntimeID(context.Background(), runID, attempt.ID, "derived-from-worktree"); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("AcknowledgeRuntimeID(foreign) error = %v, want ErrActionConflict", err)
	}
	session := acknowledgeRuntimeWithLedger(t, service, runID, attempt.ID, "runtime-child-1")
	if !session.RuntimeIDAcknowledged {
		t.Fatal("runtime ID was not acknowledged")
	}

	callback := controlplane.CompletionCallback{
		AttemptID: attempt.ID, RuntimeChildID: "runtime-child-1",
		Status: controlplane.CallbackDone, Branch: "codex/task-a",
		BaseSHA: digest("base"), HeadSHA: digest("head"),
		OwnedPathsDigest: digest("paths"), TestsDigest: digest("tests"),
		RecommendedParentAction: "verify and integrate",
	}
	if _, err := service.RecordCallback(context.Background(), runID, controlplane.CompletionCallback{
		AttemptID: attempt.ID, RuntimeChildID: "foreign", Status: controlplane.CallbackDone,
		BaseSHA: digest("base"), HeadSHA: digest("head"),
		OwnedPathsDigest: digest("paths"), TestsDigest: digest("tests"),
		RecommendedParentAction: "ignore",
	}); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("RecordCallback(foreign) error = %v, want ErrActionConflict", err)
	}
	recorded := recordCallbackWithLedger(t, service, runID, attempt.ID, callback)
	if recorded.RuntimeChildID != "runtime-child-1" {
		t.Fatalf("RecordCallback() = %#v", recorded)
	}
	snapshot, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Sessions[session.ID].State != controlplane.SessionCompleted ||
		snapshot.Callbacks[attempt.ID].HeadSHA != callback.HeadSHA {
		t.Fatalf("callback did not complete exact session: %#v", snapshot)
	}
}

func TestHandshakeHelpersRejectAcknowledgementWithoutCompletedLedgerAction(t *testing.T) {
	t.Parallel()

	_, service, runID, attempt := persistentService(t)
	dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-handshake-ledger"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-ledger"},
		ResultDigest: digest("dispatch-result"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcknowledgeRuntimeID(context.Background(), runID, attempt.ID, "runtime-ledger"); !errors.Is(err, controlplane.ErrActionIncomplete) {
		t.Fatalf("AcknowledgeRuntimeID(without ledger action) error = %v, want ErrActionIncomplete", err)
	}
	registrationDigest := controlplane.RegistrationMessageDigest(runID, attempt.ID, "runtime-ledger")
	acknowledge, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionAcknowledge, registrationDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, acknowledge.ID, agentharness.ActionResult{
		ActionID: acknowledge.ID, RuntimeWorkIDs: []string{"runtime-ledger"},
		ResultDigest: registrationDigest, MessageReceipt: "registration-message-receipt",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcknowledgeRuntimeID(context.Background(), runID, attempt.ID, "runtime-ledger"); err != nil {
		t.Fatal(err)
	}

	callback := controlplane.CompletionCallback{
		AttemptID: attempt.ID, RuntimeChildID: "runtime-ledger", Status: controlplane.CallbackDone,
		BaseSHA: digest("base-a"), HeadSHA: digest("head-ledger"), OwnedPathsDigest: digest("paths-ledger"),
		TestsDigest: digest("tests-ledger"), RecommendedParentAction: "verify ledger callback",
	}
	if _, err := service.RecordCallback(context.Background(), runID, callback); !errors.Is(err, controlplane.ErrActionIncomplete) {
		t.Fatalf("RecordCallback(without ledger action) error = %v, want ErrActionIncomplete", err)
	}
	callbackDigest := controlplane.CompletionCallbackDigest(callback)
	callbackAction, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionCallback, callbackDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, callbackAction.ID, agentharness.ActionResult{
		ActionID: callbackAction.ID, RuntimeWorkIDs: []string{"runtime-ledger"}, ResultDigest: callbackDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RecordCallback(context.Background(), runID, callback); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentCloseRequiresCallbackPollingAndArchiveReceipt(t *testing.T) {
	t.Parallel()

	store, service, runID, attempt := persistentService(t)
	dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-close"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-child-close"},
		Cursor: "cursor-dispatch", CursorSequence: 1, ResultDigest: digest("dispatch-result"),
	}); err != nil {
		t.Fatal(err)
	}
	acknowledgeRuntimeWithLedger(t, service, runID, attempt.ID, "runtime-child-close")
	recordCallbackWithLedger(t, service, runID, attempt.ID, controlplane.CompletionCallback{
		AttemptID: attempt.ID, RuntimeChildID: "runtime-child-close",
		Status: controlplane.CallbackDone, BaseSHA: digest("base-a"), HeadSHA: digest("head"),
		OwnedPathsDigest: digest("paths"), TestsDigest: digest("tests"),
		RecommendedParentAction: "verify and integrate",
	})
	closeAction, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionClose, digest("archive"))
	if err != nil {
		t.Fatal(err)
	}
	closeResult := agentharness.ActionResult{
		ActionID: closeAction.ID, RuntimeWorkIDs: []string{"runtime-child-close"},
		ResultDigest: digest("archive-result"),
		CloseEvidence: agentharness.CloseEvidence{
			Kind: agentharness.CloseArchive, Receipt: "archive-receipt", Digest: digest("archive-evidence"),
		},
	}
	if _, err := service.CompleteAction(context.Background(), runID, closeAction.ID, closeResult); !errors.Is(err, controlplane.ErrActionIncomplete) {
		t.Fatalf("CompleteAction(archive before polling) error = %v, want ErrActionIncomplete", err)
	}
	waitAction, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionWait, digest("poll"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, waitAction.ID, agentharness.ActionResult{
		ActionID: waitAction.ID, RuntimeWorkIDs: []string{"runtime-child-close"},
		Cursor: "cursor-terminal", CursorSequence: 2, ResultDigest: digest("poll-result"),
		Events: []agentharness.WorkEvent{{
			ID: "event-terminal", RuntimeWorkID: "runtime-child-close",
			Cursor: "cursor-terminal", CursorSequence: 2, Kind: "completed",
			ResultDigest: digest("terminal"), Terminal: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, closeAction.ID, closeResult); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Attempts[attempt.ID].CloseEvidence.Receipt != "archive-receipt" {
		t.Fatalf("attempt archive evidence = %#v", snapshot.Attempts[attempt.ID].CloseEvidence)
	}
	for _, session := range snapshot.Sessions {
		if session.ArchiveReceipt != "archive-receipt" || session.State != controlplane.SessionArchived {
			t.Fatalf("session archive state = %#v", session)
		}
	}
}

func TestDuplicateEventReplayCannotAlterTrustedTerminalState(t *testing.T) {
	t.Parallel()

	store, service, runID, attempt := persistentService(t)
	dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-event-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-event-replay"},
		Cursor: "cursor-1", CursorSequence: 1, ResultDigest: digest("dispatch-event-replay-result"),
	}); err != nil {
		t.Fatal(err)
	}
	observe, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionObserve, digest("observe-event-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, observe.ID, agentharness.ActionResult{
		ActionID: observe.ID, RuntimeWorkIDs: []string{"runtime-event-replay"},
		Cursor: "cursor-2", CursorSequence: 2, ResultDigest: digest("observe-event-replay-result"),
		Events: []agentharness.WorkEvent{{
			ID: "event-replayed", RuntimeWorkID: "runtime-event-replay", Cursor: "cursor-2", CursorSequence: 2,
			Kind: "progress", ResultDigest: digest("event-replayed"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	replay, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionWait, digest("wait-event-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, replay.ID, agentharness.ActionResult{
		ActionID: replay.ID, RuntimeWorkIDs: []string{"runtime-event-replay"},
		Cursor: "cursor-3", CursorSequence: 3, ResultDigest: digest("wait-event-replay-result"),
		Events: []agentharness.WorkEvent{{
			ID: "event-replayed", RuntimeWorkID: "runtime-event-replay", Cursor: "cursor-3", CursorSequence: 3,
			Kind: "completed", ResultDigest: digest("event-replayed"), Terminal: true,
		}},
	}); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("CompleteAction(tampered duplicate event) error = %v, want ErrActionConflict", err)
	}
	snapshot, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Attempts[attempt.ID].TerminalObserved {
		t.Fatal("tampered duplicate event marked terminal observed")
	}
}

func TestExactDuplicateEventReplayCompletesIdempotently(t *testing.T) {
	t.Parallel()

	store, service, runID, attempt := persistentService(t)
	dispatch, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionDispatch, digest("dispatch-exact-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-exact-replay"},
		Cursor: "cursor-1", CursorSequence: 1, ResultDigest: digest("dispatch-exact-replay-result"),
	}); err != nil {
		t.Fatal(err)
	}
	terminal := agentharness.WorkEvent{
		ID: "event-exact-replay", RuntimeWorkID: "runtime-exact-replay",
		Cursor: "cursor-2", CursorSequence: 2, Kind: "completed",
		ResultDigest: digest("event-exact-replay"), Terminal: true,
	}
	observe, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionObserve, digest("observe-exact-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, observe.ID, agentharness.ActionResult{
		ActionID: observe.ID, RuntimeWorkIDs: []string{"runtime-exact-replay"},
		Cursor: "cursor-2", CursorSequence: 2, Events: []agentharness.WorkEvent{terminal},
		ResultDigest: digest("observe-exact-replay-result"),
	}); err != nil {
		t.Fatal(err)
	}
	evidence := controlplane.Evidence{
		ID: "evidence-exact-replay", TaskID: "task-a", AttemptID: attempt.ID,
		BaseSHA: digest("base-a"), HeadSHA: digest("head-exact-replay"),
		OwnedPathsDigest: digest("paths-exact-replay"),
		Tests: []controlplane.TestEvidence{{
			CommandDigest: digest("command-exact-replay"),
			ResultDigest:  digest("result-exact-replay"), Passed: true,
		}},
	}
	if _, err := service.RecordEvidence(context.Background(), runID, evidence); err != nil {
		t.Fatal(err)
	}
	reference := controlplane.EvidenceRef{ID: evidence.ID, Digest: controlplane.EvidenceDigest(evidence)}
	if _, err := service.AttachTerminalEvidence(context.Background(), runID, attempt.ID, reference); err != nil {
		t.Fatal(err)
	}
	disposition := controlplane.Disposition{Kind: controlplane.DispositionIntegrated, EvidenceID: evidence.ID}
	if _, err := service.SetDisposition(context.Background(), runID, attempt.ID, disposition); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := service.PrepareAction(context.Background(), runID, attempt.ID, agentharness.ActionWait, digest("wait-exact-replay"))
	if err != nil {
		t.Fatal(err)
	}
	result := agentharness.ActionResult{
		ActionID: replay.ID, RuntimeWorkIDs: []string{"runtime-exact-replay"},
		Cursor: "cursor-2", CursorSequence: 2, Events: []agentharness.WorkEvent{terminal},
		ResultDigest: digest("wait-exact-replay-result"),
	}
	completed, err := service.CompleteAction(context.Background(), runID, replay.ID, result)
	if err != nil {
		t.Fatalf("CompleteAction(exact duplicate event) error = %v", err)
	}
	after, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	beforeAttempt, afterAttempt := before.Attempts[attempt.ID], after.Attempts[attempt.ID]
	if !completed.Completed || completed.Result.ActionID != replay.ID ||
		after.Actions[replay.ID].Result.ResultDigest != result.ResultDigest {
		t.Fatalf("replay lifecycle action was not durably completed: %#v", after.Actions[replay.ID])
	}
	if afterAttempt.LastCursor != beforeAttempt.LastCursor ||
		afterAttempt.CursorSequence != beforeAttempt.CursorSequence ||
		len(afterAttempt.ObservedEvents) != len(beforeAttempt.ObservedEvents) ||
		afterAttempt.ObservedEvents[terminal.ID] != terminal.ResultDigest {
		t.Fatalf("exact replay changed cursor or observed events: before=%#v after=%#v", beforeAttempt, afterAttempt)
	}
	if afterAttempt.TerminalObserved != beforeAttempt.TerminalObserved ||
		afterAttempt.TerminalEvidence != reference ||
		afterAttempt.Disposition != disposition {
		t.Fatalf("exact replay reapplied terminal state: before=%#v after=%#v", beforeAttempt, afterAttempt)
	}
}

func TestScopedMailboxAllowsDeclaredDependencyAndRejectsForeignProject(t *testing.T) {
	t.Parallel()

	store := controlmock.NewStore()
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	graph := validGraph()
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	message := controlplane.Message{
		ID: "message-1", FromTaskID: "task-a", ToTaskID: "task-b",
		Kind: controlplane.MessageDependencyHandoff, Digest: digest("message"),
	}
	if _, err := service.SendMessage(context.Background(), run.ID, message); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendMessage(context.Background(), run.ID, controlplane.Message{
		ID: "message-parent-steering", FromTaskID: controlplane.ParentAddress, ToTaskID: "task-a",
		Kind: controlplane.MessageSteering, Digest: digest("steering"),
	}); err != nil {
		t.Fatalf("SendMessage(parent steering) error = %v", err)
	}
	if _, err := service.SendMessage(context.Background(), run.ID, controlplane.Message{
		ID: "message-foreign", FromTaskID: "task-a", ToTaskID: "foreign",
		Kind: controlplane.MessageSteering, Digest: digest("foreign"),
	}); !errors.Is(err, controlplane.ErrInvalidGraph) {
		t.Fatalf("SendMessage(foreign) error = %v, want ErrInvalidGraph", err)
	}
	acknowledged, err := service.AcknowledgeMessage(context.Background(), run.ID, message.ID, "task-b")
	if err != nil {
		t.Fatal(err)
	}
	if !acknowledged.Acknowledged {
		t.Fatal("message acknowledgement was not persisted")
	}
}

func TestPrimitiveCloseActionCompletesEphemeralWithoutCreatingSession(t *testing.T) {
	t.Parallel()

	store := controlmock.NewStore()
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskReady
	graph.Tasks[0].Placement = controlplane.ExecutionPlacement{
		ParallelismPrimitive: agentharness.EphemeralSubagent,
		ExecutionPlacement:   "codex_local_subagent", PlacementRationale: "short review",
		CapabilityRequirements: []string{
			agentharness.CapDispatch, agentharness.CapObserve,
			agentharness.CapWait, agentharness.CapRuntimeClose,
		},
		LifecycleOwner: "parent", Fallback: "local_sequential",
	}
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	attempt, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), run.ID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"ephemeral-child"},
		ResultDigest: digest("dispatch-result"),
	}); err != nil {
		t.Fatal(err)
	}
	waitAction, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionWait, digest("wait"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), run.ID, waitAction.ID, agentharness.ActionResult{
		ActionID: waitAction.ID, RuntimeWorkIDs: []string{"ephemeral-child"}, ResultDigest: digest("wait-result"),
		Events: []agentharness.WorkEvent{{
			ID: "terminal-1", RuntimeWorkID: "ephemeral-child", Kind: "completed", Terminal: true, ResultDigest: digest("terminal"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CloseAttempt(context.Background(), run.ID, attempt.ID, controlplane.WorkCloseEvidence{
		Kind: controlplane.CloseRuntime, Receipt: "forged-runtime-close", Digest: digest("forged-close"),
	}); !errors.Is(err, controlplane.ErrActionIncomplete) {
		t.Fatalf("CloseAttempt(non-local without completed action) error = %v, want ErrActionIncomplete", err)
	}
	closeAction, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionClose, digest("close"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), run.ID, closeAction.ID, agentharness.ActionResult{
		ActionID: closeAction.ID, RuntimeWorkIDs: []string{"ephemeral-child"}, ResultDigest: digest("close-result"),
		CloseEvidence: agentharness.CloseEvidence{
			Kind: agentharness.CloseRuntime, Receipt: "runtime-close-1", Digest: digest("close-evidence"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sessions) != 0 ||
		snapshot.Attempts[attempt.ID].CloseEvidence.Kind != controlplane.CloseRuntime {
		t.Fatalf("ephemeral close state = %#v", snapshot)
	}
}

func TestSnapshotRejectsNonLocalCloseEvidenceWithoutCompletedAction(t *testing.T) {
	t.Parallel()

	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].Placement = controlplane.ExecutionPlacement{
		ParallelismPrimitive: agentharness.EphemeralSubagent,
		ExecutionPlacement:   "codex_local_subagent", PlacementRationale: "short review",
		CapabilityRequirements: append([]string(nil), agentharness.RequiredCapabilities(agentharness.EphemeralSubagent)...),
		LifecycleOwner:         "parent", Fallback: "local_sequential",
	}
	snapshot, err := controlplane.NewSnapshot(validRun(graph), graph)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Attempts["attempt-forged-close"] = controlplane.PlacementAttempt{
		ID: "attempt-forged-close", TaskID: "task-a", Primitive: agentharness.EphemeralSubagent,
		CapabilitySnapshot: completeCapabilities(), LifecycleOwner: "parent", State: controlplane.AttemptActive,
		RuntimeWorkIDs: []string{"runtime-forged-close"}, ActionIDs: []string{}, ObservedEvents: map[string]string{},
		CloseEvidence: controlplane.WorkCloseEvidence{
			Kind: controlplane.CloseRuntime, Receipt: "forged-runtime-close", Digest: digest("forged-runtime-close"),
		},
	}
	if err := controlplane.ValidateSnapshot(snapshot); !errors.Is(err, controlplane.ErrInvalidRecord) {
		t.Fatalf("ValidateSnapshot(forged non-local close) error = %v, want ErrInvalidRecord", err)
	}
}

func TestClosePersistsClosingStateWhenCleanupIsIncomplete(t *testing.T) {
	t.Parallel()

	store := controlmock.NewStore()
	state := lifecycleCompleteSnapshot()
	session := state.Sessions["session-1"]
	session.ArchiveReceipt = ""
	state.Sessions[session.ID] = session
	if _, err := store.Create(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	if _, err := service.CloseControlRun(context.Background(), state.Run.ID); !errors.Is(err, controlplane.ErrCleanupIncomplete) {
		t.Fatalf("CloseControlRun() error = %v, want ErrCleanupIncomplete", err)
	}
	reloaded, err := store.Load(context.Background(), state.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Run.Status != controlplane.StatusClosing ||
		reloaded.Run.Close.Code != "cleanup_incomplete" ||
		reloaded.Run.Close.Pending.PersistentSessionsUnarchived != 1 {
		t.Fatalf("persisted close state = %#v", reloaded.Run.Close)
	}
}

func TestEveryPrimitiveCreatesAttemptButOnlyPersistentCreatesSession(t *testing.T) {
	t.Parallel()

	for _, primitive := range []agentharness.Primitive{
		agentharness.HarnessNativeParallel,
		agentharness.LocalSequential,
	} {
		t.Run(string(primitive), func(t *testing.T) {
			store := controlmock.NewStore()
			service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
			graph := validGraph()
			graph.Tasks = graph.Tasks[:1]
			graph.IntegrationOrder = graph.IntegrationOrder[:1]
			graph.Tasks[0].State = controlplane.TaskReady
			graph.Tasks[0].Placement = controlplane.ExecutionPlacement{
				ParallelismPrimitive: primitive,
				ExecutionPlacement:   "test_" + string(primitive),
				PlacementRationale:   "primitive lifecycle proof",
				CapabilityRequirements: append(
					[]string(nil), agentharness.RequiredCapabilities(primitive)...,
				),
				LifecycleOwner: "parent", Fallback: "block",
			}
			run := validRun(graph)
			if _, err := service.Create(context.Background(), run, graph); err != nil {
				t.Fatal(err)
			}
			attempt, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			if primitive == agentharness.LocalSequential {
				if _, err := service.PrepareAction(
					context.Background(), run.ID, attempt.ID,
					agentharness.ActionDispatch, digest("dispatch"),
				); !errors.Is(err, agentharness.ErrUnsupportedOperation) {
					t.Fatalf("local dispatch error = %v, want ErrUnsupportedOperation", err)
				}
			} else {
				action, err := service.PrepareAction(
					context.Background(), run.ID, attempt.ID,
					agentharness.ActionDispatch, digest("dispatch"),
				)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := service.CompleteAction(context.Background(), run.ID, action.ID, agentharness.ActionResult{
					ActionID: action.ID, ResultDigest: digest("dispatch-result"),
				}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := store.Load(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Attempts) != 1 || len(snapshot.Sessions) != 0 {
				t.Fatalf("%s lifecycle = attempts %d sessions %d", primitive, len(snapshot.Attempts), len(snapshot.Sessions))
			}
		})
	}
}

func persistentService(t *testing.T) (*controlmock.Store, *controlplane.Service, string, controlplane.PlacementAttempt) {
	t.Helper()
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
	return store, service, run.ID, attempt
}

func acknowledgeRuntimeWithLedger(
	t *testing.T,
	service *controlplane.Service,
	runID, attemptID, runtimeChildID string,
) controlplane.AgentSession {
	t.Helper()
	registrationDigest := controlplane.RegistrationMessageDigest(runID, attemptID, runtimeChildID)
	action, err := service.PrepareAction(context.Background(), runID, attemptID, agentharness.ActionAcknowledge, registrationDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, action.ID, agentharness.ActionResult{
		ActionID: action.ID, RuntimeWorkIDs: []string{runtimeChildID},
		ResultDigest: registrationDigest, MessageReceipt: "registration-message-receipt",
	}); err != nil {
		t.Fatal(err)
	}
	session, err := service.AcknowledgeRuntimeID(context.Background(), runID, attemptID, runtimeChildID)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func recordCallbackWithLedger(
	t *testing.T,
	service *controlplane.Service,
	runID, attemptID string,
	callback controlplane.CompletionCallback,
) controlplane.CompletionCallback {
	t.Helper()
	callbackDigest := controlplane.CompletionCallbackDigest(callback)
	action, err := service.PrepareAction(context.Background(), runID, attemptID, agentharness.ActionCallback, callbackDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, action.ID, agentharness.ActionResult{
		ActionID: action.ID, RuntimeWorkIDs: []string{callback.RuntimeChildID}, ResultDigest: callbackDigest,
	}); err != nil {
		t.Fatal(err)
	}
	recorded, err := service.RecordCallback(context.Background(), runID, callback)
	if err != nil {
		t.Fatal(err)
	}
	return recorded
}
