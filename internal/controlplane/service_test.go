package controlplane_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane"
	controlmock "github.com/araihu/paje/internal/controlplane/mock"
)

func TestCompleteSendAtomicallyRejectsCompetingChangedMessage(t *testing.T) {
	store, service, runID, attempt := persistentService(t)
	activatePersistentAttempt(t, service, runID, attempt.ID, "runtime-atomic-send", "cursor-1", 1)

	firstMessage := controlplane.Message{
		ID: "message-shared", FromTaskID: controlplane.ParentAddress, ToTaskID: "task-a",
		Kind: controlplane.MessageSteering, Digest: digest("message-first"),
	}
	secondMessage := firstMessage
	secondMessage.Digest = digest("message-second")
	firstAction, err := service.PrepareAction(
		context.Background(), runID, attempt.ID, agentharness.ActionSend, testSendRequestDigest(firstMessage),
	)
	if err != nil {
		t.Fatal(err)
	}
	secondAction, err := service.PrepareAction(
		context.Background(), runID, attempt.ID, agentharness.ActionSend, testSendRequestDigest(secondMessage),
	)
	if err != nil {
		t.Fatal(err)
	}

	barrier := newLoadBarrierStore(store, 2)
	concurrent := controlplane.NewService(barrier, controlplane.WithClock(fixedClock))
	type outcome struct {
		actionID string
		err      error
	}
	outcomes := make(chan outcome, 2)
	complete := func(action controlplane.LifecycleAction, message controlplane.Message, label string) {
		_, _, _, err := concurrent.CompleteSend(
			context.Background(), runID, attempt.ID, action.ID,
			agentharness.ActionResult{
				ActionID: action.ID, RuntimeWorkIDs: []string{"runtime-atomic-send"},
				ResultDigest: digest("result-" + label), MessageReceipt: "receipt-" + label,
			},
			message,
		)
		outcomes <- outcome{actionID: action.ID, err: err}
	}
	go complete(firstAction, firstMessage, "first")
	go complete(secondAction, secondMessage, "second")

	results := []outcome{<-outcomes, <-outcomes}
	succeeded := ""
	failed := ""
	for _, result := range results {
		if result.err == nil {
			succeeded = result.actionID
			continue
		}
		if !errors.Is(result.err, controlplane.ErrVersionConflict) &&
			!errors.Is(result.err, controlplane.ErrActionConflict) {
			t.Fatalf("CompleteSend(competing message) error = %v", result.err)
		}
		failed = result.actionID
	}
	if succeeded == "" || failed == "" || succeeded == failed {
		t.Fatalf("competing outcomes = %#v, want exactly one success", results)
	}

	snapshot, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Actions[succeeded].Completed || snapshot.Actions[failed].Completed {
		t.Fatalf("competing actions = winner %#v, loser %#v", snapshot.Actions[succeeded], snapshot.Actions[failed])
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[firstMessage.ID].ID == "" {
		t.Fatalf("messages = %#v, want one durable winner", snapshot.Messages)
	}
	completedSendActions := 0
	for _, action := range snapshot.Actions {
		if action.Kind == agentharness.ActionSend && action.Completed {
			completedSendActions++
		}
	}
	if completedSendActions != 1 {
		t.Fatalf("completed send actions = %d, want 1", completedSendActions)
	}

	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	losingMessage := firstMessage
	if snapshot.Messages[firstMessage.ID].Digest == firstMessage.Digest {
		losingMessage = secondMessage
	}
	losingResult := agentharness.ActionResult{
		ActionID: failed, RuntimeWorkIDs: []string{"runtime-atomic-send"},
		ResultDigest: digest("restart-loser"), MessageReceipt: "restart-loser-receipt",
	}
	beforeRestart, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := restarted.CompleteSend(
		context.Background(), runID, attempt.ID, failed, losingResult, losingMessage,
	); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("CompleteSend(loser after restart) error = %v, want ErrActionConflict", err)
	}
	afterRestart, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.Actions[failed].Completed || afterRestart.Run.EventCursor != beforeRestart.Run.EventCursor ||
		len(afterRestart.Events) != len(beforeRestart.Events) {
		t.Fatalf("losing retry mutated state: before=%#v after=%#v", beforeRestart, afterRestart)
	}
}

