package isolation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
)

func TestInboxBindsAuthoritativePositionsAndAcknowledgement(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-inbox")
	callback := callbackRequest(t, scope, "callback-1", 0, "event-1", "payload-1")
	result, err := service.AppendCallback(context.Background(), callback)
	if err != nil {
		t.Fatal(err)
	}
	var inbox RunInbox = result.Projection.Inbox
	if len(inbox) != 1 {
		t.Fatalf("RunInbox length = %d, want 1", len(inbox))
	}
	item, ok := result.Projection.InboxItem("event-1")
	if !ok {
		t.Fatal("callback is absent from inbox")
	}
	if item.JournalPosition != result.Receipt.Outcome.JournalPosition ||
		item.RunSequence != result.Receipt.Outcome.RunSequence ||
		item.CorrelationID != callback.CorrelationID ||
		item.TaskID != callback.TaskID || item.AttemptID != callback.AttemptID ||
		item.ExternalActionID != callback.ExternalActionID ||
		item.ActionGeneration != callback.ActionGeneration ||
		item.Producer != callback.Producer || item.Consumer != callback.Consumer ||
		item.PayloadDigest != callback.PayloadDigest {
		t.Fatalf("inbox item = %#v, receipt = %#v, request = %#v", item, result.Receipt, callback)
	}

	ack, err := service.AcknowledgeInbox(context.Background(), AcknowledgeRequest{
		Scope: scope, OperationID: "ack-1", GraphRevision: 1,
		ExpectedRevision: result.Projection.Revision,
		EventID:          item.EventID,
		Consumer:         item.Consumer,
		ReceiptID:        "ack-receipt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	acknowledged, ok := ack.Projection.InboxItem(item.EventID)
	if !ok || acknowledged.AcknowledgementReceipt != "ack-receipt-1" {
		t.Fatalf("acknowledged item = %#v, %v", acknowledged, ok)
	}
	if acknowledged.JournalPosition != item.JournalPosition || acknowledged.RunSequence != item.RunSequence {
		t.Fatal("acknowledgement rewrote the inbox item's authoritative position")
	}
}

func TestCallbackExactReplayIsIdempotentAndChangedDuplicateConflicts(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-callback-replay")
	request := callbackRequest(t, scope, "callback-1", 0, "event-1", "payload-1")
	first, err := service.AppendCallback(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.AppendCallback(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Receipt.Created {
		t.Fatal("exact callback replay created a second commit")
	}
	if second.Receipt.Outcome != first.Receipt.Outcome || len(second.Projection.Inbox) != 1 {
		t.Fatalf("callback replay = %#v, want original outcome and one inbox item", second)
	}

	changed := request
	changed.PayloadDigest = mustDigest(t, "changed-payload")
	if _, err := service.AppendCallback(context.Background(), changed); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("changed duplicate error = %v, want journal conflict", err)
	}
}

func TestCallbackMissingAndTerminalVisibleSessionDoNotSynthesizeAuthority(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-terminal-visible")
	before := feedLength(t, store)
	projection, err := service.Observe(context.Background(), ObservationRequest{
		Scope: scope, Source: ObservationTerminal, Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Inbox) != 0 || projection.Revision != 0 {
		t.Fatalf("terminal-visible session synthesized authority: %#v", projection)
	}
	if after := feedLength(t, store); after != before {
		t.Fatalf("terminal-visible observation appended %d journal events", after-before)
	}
}

func TestCallbackPermutationsRemainMonotonicWithoutDuplicateIdentity(t *testing.T) {
	t.Parallel()

	for _, permutation := range permutations([]string{"event-1", "event-2", "event-3"}) {
		permutation := append([]string(nil), permutation...)
		t.Run(fmt.Sprint(permutation), func(t *testing.T) {
			t.Parallel()
			service, _ := newTestServiceWithInstallation(t, "installation-"+permutation[0])
			scope := mustRunScope(t, "installation-"+permutation[0], "run")
			revision := uint64(0)
			for _, eventID := range permutation {
				request := callbackRequest(t, scope, "callback-"+eventID, revision, eventID, "payload-"+eventID)
				result, err := service.AppendCallback(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				revision = result.Projection.Revision
			}
			projection, err := service.Current(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if len(projection.Inbox) != len(permutation) {
				t.Fatalf("inbox length = %d, want %d", len(projection.Inbox), len(permutation))
			}
			for index, item := range projection.Inbox {
				if item.EventID != permutation[index] {
					t.Fatalf("inbox[%d].event = %q, want arrival %q", index, item.EventID, permutation[index])
				}
				if index > 0 && (item.JournalPosition <= projection.Inbox[index-1].JournalPosition ||
					item.RunSequence <= projection.Inbox[index-1].RunSequence) {
					t.Fatalf("inbox is not monotonic: %#v", projection.Inbox)
				}
			}
		})
	}
}

func TestGateKindsRequireExactResolverAndWakeIdentity(t *testing.T) {
	t.Parallel()

	for _, kind := range []GateKind{
		GateTimeNotBefore,
		GateExternalStatus,
		GateWorkflowTerminal,
		GateEvidenceRequired,
		GateNoOverlapWindow,
		GateHumanApproval,
		GateSecurityContainment,
	} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			clock := newMutableClock(time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC))
			service, _ := newTestServiceWithClock(t, "installation-"+string(kind), clock.Now)
			scope := mustRunScope(t, "installation-"+string(kind), "run")
			revision := deferTask(t, service, scope)
			gate := PendingWorkGate{
				ID: "gate", TaskID: "task", Kind: kind, ResolverAuthority: "resolver",
			}
			if kind == GateTimeNotBefore {
				gate.WakeAt = clock.Now().Add(time.Minute)
			} else {
				gate.WakeEventID = "wake-event"
				callback := callbackRequest(t, scope, "wake-callback", revision, "wake-event", "wake")
				result, err := service.AppendCallback(context.Background(), callback)
				if err != nil {
					t.Fatal(err)
				}
				revision = result.Projection.Revision
			}
			registered, err := service.RegisterGate(context.Background(), RegisterGateRequest{
				Scope: scope, OperationID: "register-gate", GraphRevision: 1,
				ExpectedRevision: revision, Gate: gate,
			})
			if err != nil {
				t.Fatal(err)
			}
			wake := WakeGateRequest{
				Scope: scope, OperationID: "wake-gate", GraphRevision: 1,
				ExpectedRevision: registered.Projection.Revision,
				GateID:           gate.ID, ResolverAuthority: "wrong-resolver",
				WakeEventID: gate.WakeEventID, WakeTime: gate.WakeAt,
			}
			if _, err := service.WakeGate(context.Background(), wake); !errors.Is(err, ErrResolverAuthority) {
				t.Fatalf("wrong resolver error = %v, want resolver authority", err)
			}
		})
	}
}

func TestGateEventWakeRequiresTheCommittedInboxFact(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-event-wake")
	revision := deferTask(t, service, scope)
	registered, err := service.RegisterGate(context.Background(), RegisterGateRequest{
		Scope: scope, OperationID: "register", GraphRevision: 1,
		ExpectedRevision: revision,
		Gate: PendingWorkGate{
			ID: "gate", TaskID: "task", Kind: GateWorkflowTerminal,
			ResolverAuthority: "workflow-observer", WakeEventID: "terminal-event",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.WakeGate(context.Background(), WakeGateRequest{
		Scope: scope, OperationID: "wake", GraphRevision: 1,
		ExpectedRevision: registered.Projection.Revision,
		GateID:           "gate", ResolverAuthority: "workflow-observer", WakeEventID: "terminal-event",
	})
	if !errors.Is(err, ErrWakeFactMissing) {
		t.Fatalf("wake without inbox fact error = %v, want wake fact missing", err)
	}
}

func TestQuiescentTimeGateHasZeroHotPollAndWakesExactlyOnce(t *testing.T) {
	t.Parallel()

	clock := newMutableClock(time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC))
	service, store := newTestServiceWithClock(t, "installation-1", clock.Now)
	scope := mustRunScope(t, "installation-1", "run-quiescent")
	revision := deferTask(t, service, scope)
	wakeAt := clock.Now().Add(5 * time.Minute)
	registered, err := service.RegisterGate(context.Background(), RegisterGateRequest{
		Scope: scope, OperationID: "register-time-gate", GraphRevision: 1,
		ExpectedRevision: revision,
		Gate: PendingWorkGate{
			ID: "time-gate", TaskID: "task", Kind: GateTimeNotBefore,
			ResolverAuthority: "clock", WakeAt: wakeAt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Projection.RunPhase != RunQuiescent {
		t.Fatalf("run phase = %s, want %s", registered.Projection.RunPhase, RunQuiescent)
	}

	before := feedLength(t, store)
	for range 10 {
		current, err := service.Current(context.Background(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if current.RunPhase != RunQuiescent {
			t.Fatalf("current run phase = %s, want quiescent", current.RunPhase)
		}
	}
	if after := feedLength(t, store); after != before {
		t.Fatalf("quiescent reads hot-polled into %d journal events", after-before)
	}

	wake := WakeGateRequest{
		Scope: scope, OperationID: "wake-time-gate", GraphRevision: 1,
		ExpectedRevision: registered.Projection.Revision,
		GateID:           "time-gate", ResolverAuthority: "clock", WakeTime: wakeAt,
	}
	if _, err := service.WakeGate(context.Background(), wake); !errors.Is(err, ErrWakeNotReady) {
		t.Fatalf("early wake error = %v, want wake not ready", err)
	}
	clock.Set(wakeAt)
	woken, err := service.WakeGate(context.Background(), wake)
	if err != nil {
		t.Fatal(err)
	}
	if woken.Projection.RunPhase != RunActive {
		t.Fatalf("woken run phase = %s, want active", woken.Projection.RunPhase)
	}
	task, ok := woken.Projection.Task("task")
	if !ok || task.Phase != TaskReadyForOwnership {
		t.Fatalf("woken task = %#v, %v", task, ok)
	}
	gate, ok := woken.Projection.Gate("time-gate")
	if !ok || gate.State != GateResolved || gate.WakeReceipt == "" {
		t.Fatalf("woken gate = %#v, %v", gate, ok)
	}

	replayed, err := service.WakeGate(context.Background(), wake)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Receipt.Created || replayed.Receipt.Outcome != woken.Receipt.Outcome {
		t.Fatalf("wake replay = %#v, want immutable original receipt", replayed.Receipt)
	}
	secondWake := wake
	secondWake.OperationID = "wake-time-gate-again"
	secondWake.ExpectedRevision = woken.Projection.Revision
	if _, err := service.WakeGate(context.Background(), secondWake); !errors.Is(err, ErrGateResolved) {
		t.Fatalf("second wake error = %v, want gate resolved", err)
	}
}

func TestRebuildUsesJournalOnlyAndPreservesResolvedGate(t *testing.T) {
	t.Parallel()

	clock := newMutableClock(time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC))
	service, store := newTestServiceWithClock(t, "installation-1", clock.Now)
	scope := mustRunScope(t, "installation-1", "run-rebuild")
	revision := deferTask(t, service, scope)
	wakeAt := clock.Now().Add(time.Minute)
	registered, err := service.RegisterGate(context.Background(), RegisterGateRequest{
		Scope: scope, OperationID: "register", GraphRevision: 1,
		ExpectedRevision: revision,
		Gate: PendingWorkGate{
			ID: "gate", TaskID: "task", Kind: GateTimeNotBefore,
			ResolverAuthority: "clock", WakeAt: wakeAt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Set(wakeAt)
	woken, err := service.WakeGate(context.Background(), WakeGateRequest{
		Scope: scope, OperationID: "wake", GraphRevision: 1,
		ExpectedRevision: registered.Projection.Revision,
		GateID:           "gate", ResolverAuthority: "clock", WakeTime: wakeAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	restarted, err := New(store, "installation-1", clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := restarted.Current(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Revision != woken.Projection.Revision || rebuilt.RunPhase != RunActive ||
		!slices.Equal(rebuilt.Tasks, woken.Projection.Tasks) ||
		!slices.Equal(rebuilt.Gates, woken.Projection.Gates) ||
		!slices.Equal(rebuilt.Inbox, woken.Projection.Inbox) {
		t.Fatalf("journal-only rebuild = %#v, want %#v", rebuilt, woken.Projection)
	}
}

func TestRebuildRejectsForeignScope(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	foreign := mustRunScope(t, "another-installation", "run")
	if _, err := service.Current(context.Background(), foreign); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("Current(foreign) error = %v, want invalid scope", err)
	}
}

func TestRebuildIgnoresUnrelatedJournalOutcomeWithoutPayload(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-unrelated-event")
	action := journal.Action{
		ID:                     "another-package-action",
		ControlRunID:           scope.ControlRunID,
		Kind:                   journal.KindDispatch,
		GraphRevision:          1,
		CanonicalRequestDigest: mustDigest(t, map[string]string{"request": "other"}),
		IdempotencyKey:         "another-package-key",
	}
	if _, _, err := store.Reserve(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), scope.ControlRunID, 1, journal.Event{
		ID:            "another-package-outcome",
		ControlRunID:  scope.ControlRunID,
		ActionID:      action.ID,
		Kind:          journal.EventActionResult,
		PayloadDigest: mustDigest(t, map[string]string{"outcome": "not-in-payload-store"}),
		OccurredAt:    time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	projection, err := service.Current(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Revision != 0 || len(projection.Inbox) != 0 || len(projection.Gates) != 0 {
		t.Fatalf("unrelated outcome mutated isolation projection: %#v", projection)
	}
}

func TestRebuildRejectsForeignPayloadInsideReservedIsolationNamespace(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-reserved-namespace")
	requestValue := map[string]any{
		"schema_version":     1,
		"semantic_operation": "other.operation",
		"scope": map[string]string{
			"installation_id": scope.InstallationID,
			"control_run_id":  scope.ControlRunID,
		},
	}
	requestPayload, err := journal.CanonicalJSON(requestValue)
	if err != nil {
		t.Fatal(err)
	}
	outcomeValue := map[string]any{
		"schema_version":     1,
		"semantic_operation": "other.operation",
		"scope": map[string]string{
			"installation_id": scope.InstallationID,
			"control_run_id":  scope.ControlRunID,
		},
	}
	outcomePayload, err := journal.CanonicalJSON(outcomeValue)
	if err != nil {
		t.Fatal(err)
	}
	action := journal.Action{
		ID:                     isolationActionPrefix + "foreign",
		ControlRunID:           scope.ControlRunID,
		Kind:                   journal.KindObserve,
		GraphRevision:          1,
		CanonicalRequestDigest: mustDigest(t, requestValue),
		IdempotencyKey:         "reserved-isolation-namespace",
	}
	_, err = store.Commit(context.Background(), journal.CommitRequest{
		Action:         action,
		ExpectedRun:    journal.NewRunCursor(scope.InstallationID, scope.ControlRunID),
		ExpectedGlobal: journal.NewGlobalCursor(scope.InstallationID),
		RequestPayload: requestPayload,
		Outcome: journal.Event{
			ID:            "reserved-isolation-outcome",
			ControlRunID:  scope.ControlRunID,
			ActionID:      action.ID,
			Kind:          journal.EventActionResult,
			PayloadDigest: mustDigest(t, outcomeValue),
			OccurredAt:    time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		},
		OutcomePayload: outcomePayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Current(context.Background(), scope); !errors.Is(err, journal.ErrInvalidRecord) {
		t.Fatalf("Current() error = %v, want invalid journal record", err)
	}
}

func TestRebuildRejectsActionSubjectRebinding(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-subject-rebinding")
	delta := inboxAppendDelta{
		EventID: "event", CorrelationID: "correlation",
		TaskID: "payload-task", AttemptID: "payload-attempt",
		ExternalActionID: "external-action", ActionGeneration: 1,
		Producer: "runtime", Consumer: "control", PayloadDigest: mustDigest(t, "claim"),
	}
	requestValue := requestEnvelope{
		SchemaVersion: isolationSchemaVersion, SemanticOperation: operationInboxAppend,
		Scope: scope, Input: delta,
	}
	requestPayload, requestDigest, err := canonicalPayload(requestValue)
	if err != nil {
		t.Fatal(err)
	}
	outcomeValue := outcomeEnvelope{
		SchemaVersion: isolationSchemaVersion, SemanticOperation: operationInboxAppend,
		Scope: scope, InboxAppend: &delta,
	}
	outcomePayload, outcomeDigest, err := canonicalPayload(outcomeValue)
	if err != nil {
		t.Fatal(err)
	}
	action := journal.Action{
		ID: isolationActionPrefix + "subject-rebinding", ControlRunID: scope.ControlRunID,
		TaskID: "different-task", AttemptID: "different-attempt", Kind: journal.KindCallback,
		GraphRevision: 1, CanonicalRequestDigest: requestDigest,
		IdempotencyKey: "subject-rebinding",
	}
	_, err = store.Commit(context.Background(), journal.CommitRequest{
		Action:         action,
		ExpectedRun:    journal.NewRunCursor(scope.InstallationID, scope.ControlRunID),
		ExpectedGlobal: journal.NewGlobalCursor(scope.InstallationID),
		RequestPayload: requestPayload,
		Outcome: journal.Event{
			ID: "subject-rebinding-outcome", ControlRunID: scope.ControlRunID,
			ActionID: action.ID, Kind: journal.EventActionResult,
			PayloadDigest: outcomeDigest,
			OccurredAt:    time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		},
		OutcomePayload: outcomePayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Current(context.Background(), scope); !errors.Is(err, journal.ErrInvalidRecord) {
		t.Fatalf("Current() error = %v, want invalid journal record", err)
	}
}

func TestRebuildRejectsRequestOutcomeAndDerivedIdentityRebinding(t *testing.T) {
	t.Parallel()

	operations := []string{operationTaskPhase, operationInboxAppend, operationGateRegister}
	mutations := []struct {
		name  string
		apply func(*replayBindingRecord, replayBindingRecord)
	}{
		{
			name: "request_outcome_delta",
			apply: func(record *replayBindingRecord, alternate replayBindingRecord) {
				record.Action = alternate.Action
				record.Outcome = alternate.Outcome
				record.OutcomePayload = alternate.OutcomePayload
			},
		},
		{
			name: "operation_id",
			apply: func(record *replayBindingRecord, _ replayBindingRecord) {
				record.Request["operation_id"] = "rebound-operation"
			},
		},
		{
			name: "action_id",
			apply: func(record *replayBindingRecord, _ replayBindingRecord) {
				record.Action.ID += "-rebound"
				record.Outcome.ActionID = record.Action.ID
			},
		},
		{
			name: "idempotency_key",
			apply: func(record *replayBindingRecord, _ replayBindingRecord) {
				record.Action.IdempotencyKey += "-rebound"
			},
		},
		{
			name: "graph_revision",
			apply: func(record *replayBindingRecord, _ replayBindingRecord) {
				record.Request["graph_revision"] = record.Action.GraphRevision + 1
			},
		},
		{
			name: "expected_revision",
			apply: func(record *replayBindingRecord, _ replayBindingRecord) {
				record.Request["expected_revision"] = record.Action.ExpectedProjection + 1
			},
		},
		{
			name: "outcome_event_id",
			apply: func(record *replayBindingRecord, _ replayBindingRecord) {
				record.Outcome.ID += "-rebound"
			},
		},
	}

	for _, operation := range operations {
		operation := operation
		for _, mutation := range mutations {
			mutation := mutation
			t.Run(operation+"/"+mutation.name, func(t *testing.T) {
				t.Parallel()

				primary := recordIsolationOperation(t, operation, "primary")
				alternate := recordIsolationOperation(t, operation, "alternate")
				mutation.apply(&primary, alternate)

				service, store, scope, projection := prepareIsolationOperation(t, operation)
				before := cloneProjection(projection)
				requestPayload, requestDigest, err := canonicalPayload(primary.Request)
				if err != nil {
					t.Fatal(err)
				}
				primary.Action.CanonicalRequestDigest = requestDigest
				primary.Outcome.RunSequence = 0
				primary.Outcome.JournalPosition = 0
				_, runCursor, err := service.rebuild(context.Background(), scope)
				if err != nil {
					t.Fatal(err)
				}
				globalCursor, err := service.globalCursor(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := store.Commit(context.Background(), journal.CommitRequest{
					Action: primary.Action, ExpectedRun: runCursor, ExpectedGlobal: globalCursor,
					RequestPayload: requestPayload, Outcome: primary.Outcome,
					OutcomePayload: primary.OutcomePayload,
				})
				if err != nil {
					t.Fatal(err)
				}

				err = service.applyOutcome(context.Background(), &projection, receipt.Outcome)
				if !errors.Is(err, journal.ErrInvalidRecord) {
					t.Errorf("applyOutcome(rebound record) error = %v, want invalid record", err)
				}
				if !reflect.DeepEqual(projection, before) {
					t.Errorf("rejected record mutated projection\n got: %#v\nwant: %#v", projection, before)
				}
			})
		}
	}
}

type replayBindingRecord struct {
	Action         journal.Action
	Outcome        journal.Event
	Request        map[string]any
	OutcomePayload []byte
}

func recordIsolationOperation(t *testing.T, operation, variant string) replayBindingRecord {
	t.Helper()
	service, store, scope, projection := prepareIsolationOperation(t, operation)
	const operationID = "binding-operation"

	var result CommitResult
	var err error
	switch operation {
	case operationTaskPhase:
		result, err = service.TransitionTask(context.Background(), TaskPhaseRequest{
			Scope: scope, OperationID: operationID, GraphRevision: 7,
			ExpectedRevision: projection.Revision,
			TaskID:           "task-" + variant, AttemptID: "attempt-" + variant,
			To: TaskDiscovered,
		})
	case operationInboxAppend:
		request := callbackRequest(t, scope, operationID, projection.Revision, "event-"+variant, "payload-"+variant)
		request.GraphRevision = 7
		result, err = service.AppendCallback(context.Background(), request)
	case operationGateRegister:
		result, err = service.RegisterGate(context.Background(), RegisterGateRequest{
			Scope: scope, OperationID: operationID, GraphRevision: 7,
			ExpectedRevision: projection.Revision,
			Gate: PendingWorkGate{
				ID: "gate-" + variant, TaskID: "task", Kind: GateHumanApproval,
				ResolverAuthority: "operator", WakeEventID: "approval-" + variant,
			},
		})
	default:
		t.Fatalf("unsupported replay binding operation %q", operation)
	}
	if err != nil {
		t.Fatal(err)
	}
	requestPayload, err := store.Payload(context.Background(), result.Receipt.Action.CanonicalRequestDigest)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := journal.DecodeStrict(requestPayload, &request); err != nil {
		t.Fatal(err)
	}
	request["operation_id"] = operationID
	request["graph_revision"] = uint64(7)
	request["expected_revision"] = projection.Revision
	outcomePayload, err := store.Payload(context.Background(), result.Receipt.Outcome.PayloadDigest)
	if err != nil {
		t.Fatal(err)
	}
	return replayBindingRecord{
		Action: result.Receipt.Action, Outcome: result.Receipt.Outcome,
		Request: request, OutcomePayload: outcomePayload,
	}
}

func prepareIsolationOperation(
	t *testing.T,
	operation string,
) (*Service, *journal.MemoryStore, RunScope, Projection) {
	t.Helper()
	service, store := newTestService(t)
	runID := "run-bind-task"
	if operation == operationInboxAppend {
		runID = "run-bind-callback"
	}
	if operation == operationGateRegister {
		runID = "run-bind-gate"
	}
	scope := mustRunScope(t, "installation-1", runID)
	if operation == operationGateRegister {
		deferTask(t, service, scope)
	}
	projection, err := service.Current(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, scope, projection
}

func cloneProjection(projection Projection) Projection {
	projection.Tasks = slices.Clone(projection.Tasks)
	projection.Inbox = slices.Clone(projection.Inbox)
	projection.Gates = slices.Clone(projection.Gates)
	return projection
}

func TestScopeConcurrentUnrelatedRunsDoNotContaminateInboxOrPhase(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	runs := []RunScope{
		mustRunScope(t, "installation-1", "run-a"),
		mustRunScope(t, "installation-1", "run-b"),
	}
	start := make(chan struct{})
	var wait sync.WaitGroup
	errorsByRun := make([]error, len(runs))
	for index, scope := range runs {
		index, scope := index, scope
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			request := callbackRequest(t, scope, "same-operation", 0, "same-event", fmt.Sprintf("payload-%d", index))
			request.TaskID = "same-task"
			request.AttemptID = "same-attempt"
			request.ExternalActionID = "same-action"
			request.CorrelationID = "same-correlation"
			_, errorsByRun[index] = service.AppendCallback(context.Background(), request)
		}()
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByRun {
		if err != nil {
			t.Fatalf("run %d callback: %v", index, err)
		}
	}
	for index, scope := range runs {
		projection, err := service.Current(context.Background(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if len(projection.Inbox) != 1 || projection.Inbox[0].PayloadDigest != mustDigest(t, fmt.Sprintf("payload-%d", index)) {
			t.Fatalf("run %d inbox contaminated: %#v", index, projection.Inbox)
		}
	}
}

func callbackRequest(
	t *testing.T,
	scope RunScope,
	operation string,
	expectedRevision uint64,
	eventID string,
	payload string,
) CallbackRequest {
	t.Helper()
	return CallbackRequest{
		Scope:            scope,
		OperationID:      operation,
		GraphRevision:    1,
		ExpectedRevision: expectedRevision,
		EventID:          eventID,
		CorrelationID:    "correlation-" + eventID,
		TaskID:           "task",
		AttemptID:        "attempt",
		ExternalActionID: "external-action",
		ActionGeneration: 1,
		Producer:         "runtime",
		Consumer:         "control",
		PayloadDigest:    mustDigest(t, payload),
	}
}

func deferTask(t *testing.T, service *Service, scope RunScope) uint64 {
	t.Helper()
	discovered, err := service.TransitionTask(context.Background(), TaskPhaseRequest{
		Scope: scope, OperationID: "discover", GraphRevision: 1,
		ExpectedRevision: 0, TaskID: "task", AttemptID: "attempt", To: TaskDiscovered,
	})
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := service.TransitionTask(context.Background(), TaskPhaseRequest{
		Scope: scope, OperationID: "defer", GraphRevision: 1,
		ExpectedRevision: discovered.Projection.Revision,
		TaskID:           "task", AttemptID: "attempt", From: TaskDiscovered, To: TaskDeferred,
	})
	if err != nil {
		t.Fatal(err)
	}
	return deferred.Projection.Revision
}

func newTestService(t *testing.T) (*Service, *journal.MemoryStore) {
	t.Helper()
	return newTestServiceWithInstallation(t, "installation-1")
}

func newTestServiceWithInstallation(t *testing.T, installationID string) (*Service, *journal.MemoryStore) {
	t.Helper()
	return newTestServiceWithClock(t, installationID, func() time.Time {
		return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	})
}

func newTestServiceWithClock(
	t *testing.T,
	installationID string,
	now func() time.Time,
) (*Service, *journal.MemoryStore) {
	t.Helper()
	store, err := journal.NewMemoryStore(installationID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(store, installationID, now)
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func feedLength(t *testing.T, store *journal.MemoryStore) int {
	t.Helper()
	events, _, err := store.Feed(context.Background(), journal.GlobalCursor{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

func permutations(values []string) [][]string {
	result := make([][]string, 0)
	var visit func(int)
	visit = func(index int) {
		if index == len(values) {
			result = append(result, append([]string(nil), values...))
			return
		}
		for candidate := index; candidate < len(values); candidate++ {
			values[index], values[candidate] = values[candidate], values[index]
			visit(index + 1)
			values[index], values[candidate] = values[candidate], values[index]
		}
	}
	visit(0)
	return result
}

type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMutableClock(now time.Time) *mutableClock {
	return &mutableClock{now: now}
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mutableClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}