func TestCompleteSendTreatsAcknowledgementAsServerOwnedAndReplaysAfterRestart(t *testing.T) {
	store, service, runID, attempt := persistentService(t)
	activatePersistentAttempt(t, service, runID, attempt.ID, "runtime-send-replay", "cursor-1", 1)

	message := controlplane.Message{
		ID: "message-replay", FromTaskID: controlplane.ParentAddress, ToTaskID: "task-a",
		Kind: controlplane.MessageSteering, Digest: digest("message-replay"),
	}
	forged := message
	forged.Acknowledged = true
	if testSendRequestDigest(forged) != testSendRequestDigest(message) {
		t.Fatal("test protocol digest unexpectedly includes server-owned acknowledgement")
	}
	action, err := service.PrepareAction(
		context.Background(), runID, attempt.ID, agentharness.ActionSend, testSendRequestDigest(message),
	)
	if err != nil {
		t.Fatal(err)
	}
	result := agentharness.ActionResult{
		ActionID: action.ID, RuntimeWorkIDs: []string{"runtime-send-replay"},
		ResultDigest: digest("send-replay-result"), MessageReceipt: "send-replay-receipt",
	}
	beforeForged, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.CompleteSend(
		context.Background(), runID, attempt.ID, action.ID, result, forged,
	); !errors.Is(err, controlplane.ErrInvalidGraph) {
		t.Fatalf("CompleteSend(forged acknowledged) error = %v, want ErrInvalidGraph", err)
	}
	afterForged, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if afterForged.Actions[action.ID].Completed || afterForged.Run.EventCursor != beforeForged.Run.EventCursor ||
		len(afterForged.Events) != len(beforeForged.Events) || len(afterForged.Messages) != 0 {
		t.Fatalf("forged acknowledgement mutated state: before=%#v after=%#v", beforeForged, afterForged)
	}

	if _, _, sent, err := service.CompleteSend(
		context.Background(), runID, attempt.ID, action.ID, result, message,
	); err != nil || sent.Acknowledged {
		t.Fatalf("CompleteSend(valid) = message %#v, error %v", sent, err)
	}
	if _, err := service.AcknowledgeMessage(context.Background(), runID, message.ID, message.ToTaskID); err != nil {
		t.Fatal(err)
	}
	beforeReplay, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	completed, _, replayed, err := restarted.CompleteSend(
		context.Background(), runID, attempt.ID, action.ID, result, message,
	)
	if err != nil {
		t.Fatalf("CompleteSend(exact replay after acknowledgement) error = %v", err)
	}
	if !completed.Completed || !replayed.Acknowledged {
		t.Fatalf("replayed send = action %#v, message %#v", completed, replayed)
	}
	afterReplay, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !afterReplay.Messages[message.ID].Acknowledged ||
		afterReplay.Run.EventCursor != beforeReplay.Run.EventCursor || len(afterReplay.Events) != len(beforeReplay.Events) {
		t.Fatalf("exact acknowledged replay changed durable effects: before=%#v after=%#v", beforeReplay, afterReplay)
	}

	changed := message
	changed.Digest = digest("message-replay-changed")
	beforeChanged, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := restarted.CompleteSend(
		context.Background(), runID, attempt.ID, action.ID, result, changed,
	); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("CompleteSend(changed immutable message) error = %v, want ErrActionConflict", err)
	}
	afterChanged, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if afterChanged.Run.EventCursor != beforeChanged.Run.EventCursor || len(afterChanged.Events) != len(beforeChanged.Events) ||
		afterChanged.Messages[message.ID] != beforeChanged.Messages[message.ID] {
		t.Fatalf("changed replay mutated state: before=%#v after=%#v", beforeChanged, afterChanged)
	}
}

func TestCompleteSendRejectsDifferentActionForAlreadyBoundMessage(t *testing.T) {
	store := controlmock.NewStore()
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	graph := validGraph()
	graph.Tasks[0].State = controlplane.TaskReady
	graph.Tasks[1].DependsOn = nil
	graph.Tasks[1].Communication = nil
	graph.Tasks[1].State = controlplane.TaskReady
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	firstAttempt, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	secondAttempt, err := service.CreateAttempt(context.Background(), run.ID, "task-b", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	activatePersistentAttempt(t, service, run.ID, firstAttempt.ID, "runtime-action-first", "cursor-1a", 1)
	activatePersistentAttempt(t, service, run.ID, secondAttempt.ID, "runtime-action-second", "cursor-1b", 1)

	message := controlplane.Message{
		ID: "message-bound-action", FromTaskID: controlplane.ParentAddress, ToTaskID: "task-a",
		Kind: controlplane.MessageSteering, Digest: digest("message-bound-action"),
	}
	requestDigest := testSendRequestDigest(message)
	firstAction, err := service.PrepareAction(
		context.Background(), run.ID, firstAttempt.ID, agentharness.ActionSend, requestDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondAction, err := service.PrepareAction(
		context.Background(), run.ID, secondAttempt.ID, agentharness.ActionSend, requestDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstAction.ID == secondAction.ID {
		t.Fatal("test requires distinct attempt-bound actions")
	}
	if _, _, _, err := service.CompleteSend(
		context.Background(), run.ID, firstAttempt.ID, firstAction.ID,
		agentharness.ActionResult{
			ActionID: firstAction.ID, RuntimeWorkIDs: []string{"runtime-action-first"},
			ResultDigest: digest("message-bound-action-first-result"), MessageReceipt: "message-bound-action-first-receipt",
		},
		message,
	); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.CompleteSend(
		context.Background(), run.ID, secondAttempt.ID, secondAction.ID,
		agentharness.ActionResult{
			ActionID: secondAction.ID, RuntimeWorkIDs: []string{"runtime-action-second"},
			ResultDigest: digest("message-bound-action-second-result"), MessageReceipt: "message-bound-action-second-receipt",
		},
		message,
	); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("CompleteSend(changed action) error = %v, want ErrActionConflict", err)
	}
	after, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Actions[secondAction.ID].Completed || after.Run.EventCursor != before.Run.EventCursor ||
		len(after.Events) != len(before.Events) || after.Messages[message.ID] != before.Messages[message.ID] {
		t.Fatalf("changed action mutated state: before=%#v after=%#v", before, after)
	}
}

func TestPrepareObserveAtomicallyBindsCurrentCursorAndExactReplay(t *testing.T) {
	store, service, runID, attempt := persistentService(t)
	activatePersistentAttempt(t, service, runID, attempt.ID, "runtime-observe-atomic", "cursor-1", 1)

	digestAtOne := testObserveRequestDigest(runID, attempt.ID, "cursor-1", 1)
	prepared, reused, err := service.PrepareObserve(
		context.Background(), runID, attempt.ID, digestAtOne, "cursor-1", 1,
	)
	if err != nil || reused {
		t.Fatalf("PrepareObserve(first) = action %#v, reused %v, error %v", prepared, reused, err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, prepared.ID, agentharness.ActionResult{
		ActionID: prepared.ID, RuntimeWorkIDs: []string{"runtime-observe-atomic"},
		Cursor: "cursor-2", CursorSequence: 2, ResultDigest: digest("observe-atomic-result"),
	}); err != nil {
		t.Fatal(err)
	}
	beforeReplay, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	replayed, reused, err := restarted.PrepareObserve(
		context.Background(), runID, attempt.ID, digestAtOne, "cursor-1", 1,
	)
	if err != nil || !reused || replayed.ID != prepared.ID || !replayed.Completed {
		t.Fatalf("PrepareObserve(exact restart replay) = action %#v, reused %v, error %v", replayed, reused, err)
	}
	afterReplay, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if afterReplay.Run.EventCursor != beforeReplay.Run.EventCursor || len(afterReplay.Events) != len(beforeReplay.Events) {
		t.Fatalf("exact observe replay duplicated effects: before=%#v after=%#v", beforeReplay, afterReplay)
	}

	changedTupleDigest := testObserveRequestDigest(runID, attempt.ID, "cursor-1", 2)
	if _, _, err := restarted.PrepareObserve(
		context.Background(), runID, attempt.ID, changedTupleDigest, "cursor-1", 2,
	); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("PrepareObserve(changed tuple) error = %v, want ErrActionConflict", err)
	}
	if _, _, err := restarted.PrepareObserve(
		context.Background(), runID, attempt.ID, digest("wrong-observe-digest"), "cursor-2", 2,
	); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("PrepareObserve(wrong digest) error = %v, want ErrActionConflict", err)
	}
	if _, _, err := restarted.PrepareObserve(
		context.Background(), runID, "wrong-attempt", digestAtOne, "cursor-1", 1,
	); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("PrepareObserve(wrong attempt) error = %v, want ErrActionConflict", err)
	}

	nextDigest := testObserveRequestDigest(runID, attempt.ID, "cursor-2", 2)
	next, reused, err := restarted.PrepareObserve(
		context.Background(), runID, attempt.ID, nextDigest, "cursor-2", 2,
	)
	if err != nil || reused || next.ID == prepared.ID {
		t.Fatalf("PrepareObserve(next cursor) = action %#v, reused %v, error %v", next, reused, err)
	}
}

func TestPrepareObserveLosesCASRaceToCursorAdvanceWithoutPersistingStaleAction(t *testing.T) {
	store, service, runID, attempt := persistentService(t)
	activatePersistentAttempt(t, service, runID, attempt.ID, "runtime-observe-race", "cursor-1", 1)
	wait, err := service.PrepareAction(
		context.Background(), runID, attempt.ID, agentharness.ActionWait, digest("wait-advance-observe-race"),
	)
	if err != nil {
		t.Fatal(err)
	}

	ordered := newOrderedSaveStore(store)
	tracing := controlplane.NewService(ordered, controlplane.WithClock(fixedClock))
	prepareContext := context.WithValue(context.Background(), storeOperationKey{}, "prepare")
	prepareResult := make(chan error, 1)
	staleDigest := testObserveRequestDigest(runID, attempt.ID, "cursor-1", 1)
	go func() {
		_, _, err := tracing.PrepareObserve(
			prepareContext, runID, attempt.ID, staleDigest, "cursor-1", 1,
		)
		prepareResult <- err
	}()
	<-ordered.prepareSaving

	completeContext := context.WithValue(context.Background(), storeOperationKey{}, "complete")
	if _, err := tracing.CompleteAction(completeContext, runID, wait.ID, agentharness.ActionResult{
		ActionID: wait.ID, RuntimeWorkIDs: []string{"runtime-observe-race"},
		Cursor: "cursor-2", CursorSequence: 2, ResultDigest: digest("wait-advance-observe-race-result"),
	}); err != nil {
		t.Fatal(err)
	}
	close(ordered.releasePrepare)
	if err := <-prepareResult; !errors.Is(err, controlplane.ErrVersionConflict) {
		t.Fatalf("PrepareObserve(stale concurrent tuple) error = %v, want ErrVersionConflict", err)
	}

	snapshot, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Attempts[attempt.ID].LastCursor != "cursor-2" || snapshot.Attempts[attempt.ID].CursorSequence != 2 {
		t.Fatalf("attempt cursor = %#v", snapshot.Attempts[attempt.ID])
	}
	for _, action := range snapshot.Actions {
		if action.Kind == agentharness.ActionObserve && action.RequestDigest == staleDigest {
			t.Fatalf("stale observe action persisted: %#v", action)
		}
	}
	beforeStaleRetry := snapshot
	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	if _, _, err := restarted.PrepareObserve(
		context.Background(), runID, attempt.ID, staleDigest, "cursor-1", 1,
	); !errors.Is(err, controlplane.ErrActionConflict) {
		t.Fatalf("PrepareObserve(stale after restart) error = %v, want ErrActionConflict", err)
	}
	afterStaleRetry, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if afterStaleRetry.Run.EventCursor != beforeStaleRetry.Run.EventCursor ||
		len(afterStaleRetry.Events) != len(beforeStaleRetry.Events) || len(afterStaleRetry.Actions) != len(beforeStaleRetry.Actions) {
		t.Fatalf("stale restart retry mutated state: before=%#v after=%#v", beforeStaleRetry, afterStaleRetry)
	}
}

func TestStrictModelRejectsUnknownFieldsAndInvalidGraphs(t *testing.T) {
	t.Parallel()

	graph := validGraph()
	encoded, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded[:len(encoded)-1], []byte(`,"provider_object":{}}`)...)
	if _, err := controlplane.DecodeTaskGraph(encoded); !errors.Is(err, controlplane.ErrInvalidRecord) {
		t.Fatalf("DecodeTaskGraph(unknown field) error = %v, want ErrInvalidRecord", err)
	}

	tests := []struct {
		name   string
		mutate func(*controlplane.TaskGraph)
	}{
		{"duplicate task", func(g *controlplane.TaskGraph) { g.Tasks = append(g.Tasks, g.Tasks[0]) }},
		{"missing predecessor", func(g *controlplane.TaskGraph) { g.Tasks[1].DependsOn = []string{"missing"} }},
		{"cycle", func(g *controlplane.TaskGraph) { g.Tasks[0].DependsOn = []string{"task-b"} }},
		{"missing immutable base SHA", func(g *controlplane.TaskGraph) { g.Tasks[0].Projects[0].BaseSHA = "" }},
		{"ambiguous integration order", func(g *controlplane.TaskGraph) { g.IntegrationOrder = []string{"task-a", "task-a"} }},
		{"undeclared communication edge", func(g *controlplane.TaskGraph) {
			g.Tasks[0].Communication = []controlplane.CommunicationEdge{{ProjectID: "project-b", TaskID: "task-b"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := validGraph()
			test.mutate(&candidate)
			if err := controlplane.ValidateGraph(candidate, nil); !errors.Is(err, controlplane.ErrInvalidGraph) {
				t.Fatalf("ValidateGraph() error = %v, want ErrInvalidGraph", err)
			}
		})
	}
}

func TestGraphRejectsProjectIDReboundAcrossTasks(t *testing.T) {
	t.Parallel()

	graph := validGraph()
	graph.Tasks[1].Projects[0].ID = graph.Tasks[0].Projects[0].ID
	if err := controlplane.ValidateGraph(graph, nil); !errors.Is(err, controlplane.ErrInvalidGraph) {
		t.Fatalf("ValidateGraph(rebound project ID) error = %v, want ErrInvalidGraph", err)
	}
}

func TestGraphRejectsProjectIDReboundAcrossRevisions(t *testing.T) {
	t.Parallel()

	previous := validGraph()
	previous.Tasks[0].State = controlplane.TaskCompleted
	next := controlplane.CloneGraph(previous)
	next.Revision++
	next.Tasks[0].Projects[0].Repository = "https://example.test/rebound.git"
	next.Tasks[0].Projects[0].BaseSHA = digest("rebound-base")
	if err := controlplane.ValidateGraph(next, &previous); !errors.Is(err, controlplane.ErrImmutableBoundary) {
		t.Fatalf("ValidateGraph(rebound completed project) error = %v, want ErrImmutableBoundary", err)
	}

	next = controlplane.CloneGraph(previous)
	next.Revision++
	next.Tasks = next.Tasks[1:]
	next.Tasks[0].DependsOn = nil
	next.Tasks[0].Communication = nil
	next.IntegrationOrder = []string{"task-b"}
	if err := controlplane.ValidateGraph(next, &previous); !errors.Is(err, controlplane.ErrImmutableBoundary) {
		t.Fatalf("ValidateGraph(removed project identity) error = %v, want ErrImmutableBoundary", err)
	}
}

func TestCreateAttemptProvesReadinessCapacityAndActiveOwnership(t *testing.T) {
	t.Run("forged ready state", func(t *testing.T) {
		store := controlmock.NewStore()
		service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
		graph := validGraph()
		graph.Tasks[0].State = controlplane.TaskActive
		graph.Tasks[1].State = controlplane.TaskReady
		run := validRun(graph)
		if _, err := service.Create(context.Background(), run, graph); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateAttempt(context.Background(), run.ID, "task-b", completeCapabilities()); !errors.Is(err, controlplane.ErrInvalidPlacement) {
			t.Fatalf("CreateAttempt(forged ready) error = %v, want ErrInvalidPlacement", err)
		}
	})

	t.Run("completed predecessor with active attempt", func(t *testing.T) {
		store := controlmock.NewStore()
		graph := validGraph()
		graph.Tasks[0].State = controlplane.TaskCompleted
		graph.Tasks[1].State = controlplane.TaskReady
		snapshot, err := controlplane.NewSnapshot(validRun(graph), graph)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Attempts["active-predecessor"] = controlplane.PlacementAttempt{
			ID: "active-predecessor", TaskID: "task-a", Primitive: agentharness.PersistentSession,
			CapabilitySnapshot: completeCapabilities(), LifecycleOwner: "parent", State: controlplane.AttemptActive,
			RuntimeWorkIDs: []string{"still-running"}, ActionIDs: []string{}, ObservedEvents: map[string]string{},
		}
		if _, err := store.Create(context.Background(), snapshot); err != nil {
			t.Fatal(err)
		}
		service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
		if _, err := service.CreateAttempt(context.Background(), snapshot.Run.ID, "task-b", completeCapabilities()); !errors.Is(err, controlplane.ErrClosePrecondition) {
			t.Fatalf("CreateAttempt(active predecessor attempt) error = %v, want ErrClosePrecondition", err)
		}
	})

	t.Run("completed predecessor with closed attempt", func(t *testing.T) {
		store := controlmock.NewStore()
		graph := validGraph()
		graph.Tasks[0].State = controlplane.TaskCompleted
		graph.Tasks[1].State = controlplane.TaskReady
		snapshot, err := controlplane.NewSnapshot(validRun(graph), graph)
		if err != nil {
			t.Fatal(err)
		}
		evidence := completedEvidence("evidence-predecessor", "attempt-predecessor")
		attempt := completedAttempt(
			"attempt-predecessor", agentharness.LocalSequential,
			controlplane.WorkCloseEvidence{Kind: controlplane.CloseInactive, Receipt: "inactive", Digest: digest("inactive")},
		)
		attempt.TerminalEvidence = controlplane.EvidenceRef{ID: evidence.ID, Digest: controlplane.EvidenceDigest(evidence)}
		attempt.Disposition = controlplane.Disposition{Kind: controlplane.DispositionIntegrated, EvidenceID: evidence.ID}
		snapshot.Attempts[attempt.ID] = attempt
		snapshot.Evidence[evidence.ID] = evidence
		if _, err := store.Create(context.Background(), snapshot); err != nil {
			t.Fatal(err)
		}
		service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
		if _, err := service.CreateAttempt(context.Background(), snapshot.Run.ID, "task-b", completeCapabilities()); err != nil {
			t.Fatalf("CreateAttempt(closed predecessor attempt) error = %v", err)
		}
	})

	t.Run("harness capacity", func(t *testing.T) {
		store := controlmock.NewStore()
		service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
		graph := validGraph()
		graph.Tasks[0].State = controlplane.TaskReady
		graph.Tasks[1].State = controlplane.TaskPending
		graph.Tasks[1].DependsOn = nil
		graph.Tasks[1].Communication = nil
		run := validRun(graph)
		if _, err := service.Create(context.Background(), run, graph); err != nil {
			t.Fatal(err)
		}
		capabilities := completeCapabilities()
		persistent := capabilities.Primitives[agentharness.PersistentSession]
		persistent.ConcurrencyLimit = 1
		capabilities.Primitives[agentharness.PersistentSession] = persistent
		if _, err := service.CreateAttempt(context.Background(), run.ID, "task-a", capabilities); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateAttempt(context.Background(), run.ID, "task-b", capabilities); !errors.Is(err, controlplane.ErrConcurrencyExhausted) {
			t.Fatalf("CreateAttempt(exhausted) error = %v, want ErrConcurrencyExhausted", err)
		}
	})

	t.Run("active ownership", func(t *testing.T) {
		store := controlmock.NewStore()
		service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
		graph := validGraph()
		graph.Tasks[0].State = controlplane.TaskReady
		graph.Tasks[1].State = controlplane.TaskPending
		graph.Tasks[1].DependsOn = nil
		graph.Tasks[1].Communication = nil
		graph.Tasks[1].Projects = []controlplane.ProjectRef{graph.Tasks[0].Projects[0]}
		graph.Tasks[1].Ownership.Mutable = []string{"internal/a/file.go"}
		run := validRun(graph)
		if _, err := service.Create(context.Background(), run, graph); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities()); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateAttempt(context.Background(), run.ID, "task-b", completeCapabilities()); !errors.Is(err, controlplane.ErrOwnershipConflict) {
			t.Fatalf("CreateAttempt(overlap) error = %v, want ErrOwnershipConflict", err)
		}
	})

	t.Run("unrelated project ownership", func(t *testing.T) {
		store := controlmock.NewStore()
		service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
		graph := validGraph()
		graph.Tasks[0].State = controlplane.TaskReady
		graph.Tasks[1].State = controlplane.TaskPending
		graph.Tasks[1].DependsOn = nil
		graph.Tasks[1].Communication = nil
		graph.Tasks[1].Ownership.Mutable = append([]string(nil), graph.Tasks[0].Ownership.Mutable...)
		run := validRun(graph)
		if _, err := service.Create(context.Background(), run, graph); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities()); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateAttempt(context.Background(), run.ID, "task-b", completeCapabilities()); err != nil {
			t.Fatalf("CreateAttempt(unrelated project same path) error = %v", err)
		}
	})
}

func TestOwnershipRejectsOverlappingActiveWritersAndFrozenRevisionChanges(t *testing.T) {
	t.Parallel()

	graph := validGraph()
	graph.Tasks[1].DependsOn = nil
	graph.Tasks[1].Communication = nil
	graph.Tasks[1].Projects = []controlplane.ProjectRef{graph.Tasks[0].Projects[0]}
	graph.Tasks[1].Ownership.Mutable = []string{"internal/a/service.go"}
	graph.Tasks[0].State = controlplane.TaskActive
	graph.Tasks[1].State = controlplane.TaskReady
	if err := controlplane.ValidateGraph(graph, nil); !errors.Is(err, controlplane.ErrOwnershipConflict) {
		t.Fatalf("ValidateGraph(overlap) error = %v, want ErrOwnershipConflict", err)
	}

	previous := validGraph()
	previous.Tasks[0].State = controlplane.TaskActive
	next := controlplane.CloneGraph(previous)
	next.Revision++
	next.Tasks[0].FrozenInputs[0].Digest = digest("changed")
	if err := controlplane.ValidateGraph(next, &previous); !errors.Is(err, controlplane.ErrImmutableBoundary) {
		t.Fatalf("ValidateGraph(active frozen change) error = %v, want ErrImmutableBoundary", err)
	}
	next = controlplane.CloneGraph(previous)
	next.Revision++
	next.Tasks[0].Ownership.Mutable = []string{"different/**"}
	if err := controlplane.ValidateGraph(next, &previous); !errors.Is(err, controlplane.ErrImmutableBoundary) {
		t.Fatalf("ValidateGraph(active ownership change) error = %v, want ErrImmutableBoundary", err)
	}
}

func TestMultiProjectReadyTasksRemainIsolatedAndMayRunConcurrently(t *testing.T) {
	t.Parallel()

	graph := validGraph()
	graph.Tasks[1].DependsOn = nil
	graph.Tasks[1].Communication = nil
	graph.Tasks[0].State = controlplane.TaskReady
	graph.Tasks[1].State = controlplane.TaskReady
	if err := controlplane.ValidateGraph(graph, nil); err != nil {
		t.Fatalf("ValidateGraph() error = %v", err)
	}
	ready, err := controlplane.ReadyTasks(graph)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 2 {
		t.Fatalf("ReadyTasks() = %d tasks, want 2", len(ready))
	}
	a, b := ready[0].Projects[0], ready[1].Projects[0]
	if a.Repository == b.Repository || a.WorkspaceScope == b.WorkspaceScope ||
		a.CredentialScope == b.CredentialScope || a.MailboxNamespace == b.MailboxNamespace ||
		a.EvidenceNamespace == b.EvidenceNamespace {
		t.Fatalf("unrelated projects are not isolated: %#v %#v", a, b)
	}
}

func validGraph() controlplane.TaskGraph {
	persistent := controlplane.ExecutionPlacement{
		ParallelismPrimitive:   agentharness.PersistentSession,
		ExecutionPlacement:     "worktree_backed_codex_task",
		PlacementRationale:     "long restartable isolated mutation",
		CapabilityRequirements: append([]agentharness.Capability(nil), agentharness.RequiredCapabilities(agentharness.PersistentSession)...),
		LifecycleOwner:         "parent",
		Fallback:               "block",
	}
	return controlplane.TaskGraph{
		SchemaVersion:    controlplane.SchemaVersion,
		ControlRunID:     "control-1",
		Revision:         1,
		IntegrationOrder: []string{"task-a", "task-b"},
		CombinedGates:    []controlplane.Gate{{ID: "combined", Digest: digest("combined")}},
		Tasks: []controlplane.Task{
			{
				ID: "task-a", Goal: "Implement A", Projects: []controlplane.ProjectRef{{
					ID: "project-a", Repository: "https://example.test/a.git", BaseRef: "main",
					BaseSHA: digest("base-a"), WorkspaceScope: "workspace-a",
					CredentialScope: "credential-a", MailboxNamespace: "mail-a",
					EvidenceNamespace: "evidence-a",
				}},
				Ownership: controlplane.Ownership{Mutable: []string{"internal/a/**"}},
				Placement: persistent, FrozenInputs: []controlplane.FrozenInput{{ID: "spec", Digest: digest("spec-a")}},
				Acceptance: []controlplane.Gate{{ID: "test-a", Digest: digest("test-a")}}, State: controlplane.TaskPending,
			},
			{
				ID: "task-b", Goal: "Implement B", DependsOn: []string{"task-a"},
				Projects: []controlplane.ProjectRef{{
					ID: "project-b", Repository: "https://example.test/b.git", BaseRef: "trunk",
					BaseSHA: digest("base-b"), WorkspaceScope: "workspace-b",
					CredentialScope: "credential-b", MailboxNamespace: "mail-b",
					EvidenceNamespace: "evidence-b",
				}},
				Ownership: controlplane.Ownership{Mutable: []string{"internal/b/**"}},
				Placement: persistent, FrozenInputs: []controlplane.FrozenInput{{ID: "spec", Digest: digest("spec-b")}},
				Acceptance: []controlplane.Gate{{ID: "test-b", Digest: digest("test-b")}}, State: controlplane.TaskPending,
				Communication: []controlplane.CommunicationEdge{{ProjectID: "project-a", TaskID: "task-a"}},
			},
		},
	}
}

func digest(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}

func activatePersistentAttempt(
	t *testing.T,
	service *controlplane.Service,
	runID, attemptID, runtimeID, cursor string,
	sequence uint64,
) {
	t.Helper()
	action, err := service.PrepareAction(
		context.Background(), runID, attemptID, agentharness.ActionDispatch, digest("dispatch-"+runtimeID),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), runID, action.ID, agentharness.ActionResult{
		ActionID: action.ID, RuntimeWorkIDs: []string{runtimeID}, Cursor: cursor, CursorSequence: sequence,
		ResultDigest: digest("dispatch-result-" + runtimeID),
	}); err != nil {
		t.Fatal(err)
	}
}

func testSendRequestDigest(message controlplane.Message) string {
	immutable := struct {
		ID         string                   `json:"id"`
		FromTaskID string                   `json:"from_task_id"`
		ToTaskID   string                   `json:"to_task_id"`
		Kind       controlplane.MessageKind `json:"kind"`
		Digest     string                   `json:"digest"`
	}{
		ID: message.ID, FromTaskID: message.FromTaskID, ToTaskID: message.ToTaskID,
		Kind: message.Kind, Digest: message.Digest,
	}
	raw, _ := json.Marshal(immutable)
	sum := sha256.Sum256(append([]byte("paje-control-send-v1\x00"), raw...))
	return fmt.Sprintf("sha256:%x", sum)
}

func testObserveRequestDigest(
	runID, attemptID, afterCursor string,
	afterSequence uint64,
) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"paje-control-observe-v1", runID, attemptID, afterCursor,
		strconv.FormatUint(afterSequence, 10),
	}, "\x00")))
	return fmt.Sprintf("sha256:%x", sum)
}

type loadBarrierStore struct {
	controlplane.Store
	mu       sync.Mutex
	want     int
	arrived  int
	release  chan struct{}
	released bool
}

func newLoadBarrierStore(store controlplane.Store, want int) *loadBarrierStore {
	return &loadBarrierStore{Store: store, want: want, release: make(chan struct{})}
}

func (s *loadBarrierStore) Load(ctx context.Context, id string) (controlplane.Snapshot, error) {
	snapshot, err := s.Store.Load(ctx, id)
	if err != nil {
		return controlplane.Snapshot{}, err
	}
	s.mu.Lock()
	s.arrived++
	if s.arrived == s.want && !s.released {
		close(s.release)
		s.released = true
	}
	release := s.release
	s.mu.Unlock()
	select {
	case <-release:
		return snapshot, nil
	case <-ctx.Done():
		return controlplane.Snapshot{}, ctx.Err()
	}
}

type storeOperationKey struct{}

type orderedSaveStore struct {
	controlplane.Store
	prepareSaving  chan struct{}
	releasePrepare chan struct{}
	once           sync.Once
}

func newOrderedSaveStore(store controlplane.Store) *orderedSaveStore {
	return &orderedSaveStore{
		Store: store, prepareSaving: make(chan struct{}), releasePrepare: make(chan struct{}),
	}
}

func (s *orderedSaveStore) Save(
	ctx context.Context,
	next controlplane.Snapshot,
	expectedVersion uint64,
) (controlplane.Snapshot, error) {
	if ctx.Value(storeOperationKey{}) == "prepare" {
		s.once.Do(func() { close(s.prepareSaving) })
		select {
		case <-s.releasePrepare:
		case <-ctx.Done():
			return controlplane.Snapshot{}, ctx.Err()
		}
	}
	return s.Store.Save(ctx, next, expectedVersion)
}
