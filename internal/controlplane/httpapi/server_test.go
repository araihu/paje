package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane"
	controlhttp "github.com/araihu/paje/internal/controlplane/httpapi"
	controlmock "github.com/araihu/paje/internal/controlplane/mock"
	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/submission/auth"
)

const controlToken = "paje_v1_codex01.ERERERERERERERERERERERERERERERERERERERERERE"

type controlFixture struct {
	handler       http.Handler
	service       *controlplane.Service
	store         *controlmock.Store
	authenticator *auth.Authenticator
	principal     submission.Principal
	foreign       submission.Principal
}

func TestRouteCapabilitiesControlCreateReuseAndCursor(t *testing.T) {
	fixture := newControlFixture(t)

	capabilities := controlRequest(t, fixture, fixture.principal, http.MethodGet, "/v1/capabilities", nil, "")
	if capabilities.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d, body = %s", capabilities.Code, capabilities.Body.String())
	}
	var capabilitiesBody struct {
		APIVersion   string   `json:"api_version"`
		CredentialID string   `json:"credential_id"`
		Actions      []string `json:"actions"`
		Harnesses    []string `json:"harnesses"`
		Projects     []string `json:"projects"`
	}
	decodeControlResponse(t, capabilities, &capabilitiesBody)
	if capabilitiesBody.APIVersion != "v1" || capabilitiesBody.CredentialID != "codex01" ||
		!contains(capabilitiesBody.Actions, auth.ActionControlCreate) ||
		!contains(capabilitiesBody.Harnesses, "codex") || !contains(capabilitiesBody.Projects, "service") {
		t.Fatalf("capabilities = %#v", capabilitiesBody)
	}

	body := createControlBody(t, validControlRun(), validControlGraph())
	created := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", body, strings.Repeat("a", 32))
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	reused := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", body, strings.Repeat("a", 32))
	if reused.Code != http.StatusOK {
		t.Fatalf("reuse status = %d, body = %s", reused.Code, reused.Body.String())
	}

	changedRun := validControlRun()
	changedRun.GoalDigest = digestControl("changed-goal")
	changed := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", createControlBody(t, changedRun, validControlGraph()), strings.Repeat("a", 32))
	assertControlError(t, changed, http.StatusConflict, "idempotency_conflict")

	status := controlRequest(t, fixture, fixture.principal, http.MethodGet, "/v1/control-runs/control-1?after_cursor=0", nil, "")
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status.Code, status.Body.String())
	}
	if status.Header().Get("Retry-After") == "" {
		t.Fatal("open control status is missing Retry-After")
	}
	var statusBody struct {
		Snapshot   controlplane.Snapshot `json:"snapshot"`
		Events     []controlplane.Event  `json:"events"`
		NextCursor string                `json:"next_cursor"`
	}
	decodeControlResponse(t, status, &statusBody)
	if statusBody.Snapshot.Run.PrincipalID != "codex01" || statusBody.NextCursor == "" {
		t.Fatalf("status body = %#v", statusBody)
	}
	_, err := strconv.ParseUint(statusBody.NextCursor, 10, 64)
	if err != nil {
		t.Fatalf("next cursor = %q, error = %v", statusBody.NextCursor, err)
	}
	empty := controlRequest(t, fixture, fixture.principal, http.MethodGet, "/v1/control-runs/control-1?after_cursor="+statusBody.NextCursor, nil, "")
	var emptyBody struct {
		Events     []controlplane.Event `json:"events"`
		NextCursor string               `json:"next_cursor"`
	}
	decodeControlResponse(t, empty, &emptyBody)
	if len(emptyBody.Events) != 0 || emptyBody.NextCursor != statusBody.NextCursor {
		t.Fatalf("cursor replay = %#v", emptyBody)
	}

	foreign := controlRequest(t, fixture, fixture.foreign, http.MethodGet, "/v1/control-runs/control-1", nil, "")
	assertControlError(t, foreign, http.StatusNotFound, "not_found")
}

func TestRouteIdempotencyKeyBindsCredentialMethodPathRunAndBody(t *testing.T) {
	fixture := newControlFixture(t)
	key := strings.Repeat("7", 32)
	createBody := createControlBody(t, validControlRun(), validControlGraph())
	created := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", createBody, key)
	if created.Code != http.StatusAccepted {
		t.Fatalf("first create = %d, body = %s", created.Code, created.Body.String())
	}

	restartedHandler, err := controlhttp.New(controlhttp.Dependencies{Service: fixture.service, Authenticator: fixture.authenticator})
	if err != nil {
		t.Fatal(err)
	}
	restarted := fixture
	restarted.handler = restartedHandler
	exact := controlRequest(t, restarted, restarted.principal, http.MethodPost, "/v1/control-runs", createBody, key)
	if exact.Code != http.StatusOK {
		t.Fatalf("exact replay after handler restart = %d, body = %s", exact.Code, exact.Body.String())
	}

	changedRun := validControlRun()
	changedRun.ID = "control-2"
	changedGraph := validControlGraph()
	changedGraph.ControlRunID = changedRun.ID
	changedRunRequest := controlRequest(t, restarted, restarted.principal, http.MethodPost, "/v1/control-runs", createControlBody(t, changedRun, changedGraph), key)
	assertControlError(t, changedRunRequest, http.StatusConflict, "idempotency_conflict")
	if _, err := fixture.service.Load(context.Background(), changedRun.ID); err == nil {
		t.Fatal("changed run was created despite credential-scoped key conflict")
	}

	task := validControlTask("task-b", "docs", agentharness.LocalSequential)
	taskRequest := controlRequest(t, restarted, restarted.principal, http.MethodPost, "/v1/control-runs/control-1/tasks", mustControlJSON(t, map[string]any{
		"expected_revision": uint64(1), "task": task,
		"integration_order": []string{"task-a", "task-b"}, "combined_gates": validControlGraph().CombinedGates,
	}), key)
	assertControlError(t, taskRequest, http.StatusConflict, "idempotency_conflict")
	unchanged, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Graph.Revision != 1 || len(unchanged.Graph.Tasks) != 1 {
		t.Fatalf("changed-path conflict mutated graph = %#v", unchanged.Graph)
	}

	foreignRun := validControlRun()
	foreignRun.ID = "control-foreign"
	foreignGraph := validControlGraph()
	foreignGraph.ControlRunID = foreignRun.ID
	foreign := controlRequest(t, restarted, restarted.foreign, http.MethodPost, "/v1/control-runs", createControlBody(t, foreignRun, foreignGraph), key)
	if foreign.Code != http.StatusAccepted {
		t.Fatalf("foreign credential using same clear key = %d, body = %s", foreign.Code, foreign.Body.String())
	}
}

func TestRouteTaskGraphCASAndScope(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)

	graph := validControlGraph()
	second := validControlTask("task-b", "docs", agentharness.LocalSequential)
	second.DependsOn = []string{"task-a"}
	second.Communication = []controlplane.CommunicationEdge{{ProjectID: "service", TaskID: "task-a"}}
	second.State = controlplane.TaskPending
	body := mustControlJSON(t, map[string]any{
		"expected_revision": uint64(1),
		"task":              second,
		"integration_order": []string{"task-a", "task-b"},
		"combined_gates":    graph.CombinedGates,
	})
	created := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks", body, strings.Repeat("b", 32))
	if created.Code != http.StatusAccepted {
		t.Fatalf("task create = %d, body = %s", created.Code, created.Body.String())
	}
	reused := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks", body, strings.Repeat("b", 32))
	if reused.Code != http.StatusOK {
		t.Fatalf("task reuse = %d, body = %s", reused.Code, reused.Body.String())
	}

	changedTask := second
	changedTask.Goal = "changed"
	conflictBody := mustControlJSON(t, map[string]any{
		"expected_revision": uint64(1), "task": changedTask,
		"integration_order": []string{"task-a", "task-b"}, "combined_gates": graph.CombinedGates,
	})
	conflict := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks", conflictBody, strings.Repeat("b", 32))
	assertControlError(t, conflict, http.StatusConflict, "idempotency_conflict")

	foreignTask := validControlTask("task-foreign", "unknown", agentharness.LocalSequential)
	foreignBody := mustControlJSON(t, map[string]any{
		"expected_revision": uint64(2), "task": foreignTask,
		"integration_order": []string{"task-a", "task-b", "task-foreign"}, "combined_gates": graph.CombinedGates,
	})
	denied := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks", foreignBody, strings.Repeat("c", 32))
	assertControlError(t, denied, http.StatusForbidden, "forbidden")
}

func TestRoutePersistentAttemptRuntimeAcknowledgementCallbackAndCloseDenial(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	key := strings.Repeat("d", 32)

	prepareBody := mustControlJSON(t, map[string]any{
		"operation": "prepare", "capabilities": completeControlCapabilities(),
		"request_digest": digestControl("dispatch"),
	})
	prepared := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks/task-a/attempts", prepareBody, key)
	if prepared.Code != http.StatusAccepted {
		t.Fatalf("dispatch prepare = %d, body = %s", prepared.Code, prepared.Body.String())
	}
	var preparedBody struct {
		Attempt controlplane.PlacementAttempt `json:"attempt"`
		Action  controlplane.LifecycleAction  `json:"action"`
	}
	decodeControlResponse(t, prepared, &preparedBody)
	if preparedBody.Attempt.ID == "" || preparedBody.Action.Kind != agentharness.ActionDispatch {
		t.Fatalf("prepared = %#v", preparedBody)
	}
	repeatedPrepare := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks/task-a/attempts", prepareBody, key)
	var repeatBody struct {
		Attempt controlplane.PlacementAttempt `json:"attempt"`
		Action  controlplane.LifecycleAction  `json:"action"`
	}
	decodeControlResponse(t, repeatedPrepare, &repeatBody)
	if repeatedPrepare.Code != http.StatusOK || repeatBody.Attempt.ID != preparedBody.Attempt.ID || repeatBody.Action.ID != preparedBody.Action.ID {
		t.Fatalf("repeated prepare = %d %#v", repeatedPrepare.Code, repeatBody)
	}

	completeBody := mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": preparedBody.Action.ID,
		"result": agentharness.ActionResult{
			ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-1"},
			Cursor: "runtime-cursor-1", CursorSequence: 1, ResultDigest: digestControl("dispatch-result"),
		},
	})
	completed := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks/task-a/attempts", completeBody, strings.Repeat("e", 32))
	if completed.Code != http.StatusOK {
		t.Fatalf("dispatch complete = %d, body = %s", completed.Code, completed.Body.String())
	}

	attemptPath := "/v1/control-runs/control-1/attempts/" + preparedBody.Attempt.ID
	observed := controlRequest(t, fixture, fixture.principal, http.MethodGet, attemptPath+"?after_cursor=0", nil, "")
	var observedBody struct {
		Attempt controlplane.PlacementAttempt `json:"attempt"`
		Session *controlplane.AgentSession    `json:"session"`
		Events  []controlplane.Event          `json:"events"`
	}
	decodeControlResponse(t, observed, &observedBody)
	if observedBody.Session == nil || observedBody.Session.RuntimeChildID != "runtime-child-1" || observedBody.Session.RuntimeIDAcknowledged {
		t.Fatalf("observed = %#v", observedBody)
	}

	registrationDigest := controlplane.RegistrationMessageDigest("control-1", preparedBody.Attempt.ID, "runtime-child-1")
	ackPrepare := controlRequest(t, fixture, fixture.principal, http.MethodPost, attemptPath+"/messages", mustControlJSON(t, map[string]any{
		"operation": "prepare", "kind": "acknowledge", "request_digest": registrationDigest,
	}), strings.Repeat("f", 32))
	var ackPrepareBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, ackPrepare, &ackPrepareBody)
	ackComplete := controlRequest(t, fixture, fixture.principal, http.MethodPost, attemptPath+"/messages", mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": ackPrepareBody.Action.ID,
		"result": agentharness.ActionResult{
			ActionID: ackPrepareBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-1"},
			ResultDigest: registrationDigest, MessageReceipt: "registration-receipt",
		},
		"runtime_child_id": "runtime-child-1",
	}), strings.Repeat("g", 32))
	if ackComplete.Code != http.StatusOK {
		t.Fatalf("ack complete = %d, body = %s", ackComplete.Code, ackComplete.Body.String())
	}
	var ackBody struct {
		Session *controlplane.AgentSession `json:"session"`
	}
	decodeControlResponse(t, ackComplete, &ackBody)
	if ackBody.Session == nil || !ackBody.Session.RuntimeIDAcknowledged {
		t.Fatalf("ack session = %#v", ackBody.Session)
	}

	callback := validControlCallback(preparedBody.Attempt.ID)
	callbackDigest := controlplane.CompletionCallbackDigest(callback)
	callbackPrepare := controlRequest(t, fixture, fixture.principal, http.MethodPost, attemptPath+"/messages", mustControlJSON(t, map[string]any{
		"operation": "prepare", "kind": "callback", "request_digest": callbackDigest,
	}), strings.Repeat("h", 32))
	var callbackPrepareBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, callbackPrepare, &callbackPrepareBody)
	callbackComplete := controlRequest(t, fixture, fixture.principal, http.MethodPost, attemptPath+"/messages", mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": callbackPrepareBody.Action.ID,
		"result": agentharness.ActionResult{
			ActionID: callbackPrepareBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-1"}, ResultDigest: callbackDigest,
		},
		"callback": callback,
	}), strings.Repeat("i", 32))
	if callbackComplete.Code != http.StatusOK {
		t.Fatalf("callback complete = %d, body = %s", callbackComplete.Code, callbackComplete.Body.String())
	}

	waitPrepare := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/attempts:wait", mustControlJSON(t, map[string]any{
		"operation": "prepare", "attempt_id": preparedBody.Attempt.ID, "request_digest": digestControl("wait"),
	}), strings.Repeat("j", 32))
	var waitPrepareBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, waitPrepare, &waitPrepareBody)
	waitComplete := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/attempts:wait", mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": waitPrepareBody.Action.ID,
		"result": agentharness.ActionResult{
			ActionID: waitPrepareBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-1"},
			Cursor: "runtime-cursor-2", CursorSequence: 2, ResultDigest: digestControl("wait-result"),
			Events: []agentharness.WorkEvent{{
				ID: "terminal-event", RuntimeWorkID: "runtime-child-1", Cursor: "runtime-cursor-2",
				CursorSequence: 2, Kind: "completed", ResultDigest: digestControl("terminal"), Terminal: true,
			}},
		},
	}), strings.Repeat("k", 32))
	if waitComplete.Code != http.StatusOK {
		t.Fatalf("wait complete = %d, body = %s", waitComplete.Code, waitComplete.Body.String())
	}

	closeRun := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/close", []byte(`{}`), strings.Repeat("l", 32))
	assertControlError(t, closeRun, http.StatusConflict, "cleanup_incomplete")
	snapshot, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != controlplane.StatusClosing || snapshot.Run.Close.Pending.PersistentSessionsUnarchived != 1 || snapshot.Run.Close.Pending.TotalPendingWork == 0 {
		t.Fatalf("close state = %#v", snapshot.Run.Close)
	}
}

func TestRouteIdempotencyConflictPrecedesAttemptMutation(t *testing.T) {
	fixture := newControlFixture(t)
	key := strings.Repeat("8", 32)
	created := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", createControlBody(t, validControlRun(), validControlGraph()), key)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create = %d, body = %s", created.Code, created.Body.String())
	}
	before, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	response := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks/task-a/attempts", mustControlJSON(t, map[string]any{
		"operation": "prepare", "capabilities": completeControlCapabilities(), "request_digest": digestControl("idempotency-conflicting-dispatch"),
	}), key)
	assertControlError(t, response, http.StatusConflict, "idempotency_conflict")
	after, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	assertControlMutationStateUnchanged(t, before, after)
}

func TestRouteIdempotencyKeyCoversEveryMutationSurface(t *testing.T) {
	key := strings.Repeat("a", 32)
	tests := []struct {
		name string
		path func(controlplane.PlacementAttempt) string
		body func(controlplane.PlacementAttempt) []byte
	}{
		{
			name: "task create",
			path: func(controlplane.PlacementAttempt) string { return "/v1/control-runs/control-1/tasks" },
			body: func(controlplane.PlacementAttempt) []byte {
				task := validControlTask("task-b", "service", agentharness.LocalSequential)
				return mustControlJSON(t, map[string]any{
					"expected_revision": uint64(1), "task": task,
					"integration_order": []string{"task-a", "task-b"},
					"combined_gates":    validControlGraph().CombinedGates,
				})
			},
		},
		{
			name: "dispatch prepare",
			path: func(controlplane.PlacementAttempt) string { return "/v1/control-runs/control-1/tasks/task-a/attempts" },
			body: func(controlplane.PlacementAttempt) []byte {
				return mustControlJSON(t, map[string]any{
					"operation": "prepare", "capabilities": completeControlCapabilities(),
					"request_digest": digestControl("all-surfaces-dispatch"),
				})
			},
		},
		{
			name: "observe prepare",
			path: func(attempt controlplane.PlacementAttempt) string {
				return "/v1/control-runs/control-1/attempts/" + attempt.ID
			},
			body: func(attempt controlplane.PlacementAttempt) []byte {
				return mustControlJSON(t, map[string]any{
					"operation": "prepare", "request_digest": digestControlObserve("control-1", attempt.ID, attempt.LastCursor, attempt.CursorSequence),
					"after_cursor": attempt.LastCursor, "after_cursor_sequence": attempt.CursorSequence,
				})
			},
		},
		{
			name: "message prepare",
			path: func(attempt controlplane.PlacementAttempt) string {
				return "/v1/control-runs/control-1/attempts/" + attempt.ID + "/messages"
			},
			body: func(controlplane.PlacementAttempt) []byte {
				return mustControlJSON(t, map[string]any{
					"operation": "prepare", "kind": "acknowledge",
					"request_digest": digestControl("all-surfaces-message"),
				})
			},
		},
		{
			name: "wait prepare",
			path: func(controlplane.PlacementAttempt) string { return "/v1/control-runs/control-1/attempts:wait" },
			body: func(attempt controlplane.PlacementAttempt) []byte {
				return mustControlJSON(t, map[string]any{
					"operation": "prepare", "attempt_id": attempt.ID,
					"request_digest": digestControl("all-surfaces-wait"),
				})
			},
		},
		{
			name: "interrupt prepare",
			path: func(attempt controlplane.PlacementAttempt) string {
				return "/v1/control-runs/control-1/attempts/" + attempt.ID + "/interrupt"
			},
			body: func(controlplane.PlacementAttempt) []byte {
				return mustControlJSON(t, map[string]any{
					"operation": "prepare", "request_digest": digestControl("all-surfaces-interrupt"),
				})
			},
		},
		{
			name: "close prepare",
			path: func(attempt controlplane.PlacementAttempt) string {
				return "/v1/control-runs/control-1/attempts/" + attempt.ID + "/close"
			},
			body: func(controlplane.PlacementAttempt) []byte {
				return mustControlJSON(t, map[string]any{
					"operation": "prepare", "request_digest": digestControl("all-surfaces-close"),
				})
			},
		},
		{
			name: "record evidence",
			path: func(controlplane.PlacementAttempt) string { return "/v1/control-runs/control-1/evidence" },
			body: func(attempt controlplane.PlacementAttempt) []byte {
				return mustControlJSON(t, map[string]any{
					"operation": "record",
					"evidence": controlplane.Evidence{
						ID: "all-surfaces-evidence", TaskID: "task-a", AttemptID: attempt.ID,
						BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
						OwnedPathsDigest: digestControl("all-surfaces-owned"),
						Tests: []controlplane.TestEvidence{{
							CommandDigest: digestControl("all-surfaces-command"),
							ResultDigest:  digestControl("all-surfaces-result"), Passed: true,
						}},
					},
				})
			},
		},
		{
			name: "control close",
			path: func(controlplane.PlacementAttempt) string { return "/v1/control-runs/control-1/close" },
			body: func(controlplane.PlacementAttempt) []byte { return []byte(`{}`) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newControlFixture(t)
			createControl(t, fixture)
			attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			before, err := fixture.service.Load(context.Background(), "control-1")
			if err != nil {
				t.Fatal(err)
			}
			response := controlRequest(t, fixture, fixture.principal, http.MethodPost, test.path(attempt), test.body(attempt), key)
			assertControlError(t, response, http.StatusConflict, "idempotency_conflict")
			after, err := fixture.service.Load(context.Background(), "control-1")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("target state mutated on key conflict: before = %#v, after = %#v", before, after)
			}
		})
	}

	t.Run("local close", func(t *testing.T) {
		fixture := newControlFixture(t)
		graph := validControlGraph()
		graph.Tasks[0] = validControlTask("task-a", "service", agentharness.LocalSequential)
		response := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", createControlBody(t, validControlRun(), graph), key)
		if response.Code != http.StatusAccepted {
			t.Fatalf("create local control = %d, body = %s", response.Code, response.Body.String())
		}
		attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
		if err != nil {
			t.Fatal(err)
		}
		before, err := fixture.service.Load(context.Background(), "control-1")
		if err != nil {
			t.Fatal(err)
		}
		closeResponse := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/attempts/"+attempt.ID+"/close", mustControlJSON(t, map[string]any{
			"operation": "local",
			"close_evidence": controlplane.WorkCloseEvidence{
				Kind: controlplane.CloseInactive, Receipt: "all-surfaces-inactive", Digest: digestControl("all-surfaces-local-close"),
			},
		}), key)
		assertControlError(t, closeResponse, http.StatusConflict, "idempotency_conflict")
		after, err := fixture.service.Load(context.Background(), "control-1")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("local close mutated on key conflict: before = %#v, after = %#v", before, after)
		}
	})
}

func TestMalformedDispatchDigestDoesNotMutatePlacementState(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	before, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	response := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks/task-a/attempts", mustControlJSON(t, map[string]any{
		"operation": "prepare", "capabilities": completeControlCapabilities(), "request_digest": "not-a-digest",
	}), strings.Repeat("9", 32))
	assertControlError(t, response, http.StatusBadRequest, "invalid_request")
	after, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	assertControlMutationStateUnchanged(t, before, after)
}

func TestRouteObserveLifecycleBindsKindAndPersistedCursor(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := fixture.service.PrepareAction(context.Background(), "control-1", attempt.ID, agentharness.ActionDispatch, digestControl("dispatch-for-observe"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CompleteAction(context.Background(), "control-1", dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-child-observe"}, Cursor: "cursor-1", CursorSequence: 1,
		ResultDigest: digestControl("dispatch-observe-result"),
	}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/control-runs/control-1/attempts/" + attempt.ID
	before, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	future := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "prepare", "request_digest": digestControlObserve("control-1", attempt.ID, "cursor-2", 2),
		"after_cursor": "cursor-2", "after_cursor_sequence": uint64(2),
	}), strings.Repeat("A", 32))
	assertControlError(t, future, http.StatusConflict, "idempotency_conflict")
	afterFuture, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	assertControlMutationStateUnchanged(t, before, afterFuture)

	prepareBody := mustControlJSON(t, map[string]any{
		"operation": "prepare", "request_digest": digestControlObserve("control-1", attempt.ID, "cursor-1", 1),
		"after_cursor": "cursor-1", "after_cursor_sequence": uint64(1),
	})
	prepared := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, prepareBody, strings.Repeat("B", 32))
	if prepared.Code != http.StatusAccepted {
		t.Fatalf("observe prepare = %d, body = %s", prepared.Code, prepared.Body.String())
	}
	var preparedBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, prepared, &preparedBody)
	if preparedBody.Action.Kind != agentharness.ActionObserve {
		t.Fatalf("observe action = %#v", preparedBody.Action)
	}
	reused := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, prepareBody, strings.Repeat("B", 32))
	var reusedBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, reused, &reusedBody)
	if reused.Code != http.StatusOK || reusedBody.Action.ID != preparedBody.Action.ID {
		t.Fatalf("observe prepare retry = %d %#v", reused.Code, reusedBody)
	}

	wait, err := fixture.service.PrepareAction(context.Background(), "control-1", attempt.ID, agentharness.ActionWait, digestControl("wrong-kind-for-observe"))
	if err != nil {
		t.Fatal(err)
	}
	wrongKind := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": wait.ID,
		"result": agentharness.ActionResult{
			ActionID: wait.ID, RuntimeWorkIDs: []string{"runtime-child-observe"}, Cursor: "cursor-2", CursorSequence: 2,
			ResultDigest: digestControl("wrong-kind-observe-result"),
		},
	}), strings.Repeat("C", 32))
	assertControlError(t, wrongKind, http.StatusConflict, "idempotency_conflict")

	result := agentharness.ActionResult{
		ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-observe"}, Cursor: "cursor-2", CursorSequence: 2,
		ResultDigest: digestControl("observe-result"), Events: []agentharness.WorkEvent{{
			ID: "observe-event-2", RuntimeWorkID: "runtime-child-observe", Cursor: "cursor-2", CursorSequence: 2,
			Kind: "progress", ResultDigest: digestControl("observe-event-result"),
		}},
	}
	completed := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": preparedBody.Action.ID, "result": result,
	}), strings.Repeat("D", 32))
	if completed.Code != http.StatusOK {
		t.Fatalf("observe complete = %d, body = %s", completed.Code, completed.Body.String())
	}
	var completedBody struct {
		Attempt    controlplane.PlacementAttempt `json:"attempt"`
		NextCursor string                        `json:"next_cursor"`
	}
	decodeControlResponse(t, completed, &completedBody)
	if completedBody.Attempt.LastCursor != "cursor-2" || completedBody.Attempt.CursorSequence != 2 || completedBody.NextCursor != "cursor-2" {
		t.Fatalf("completed observe cursor = %#v", completedBody)
	}
	replayed := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": preparedBody.Action.ID, "result": result,
	}), strings.Repeat("D", 32))
	if replayed.Code != http.StatusOK {
		t.Fatalf("observe complete replay = %d, body = %s", replayed.Code, replayed.Body.String())
	}
}

func TestRouteObserveCanonicalRequestReplaysAfterProgressAndRestart(t *testing.T) {
	t.Run("exact prior tuple replays while stale digest and changed key do not", func(t *testing.T) {
		fixture := newControlFixture(t)
		createControl(t, fixture)
		attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
		if err != nil {
			t.Fatal(err)
		}
		dispatch, err := fixture.service.PrepareAction(context.Background(), "control-1", attempt.ID, agentharness.ActionDispatch, digestControl("dispatch-for-observe-replay"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.CompleteAction(context.Background(), "control-1", dispatch.ID, agentharness.ActionResult{
			ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-child-observe-replay"},
			Cursor: "cursor-1", CursorSequence: 1, ResultDigest: digestControl("dispatch-observe-replay-result"),
		}); err != nil {
			t.Fatal(err)
		}

		path := "/v1/control-runs/control-1/attempts/" + attempt.ID
		originalDigest := digestControlObserve("control-1", attempt.ID, "cursor-1", 1)
		originalBody := mustControlJSON(t, map[string]any{
			"operation": "prepare", "request_digest": originalDigest,
			"after_cursor": "cursor-1", "after_cursor_sequence": uint64(1),
		})
		prepareKey := strings.Repeat("E", 32)
		prepared := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, originalBody, prepareKey)
		if prepared.Code != http.StatusAccepted {
			t.Fatalf("initial observe prepare = %d, body = %s", prepared.Code, prepared.Body.String())
		}
		var preparedBody struct {
			Action controlplane.LifecycleAction `json:"action"`
		}
		decodeControlResponse(t, prepared, &preparedBody)
		result := agentharness.ActionResult{
			ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-observe-replay"},
			Cursor: "cursor-2", CursorSequence: 2, ResultDigest: digestControl("observe-replay-result"),
			Events: []agentharness.WorkEvent{{
				ID: "observe-replay-event", RuntimeWorkID: "runtime-child-observe-replay",
				Cursor: "cursor-2", CursorSequence: 2, Kind: "progress", ResultDigest: digestControl("observe-replay-event-result"),
			}},
		}
		completed := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "complete", "action_id": preparedBody.Action.ID, "result": result,
		}), strings.Repeat("F", 32))
		if completed.Code != http.StatusOK {
			t.Fatalf("observe complete = %d, body = %s", completed.Code, completed.Body.String())
		}
		beforeReplay, err := fixture.service.Load(context.Background(), "control-1")
		if err != nil {
			t.Fatal(err)
		}

		restartedHandler, err := controlhttp.New(controlhttp.Dependencies{
			Service: fixture.service, Authenticator: fixture.authenticator,
		})
		if err != nil {
			t.Fatal(err)
		}
		restarted := fixture
		restarted.handler = restartedHandler
		replayed := controlRequest(t, restarted, restarted.principal, http.MethodPost, path, originalBody, prepareKey)
		if replayed.Code != http.StatusOK {
			t.Fatalf("exact observe prepare replay after restart = %d, body = %s", replayed.Code, replayed.Body.String())
		}
		var replayedBody struct {
			Action controlplane.LifecycleAction `json:"action"`
		}
		decodeControlResponse(t, replayed, &replayedBody)
		if replayedBody.Action.ID != preparedBody.Action.ID || !replayedBody.Action.Completed {
			t.Fatalf("exact replay action = %#v, want completed %s", replayedBody.Action, preparedBody.Action.ID)
		}
		afterReplay, err := fixture.service.Load(context.Background(), "control-1")
		if err != nil {
			t.Fatal(err)
		}
		if afterReplay.Run.EventCursor != beforeReplay.Run.EventCursor || len(afterReplay.Events) != len(beforeReplay.Events) ||
			len(afterReplay.Actions) != len(beforeReplay.Actions) {
			t.Fatalf("exact observe replay mutated target state: before = %#v, after = %#v", beforeReplay, afterReplay)
		}

		staleDigest := controlRequest(t, restarted, restarted.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "prepare", "request_digest": originalDigest,
			"after_cursor": "cursor-2", "after_cursor_sequence": uint64(2),
		}), strings.Repeat("G", 32))
		assertControlError(t, staleDigest, http.StatusConflict, "idempotency_conflict")

		nextDigest := digestControlObserve("control-1", attempt.ID, "cursor-2", 2)
		changedKey := controlRequest(t, restarted, restarted.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "prepare", "request_digest": nextDigest,
			"after_cursor": "cursor-2", "after_cursor_sequence": uint64(2),
		}), prepareKey)
		assertControlError(t, changedKey, http.StatusConflict, "idempotency_conflict")

		next := controlRequest(t, restarted, restarted.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "prepare", "request_digest": nextDigest,
			"after_cursor": "cursor-2", "after_cursor_sequence": uint64(2),
		}), strings.Repeat("H", 32))
		if next.Code != http.StatusAccepted {
			t.Fatalf("next observe tuple prepare = %d, body = %s", next.Code, next.Body.String())
		}
		var nextBody struct {
			Action controlplane.LifecycleAction `json:"action"`
		}
		decodeControlResponse(t, next, &nextBody)
		if nextBody.Action.ID == preparedBody.Action.ID || nextBody.Action.RequestDigest != nextDigest {
			t.Fatalf("next observe action = %#v", nextBody.Action)
		}
	})

	t.Run("digest from another attempt is rejected before mutation", func(t *testing.T) {
		fixture := newControlFixture(t)
		graph := validControlGraph()
		graph.Tasks = append(graph.Tasks, validControlTask("task-b", "docs", agentharness.PersistentSession))
		graph.IntegrationOrder = []string{"task-a", "task-b"}
		run := validControlRun()
		run.PrincipalID = fixture.principal.CredentialID
		if _, err := fixture.service.Create(context.Background(), run, graph); err != nil {
			t.Fatal(err)
		}
		activate := func(taskID, runtimeID string) controlplane.PlacementAttempt {
			attempt, err := fixture.service.CreateAttempt(context.Background(), run.ID, taskID, completeControlCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			dispatch, err := fixture.service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digestControl("dispatch-"+taskID))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.service.CompleteAction(context.Background(), run.ID, dispatch.ID, agentharness.ActionResult{
				ActionID: dispatch.ID, RuntimeWorkIDs: []string{runtimeID}, Cursor: "cursor-1", CursorSequence: 1,
				ResultDigest: digestControl("dispatch-result-" + taskID),
			}); err != nil {
				t.Fatal(err)
			}
			return attempt
		}
		first := activate("task-a", "runtime-child-observe-a")
		second := activate("task-b", "runtime-child-observe-b")
		before, err := fixture.service.Load(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		response := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/attempts/"+second.ID, mustControlJSON(t, map[string]any{
			"operation":      "prepare",
			"request_digest": digestControlObserve(run.ID, first.ID, "cursor-1", 1),
			"after_cursor":   "cursor-1", "after_cursor_sequence": uint64(1),
		}), strings.Repeat("I", 32))
		assertControlError(t, response, http.StatusConflict, "idempotency_conflict")
		after, err := fixture.service.Load(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Run.EventCursor != before.Run.EventCursor || len(after.Events) != len(before.Events) ||
			len(after.Actions) != len(before.Actions) || len(after.Attempts[second.ID].ActionIDs) != len(before.Attempts[second.ID].ActionIDs) {
			t.Fatalf("wrong-attempt observe digest mutated target state: before = %#v, after = %#v", before, after)
		}
	})
}

func TestRouteCapabilityMessageAndMalformedDenials(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)

	deniedCapabilities := completeControlCapabilities()
	deniedCapabilities.HarnessID = "other"
	denied := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks/task-a/attempts", mustControlJSON(t, map[string]any{
		"operation": "prepare", "capabilities": deniedCapabilities, "request_digest": digestControl("dispatch"),
	}), strings.Repeat("m", 32))
	assertControlError(t, denied, http.StatusForbidden, "forbidden")

	malformedCursor := controlRequest(t, fixture, fixture.principal, http.MethodGet, "/v1/control-runs/control-1?after_cursor=not-a-number", nil, "")
	assertControlError(t, malformedCursor, http.StatusBadRequest, "invalid_request")
	unknownQuery := controlRequest(t, fixture, fixture.principal, http.MethodGet, "/v1/control-runs/control-1?workflow_name=forbidden", nil, "")
	assertControlError(t, unknownQuery, http.StatusBadRequest, "invalid_request")
	trailing := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", append(createControlBody(t, validControlRun(), validControlGraph()), []byte(` {}`)...), strings.Repeat("n", 32))
	assertControlError(t, trailing, http.StatusBadRequest, "invalid_request")
	duplicateBody := []byte(strings.Replace(string(createControlBody(t, validControlRun(), validControlGraph())), `"run":`, `"run":`+string(mustControlJSON(t, validControlRun()))+`,"run":`, 1))
	duplicate := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", duplicateBody, strings.Repeat("n", 32))
	assertControlError(t, duplicate, http.StatusBadRequest, "invalid_request")
	oversized := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", []byte(`{"padding":"`+strings.Repeat("x", (1<<20)+1)+`"}`), strings.Repeat("o", 32))
	assertControlError(t, oversized, http.StatusRequestEntityTooLarge, "invalid_request")

	method := controlRequest(t, fixture, fixture.principal, http.MethodDelete, "/v1/control-runs/control-1", nil, "")
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("method response = %d, allow = %q", method.Code, method.Header().Get("Allow"))
	}
	unlisted := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/workflows", []byte(`{}`), strings.Repeat("p", 32))
	assertControlError(t, unlisted, http.StatusNotFound, "not_found")
}

func TestMalformedCaseVariantControlJSONLeavesStateUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{name: "top-level alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"run":`, `"Run":`, 1)
		}},
		{name: "top-level semantic duplicate", mutate: func(raw string) string {
			run := string(mustControlJSON(t, validControlRun()))
			return strings.Replace(raw, `"run":`, `"Run":`+run+`,"run":`, 1)
		}},
		{name: "nested run alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"principal_id":`, `"PRINCIPAL_ID":`, 1)
		}},
		{name: "nested graph alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"control_run_id":`, `"CONTROL_RUN_ID":`, 1)
		}},
		{name: "nested task collection alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"tasks":`, `"Tasks":`, 1)
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newControlFixture(t)
			raw := test.mutate(string(createControlBody(t, validControlRun(), validControlGraph())))
			response := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", []byte(raw), strings.Repeat(string(rune('L'+index)), 32))
			assertControlError(t, response, http.StatusBadRequest, "invalid_request")
			if _, err := fixture.service.Load(context.Background(), "control-1"); err == nil {
				t.Fatal("case-variant control JSON created durable state")
			}
		})
	}
}

func TestCursorRejectsMalformedDuplicateUnknownAndFutureValues(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.EventCursor == 0 {
		t.Fatal("fixture did not issue an event cursor")
	}

	runPath := "/v1/control-runs/control-1"
	attemptPath := runPath + "/attempts/" + attempt.ID
	queries := []string{
		"after_cursor=%zz",
		"after_cursor=0&after_cursor=0",
		"unknown=0",
		"after_cursor=" + strconv.FormatUint(snapshot.Run.EventCursor+1, 10),
		"after_cursor=18446744073709551615",
	}
	for _, path := range []string{runPath, attemptPath} {
		for _, query := range queries {
			response := controlRequest(t, fixture, fixture.principal, http.MethodGet, path+"?"+query, nil, "")
			assertControlError(t, response, http.StatusBadRequest, "invalid_request")
		}
		visible := controlRequest(t, fixture, fixture.principal, http.MethodGet, path+"?after_cursor=0", nil, "")
		if visible.Code != http.StatusOK {
			t.Fatalf("valid cursor read %s = %d, body = %s", path, visible.Code, visible.Body.String())
		}
		var body struct {
			Events     []controlplane.Event `json:"events"`
			NextCursor string               `json:"next_cursor"`
		}
		decodeControlResponse(t, visible, &body)
		if len(body.Events) == 0 || body.NextCursor != strconv.FormatUint(snapshot.Run.EventCursor, 10) {
			t.Fatalf("valid cursor read %s skipped issued events: %#v", path, body)
		}
	}
}

func TestRouteRejectsActionKindConfusionBeforeMutation(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	prepared := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/tasks/task-a/attempts", mustControlJSON(t, map[string]any{
		"operation": "prepare", "capabilities": completeControlCapabilities(), "request_digest": digestControl("dispatch-kind-boundary"),
	}), strings.Repeat("q", 32))
	var preparedBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, prepared, &preparedBody)

	confused := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/attempts:wait", mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": preparedBody.Action.ID,
		"result": agentharness.ActionResult{
			ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-confused"}, ResultDigest: digestControl("confused-result"),
		},
	}), strings.Repeat("r", 32))
	assertControlError(t, confused, http.StatusConflict, "idempotency_conflict")

	snapshot, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Actions[preparedBody.Action.ID].Completed || len(snapshot.Attempts) != 1 || snapshot.Attempts[firstAttemptID(snapshot)].State != controlplane.AttemptReserved {
		t.Fatalf("wrong route mutated action: %#v", snapshot.Actions[preparedBody.Action.ID])
	}
}

func TestRouteRejectsOutOfScopeMessageBeforeCompletingAction(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := fixture.service.PrepareAction(context.Background(), "control-1", attempt.ID, agentharness.ActionDispatch, digestControl("dispatch-for-message"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CompleteAction(context.Background(), "control-1", dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-child-message"}, ResultDigest: digestControl("dispatch-message-result"),
	}); err != nil {
		t.Fatal(err)
	}
	send, err := fixture.service.PrepareAction(context.Background(), "control-1", attempt.ID, agentharness.ActionSend, digestControl("send-foreign"))
	if err != nil {
		t.Fatal(err)
	}
	response := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs/control-1/attempts/"+attempt.ID+"/messages", mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": send.ID,
		"result": agentharness.ActionResult{
			ActionID: send.ID, RuntimeWorkIDs: []string{"runtime-child-message"},
			ResultDigest: digestControl("send-result"), MessageReceipt: "send-receipt",
		},
		"message": controlplane.Message{
			ID: "message-foreign", FromTaskID: "task-a", ToTaskID: "foreign",
			Kind: controlplane.MessageSteering, Digest: digestControl("foreign-message"),
		},
	}), strings.Repeat("s", 32))
	assertControlError(t, response, http.StatusForbidden, "forbidden")
	snapshot, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Actions[send.ID].Completed {
		t.Fatal("out-of-scope send action was completed")
	}
}

func TestRouteSendActionBindsExactMessageBeforeCompletion(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := fixture.service.PrepareAction(context.Background(), "control-1", attempt.ID, agentharness.ActionDispatch, digestControl("dispatch-for-bound-send"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CompleteAction(context.Background(), "control-1", dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-child-send"}, ResultDigest: digestControl("dispatch-bound-send-result"),
	}); err != nil {
		t.Fatal(err)
	}

	messageA := controlplane.Message{
		ID: "message-a", FromTaskID: controlplane.ParentAddress, ToTaskID: "task-a",
		Kind: controlplane.MessageSteering, Digest: digestControl("message-a"),
	}
	path := "/v1/control-runs/control-1/attempts/" + attempt.ID + "/messages"
	prepared := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "prepare", "kind": "send", "request_digest": digestControlSend(messageA), "message": messageA,
	}), strings.Repeat("3", 32))
	if prepared.Code != http.StatusAccepted {
		t.Fatalf("send prepare = %d, body = %s", prepared.Code, prepared.Body.String())
	}
	var preparedBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, prepared, &preparedBody)
	result := agentharness.ActionResult{
		ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-send"},
		ResultDigest: digestControl("bound-send-result"), MessageReceipt: "message-a-receipt",
	}
	complete := func(message controlplane.Message, key string) *httptest.ResponseRecorder {
		return controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "complete", "action_id": preparedBody.Action.ID, "result": result, "message": message,
		}), key)
	}
	first := complete(messageA, strings.Repeat("4", 32))
	if first.Code != http.StatusOK {
		t.Fatalf("send A complete = %d, body = %s", first.Code, first.Body.String())
	}
	afterFirst, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}

	exactRetry := complete(messageA, strings.Repeat("5", 32))
	if exactRetry.Code != http.StatusOK {
		t.Fatalf("send A retry = %d, body = %s", exactRetry.Code, exactRetry.Body.String())
	}
	messageB := messageA
	messageB.ID = "message-b"
	messageB.Digest = digestControl("message-b")
	drifted := complete(messageB, strings.Repeat("6", 32))
	assertControlError(t, drifted, http.StatusConflict, "idempotency_conflict")

	final, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Messages) != 1 || final.Messages[messageA.ID] != messageA || final.Messages[messageB.ID].ID != "" {
		t.Fatalf("messages after retry drift = %#v", final.Messages)
	}
	if final.Run.EventCursor != afterFirst.Run.EventCursor || len(final.Events) != len(afterFirst.Events) {
		t.Fatalf("event stream mutated: before = %d/%d, after = %d/%d", afterFirst.Run.EventCursor, len(afterFirst.Events), final.Run.EventCursor, len(final.Events))
	}
}

func TestRouteSendRejectsForgedAcknowledgementAndReplaysAfterAcknowledgementRestart(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := fixture.service.PrepareAction(
		context.Background(), "control-1", attempt.ID, agentharness.ActionDispatch, digestControl("dispatch-send-ack-replay"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CompleteAction(context.Background(), "control-1", dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-send-ack-replay"},
		ResultDigest: digestControl("dispatch-send-ack-replay-result"),
	}); err != nil {
		t.Fatal(err)
	}

	message := controlplane.Message{
		ID: "message-send-ack-replay", FromTaskID: controlplane.ParentAddress, ToTaskID: "task-a",
		Kind: controlplane.MessageSteering, Digest: digestControl("message-send-ack-replay"),
	}
	path := "/v1/control-runs/control-1/attempts/" + attempt.ID + "/messages"
	forged := message
	forged.Acknowledged = true
	beforeForged, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	forgedResponse := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "prepare", "kind": "send",
		"request_digest": digestControlSendIncludingAcknowledged(forged), "message": forged,
	}), strings.Repeat("h", 32))
	assertControlError(t, forgedResponse, http.StatusBadRequest, "invalid_request")
	afterForged, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterForged.Actions) != len(beforeForged.Actions) ||
		afterForged.Run.EventCursor != beforeForged.Run.EventCursor || len(afterForged.Events) != len(beforeForged.Events) {
		t.Fatalf("forged acknowledgement mutated action state: before=%#v after=%#v", beforeForged, afterForged)
	}

	prepared := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "prepare", "kind": "send", "request_digest": digestControlSend(message), "message": message,
	}), strings.Repeat("i", 32))
	if prepared.Code != http.StatusAccepted {
		t.Fatalf("send prepare = %d, body = %s", prepared.Code, prepared.Body.String())
	}
	var preparedBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, prepared, &preparedBody)
	result := agentharness.ActionResult{
		ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-send-ack-replay"},
		ResultDigest: digestControl("send-ack-replay-result"), MessageReceipt: "send-ack-replay-receipt",
	}
	completeBody := mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": preparedBody.Action.ID, "result": result, "message": message,
	})
	completed := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, completeBody, strings.Repeat("j", 32))
	if completed.Code != http.StatusOK {
		t.Fatalf("send complete = %d, body = %s", completed.Code, completed.Body.String())
	}
	if _, err := fixture.service.AcknowledgeMessage(context.Background(), "control-1", message.ID, message.ToTaskID); err != nil {
		t.Fatal(err)
	}
	beforeReplay, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}

	restartedService := controlplane.NewService(fixture.store, controlplane.WithClock(fixedControlNow))
	restartedHandler, err := controlhttp.New(controlhttp.Dependencies{
		Service: restartedService, Authenticator: fixture.authenticator,
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted := fixture
	restarted.service = restartedService
	restarted.handler = restartedHandler
	replayed := controlRequest(t, restarted, restarted.principal, http.MethodPost, path, completeBody, strings.Repeat("k", 32))
	if replayed.Code != http.StatusOK {
		t.Fatalf("acknowledged send replay = %d, body = %s", replayed.Code, replayed.Body.String())
	}
	var replayedBody struct {
		Message controlplane.Message `json:"message"`
	}
	decodeControlResponse(t, replayed, &replayedBody)
	if !replayedBody.Message.Acknowledged {
		t.Fatalf("replayed message = %#v, want acknowledged", replayedBody.Message)
	}
	afterReplay, err := restarted.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if !afterReplay.Messages[message.ID].Acknowledged ||
		afterReplay.Run.EventCursor != beforeReplay.Run.EventCursor || len(afterReplay.Events) != len(beforeReplay.Events) {
		t.Fatalf("acknowledged replay duplicated effects: before=%#v after=%#v", beforeReplay, afterReplay)
	}

	changed := message
	changed.Digest = digestControl("message-send-ack-replay-changed")
	changedResponse := controlRequest(t, restarted, restarted.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": preparedBody.Action.ID, "result": result, "message": changed,
	}), strings.Repeat("l", 32))
	assertControlError(t, changedResponse, http.StatusConflict, "idempotency_conflict")
	afterChanged, err := restarted.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if afterChanged.Messages[message.ID] != afterReplay.Messages[message.ID] ||
		afterChanged.Run.EventCursor != afterReplay.Run.EventCursor || len(afterChanged.Events) != len(afterReplay.Events) {
		t.Fatalf("changed immutable retry mutated state: before=%#v after=%#v", afterReplay, afterChanged)
	}
}

func TestMalformedBoundSendDoesNotCompleteActionOrStrandCorrectedMessage(t *testing.T) {
	fixture := newControlFixture(t)
	createControl(t, fixture)
	attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := fixture.service.PrepareAction(context.Background(), "control-1", attempt.ID, agentharness.ActionDispatch, digestControl("dispatch-for-malformed-send"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CompleteAction(context.Background(), "control-1", dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-child-malformed-send"},
		Cursor: "cursor-1", CursorSequence: 1, ResultDigest: digestControl("dispatch-malformed-send-result"),
	}); err != nil {
		t.Fatal(err)
	}

	malformed := controlplane.Message{
		FromTaskID: controlplane.ParentAddress, ToTaskID: "task-a",
		Kind: controlplane.MessageSteering, Digest: digestControl("malformed-bound-message"),
	}
	stranded, err := fixture.service.PrepareAction(
		context.Background(), "control-1", attempt.ID, agentharness.ActionSend, digestControlSend(malformed),
	)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/control-runs/control-1/attempts/" + attempt.ID + "/messages"
	malformedResult := agentharness.ActionResult{
		ActionID: stranded.ID, RuntimeWorkIDs: []string{"runtime-child-malformed-send"},
		Cursor: "cursor-2", CursorSequence: 2, ResultDigest: digestControl("malformed-send-result"),
		MessageReceipt: "malformed-send-receipt",
	}
	rejected := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": stranded.ID, "result": malformedResult, "message": malformed,
	}), strings.Repeat("7", 32))
	assertControlError(t, rejected, http.StatusBadRequest, "invalid_request")
	after, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Actions[stranded.ID], before.Actions[stranded.ID]) ||
		!reflect.DeepEqual(after.Attempts[attempt.ID], before.Attempts[attempt.ID]) ||
		after.Run.EventCursor != before.Run.EventCursor || len(after.Events) != len(before.Events) ||
		!reflect.DeepEqual(after.Messages, before.Messages) {
		t.Fatalf("malformed send mutated target state: before = %#v, after = %#v", before, after)
	}

	corrected := malformed
	corrected.ID = "message-corrected"
	prepared := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "prepare", "kind": "send", "request_digest": digestControlSend(corrected), "message": corrected,
	}), strings.Repeat("8", 32))
	if prepared.Code != http.StatusAccepted {
		t.Fatalf("corrected send prepare = %d, body = %s", prepared.Code, prepared.Body.String())
	}
	var preparedBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, prepared, &preparedBody)
	completed := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": preparedBody.Action.ID,
		"result": agentharness.ActionResult{
			ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-malformed-send"},
			Cursor: "cursor-2", CursorSequence: 2, ResultDigest: digestControl("corrected-send-result"),
			MessageReceipt: "corrected-send-receipt",
		},
		"message": corrected,
	}), strings.Repeat("9", 32))
	if completed.Code != http.StatusOK {
		t.Fatalf("corrected send complete = %d, body = %s", completed.Code, completed.Body.String())
	}
	final, err := fixture.service.Load(context.Background(), "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if final.Actions[stranded.ID].Completed || !final.Actions[preparedBody.Action.ID].Completed ||
		len(final.Messages) != 1 || final.Messages[corrected.ID] != corrected {
		t.Fatalf("corrected send state = %#v", final)
	}
}

func TestRouteEvidenceAndNativeFanoutAggregateClose(t *testing.T) {
	fixture := newControlFixture(t)
	graph := validControlGraph()
	graph.Tasks[0] = validControlTask("task-a", "service", agentharness.HarnessNativeParallel)
	run := validControlRun()
	run.PrincipalID = fixture.principal.CredentialID
	if _, err := fixture.service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.service.CreateAttempt(context.Background(), run.ID, "task-a", completeControlCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := fixture.service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digestControl("native-dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CompleteAction(context.Background(), run.ID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, ResultDigest: digestControl("native-dispatch-result"),
	}); err != nil {
		t.Fatal(err)
	}

	evidence := controlplane.Evidence{
		ID: "evidence-native", TaskID: "task-a", AttemptID: attempt.ID,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		OwnedPathsDigest: digestControl("native-owned"),
		Tests: []controlplane.TestEvidence{{
			CommandDigest: digestControl("native-command"), ResultDigest: digestControl("native-result"), Passed: true,
		}},
	}
	evidencePath := "/v1/control-runs/control-1/evidence"
	recorded := controlRequest(t, fixture, fixture.principal, http.MethodPost, evidencePath, mustControlJSON(t, map[string]any{
		"operation": "record", "evidence": evidence,
	}), strings.Repeat("t", 32))
	if recorded.Code != http.StatusOK {
		t.Fatalf("record evidence = %d, body = %s", recorded.Code, recorded.Body.String())
	}
	changedEvidence := evidence
	changedEvidence.HeadSHA = strings.Repeat("c", 40)
	changed := controlRequest(t, fixture, fixture.principal, http.MethodPost, evidencePath, mustControlJSON(t, map[string]any{
		"operation": "record", "evidence": changedEvidence,
	}), strings.Repeat("t", 32))
	assertControlError(t, changed, http.StatusConflict, "idempotency_conflict")
	reference := controlplane.EvidenceRef{ID: evidence.ID, Digest: controlplane.EvidenceDigest(evidence)}
	attached := controlRequest(t, fixture, fixture.principal, http.MethodPost, evidencePath, mustControlJSON(t, map[string]any{
		"operation": "attach_terminal", "attempt_id": attempt.ID, "reference": reference,
	}), strings.Repeat("u", 32))
	if attached.Code != http.StatusOK {
		t.Fatalf("attach evidence = %d, body = %s", attached.Code, attached.Body.String())
	}
	dispositioned := controlRequest(t, fixture, fixture.principal, http.MethodPost, evidencePath, mustControlJSON(t, map[string]any{
		"operation": "disposition", "attempt_id": attempt.ID,
		"disposition": controlplane.Disposition{Kind: controlplane.DispositionIntegrated, EvidenceID: evidence.ID},
	}), strings.Repeat("v", 32))
	if dispositioned.Code != http.StatusOK {
		t.Fatalf("disposition = %d, body = %s", dispositioned.Code, dispositioned.Body.String())
	}

	closePath := "/v1/control-runs/control-1/attempts/" + attempt.ID + "/close"
	closePrepare := controlRequest(t, fixture, fixture.principal, http.MethodPost, closePath, mustControlJSON(t, map[string]any{
		"operation": "prepare", "request_digest": digestControl("native-close"),
	}), strings.Repeat("w", 32))
	var closePrepareBody struct {
		Action controlplane.LifecycleAction `json:"action"`
	}
	decodeControlResponse(t, closePrepare, &closePrepareBody)
	if closePrepare.Code != http.StatusAccepted || closePrepareBody.Action.Kind != agentharness.ActionClose {
		t.Fatalf("close prepare = %d %#v", closePrepare.Code, closePrepareBody)
	}
	closeComplete := controlRequest(t, fixture, fixture.principal, http.MethodPost, closePath, mustControlJSON(t, map[string]any{
		"operation": "complete", "action_id": closePrepareBody.Action.ID,
		"result": agentharness.ActionResult{
			ActionID: closePrepareBody.Action.ID, ResultDigest: digestControl("native-close-result"),
			CloseEvidence: agentharness.CloseEvidence{
				Kind: agentharness.CloseAggregate, Receipt: "aggregate-receipt", Digest: digestControl("native-aggregate"),
			},
		},
	}), strings.Repeat("x", 32))
	if closeComplete.Code != http.StatusOK {
		t.Fatalf("close complete = %d, body = %s", closeComplete.Code, closeComplete.Body.String())
	}
	snapshot, err := fixture.service.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	closed := snapshot.Attempts[attempt.ID]
	if closed.State != controlplane.AttemptCompleted || closed.CloseEvidence.Kind != controlplane.CloseAggregate || closed.CloseEvidence.Receipt != "aggregate-receipt" {
		t.Fatalf("native close = %#v", closed.CloseEvidence)
	}
}

func TestRouteInterruptLifecycleAndLocalClose(t *testing.T) {
	t.Run("interrupt persistent attempt", func(t *testing.T) {
		fixture := newControlFixture(t)
		createControl(t, fixture)
		attempt, err := fixture.service.CreateAttempt(context.Background(), "control-1", "task-a", completeControlCapabilities())
		if err != nil {
			t.Fatal(err)
		}
		dispatch, err := fixture.service.PrepareAction(context.Background(), "control-1", attempt.ID, agentharness.ActionDispatch, digestControl("dispatch-for-interrupt"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.CompleteAction(context.Background(), "control-1", dispatch.ID, agentharness.ActionResult{
			ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-child-interrupt"}, ResultDigest: digestControl("dispatch-interrupt-result"),
		}); err != nil {
			t.Fatal(err)
		}
		path := "/v1/control-runs/control-1/attempts/" + attempt.ID + "/interrupt"
		prepared := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "prepare", "request_digest": digestControl("interrupt"),
		}), strings.Repeat("y", 32))
		var preparedBody struct {
			Action controlplane.LifecycleAction `json:"action"`
		}
		decodeControlResponse(t, prepared, &preparedBody)
		if prepared.Code != http.StatusAccepted || preparedBody.Action.Kind != agentharness.ActionInterrupt {
			t.Fatalf("interrupt prepare = %d %#v", prepared.Code, preparedBody)
		}
		reusedPrepare := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "prepare", "request_digest": digestControl("interrupt"),
		}), strings.Repeat("y", 32))
		var reusedPrepareBody struct {
			Action controlplane.LifecycleAction `json:"action"`
		}
		decodeControlResponse(t, reusedPrepare, &reusedPrepareBody)
		if reusedPrepare.Code != http.StatusOK || reusedPrepareBody.Action.ID != preparedBody.Action.ID {
			t.Fatalf("interrupt prepare reuse = %d %#v", reusedPrepare.Code, reusedPrepareBody)
		}
		completed := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "complete", "action_id": preparedBody.Action.ID,
			"result": agentharness.ActionResult{
				ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-interrupt"}, ResultDigest: digestControl("interrupt-result"),
			},
		}), strings.Repeat("z", 32))
		if completed.Code != http.StatusOK {
			t.Fatalf("interrupt complete = %d, body = %s", completed.Code, completed.Body.String())
		}
		repeated := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "complete", "action_id": preparedBody.Action.ID,
			"result": agentharness.ActionResult{
				ActionID: preparedBody.Action.ID, RuntimeWorkIDs: []string{"runtime-child-interrupt"}, ResultDigest: digestControl("interrupt-result"),
			},
		}), strings.Repeat("z", 32))
		if repeated.Code != http.StatusOK {
			t.Fatalf("repeat interrupt = %d, body = %s", repeated.Code, repeated.Body.String())
		}
	})

	t.Run("close local attempt", func(t *testing.T) {
		fixture := newControlFixture(t)
		graph := validControlGraph()
		graph.Tasks[0] = validControlTask("task-a", "service", agentharness.LocalSequential)
		run := validControlRun()
		run.PrincipalID = fixture.principal.CredentialID
		if _, err := fixture.service.Create(context.Background(), run, graph); err != nil {
			t.Fatal(err)
		}
		attempt, err := fixture.service.CreateAttempt(context.Background(), run.ID, "task-a", completeControlCapabilities())
		if err != nil {
			t.Fatal(err)
		}
		evidence := controlplane.Evidence{
			ID: "evidence-local", TaskID: "task-a", AttemptID: attempt.ID,
			BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), OwnedPathsDigest: digestControl("local-owned"),
			Tests: []controlplane.TestEvidence{{CommandDigest: digestControl("local-command"), ResultDigest: digestControl("local-result"), Passed: true}},
		}
		if _, err := fixture.service.RecordEvidence(context.Background(), run.ID, evidence); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.AttachTerminalEvidence(context.Background(), run.ID, attempt.ID, controlplane.EvidenceRef{
			ID: evidence.ID, Digest: controlplane.EvidenceDigest(evidence),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.SetDisposition(context.Background(), run.ID, attempt.ID, controlplane.Disposition{
			Kind: controlplane.DispositionIntegrated, EvidenceID: evidence.ID,
		}); err != nil {
			t.Fatal(err)
		}
		path := "/v1/control-runs/control-1/attempts/" + attempt.ID + "/close"
		response := controlRequest(t, fixture, fixture.principal, http.MethodPost, path, mustControlJSON(t, map[string]any{
			"operation": "local",
			"close_evidence": controlplane.WorkCloseEvidence{
				Kind: controlplane.CloseInactive, Receipt: "inactive-receipt", Digest: digestControl("local-inactive"),
			},
		}), strings.Repeat("1", 32))
		if response.Code != http.StatusOK {
			t.Fatalf("local close = %d, body = %s", response.Code, response.Body.String())
		}
		snapshot, err := fixture.service.Load(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Attempts[attempt.ID].CloseEvidence.Kind != controlplane.CloseInactive {
			t.Fatalf("local close evidence = %#v", snapshot.Attempts[attempt.ID].CloseEvidence)
		}
	})
}

func TestNewRejectsMissingControlDependenciesAndUnauthenticatedContext(t *testing.T) {
	fixture := newControlFixture(t)
	if _, err := controlhttp.New(controlhttp.Dependencies{Service: fixture.service}); err == nil {
		t.Fatal("New accepted missing authenticator")
	}
	if _, err := controlhttp.New(controlhttp.Dependencies{Authenticator: fixture.authenticator}); err == nil {
		t.Fatal("New accepted missing service")
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	assertControlError(t, response, http.StatusUnauthorized, "unauthenticated")
}

func TestRouteControlCreateCannotBypassTaskCreateScope(t *testing.T) {
	actions := []string{
		"submit:artifact", "read", "cancel", "control:create", "work:dispatch",
		"work:observe", "work:send", "work:wait", "work:interrupt", "work:close",
		"evidence:write", "control:close",
	}
	authenticator, principal, _ := controlAuthenticatorWithActions(t, actions)
	service := controlplane.NewService(controlmock.NewStore(), controlplane.WithClock(fixedControlNow))
	handler, err := controlhttp.New(controlhttp.Dependencies{Service: service, Authenticator: authenticator})
	if err != nil {
		t.Fatal(err)
	}
	fixture := controlFixture{handler: handler, service: service, authenticator: authenticator, principal: principal}
	response := controlRequest(t, fixture, principal, http.MethodPost, "/v1/control-runs", createControlBody(t, validControlRun(), validControlGraph()), strings.Repeat("2", 32))
	assertControlError(t, response, http.StatusForbidden, "forbidden")
}

func newControlFixture(t *testing.T) controlFixture {
	t.Helper()
	store := controlmock.NewStore()
	service := controlplane.NewService(store, controlplane.WithClock(fixedControlNow))
	authenticator, principal, foreign := controlAuthenticator(t)
	handler, err := controlhttp.New(controlhttp.Dependencies{Service: service, Authenticator: authenticator})
	if err != nil {
		t.Fatal(err)
	}
	return controlFixture{handler: handler, service: service, store: store, authenticator: authenticator, principal: principal, foreign: foreign}
}

func controlAuthenticator(t *testing.T) (*auth.Authenticator, submission.Principal, submission.Principal) {
	t.Helper()
	return controlAuthenticatorWithActions(t, []string{
		"submit:artifact", "read", "cancel", "control:create", "task:create", "work:dispatch", "work:observe",
		"work:send", "work:wait", "work:interrupt", "work:close", "evidence:write", "control:close",
	})
}

func controlAuthenticatorWithActions(t *testing.T, actions []string) (*auth.Authenticator, submission.Principal, submission.Principal) {
	t.Helper()
	secondSecret := bytes.Repeat([]byte{0x33}, 32)
	secondToken := "paje_v1_other02." + base64.RawURLEncoding.EncodeToString(secondSecret)
	credential := func(id, token, subject string) map[string]any {
		secret, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
		sum := sha256.Sum256(secret)
		return map[string]any{
			"id": id, "secret_hash": hex.EncodeToString(sum[:]), "subject": subject,
			"user_id": subject, "app_id": "service", "repositories": []string{"https://github.com/example/service.git"},
			"actions":   actions,
			"harnesses": []string{"codex"}, "projects": []string{"service", "docs"},
			"communication_edges": []map[string]string{{"from": "service", "to": "docs"}},
			"max_depth":           0, "expires_at": "2027-01-01T00:00:00Z",
		}
	}
	raw := mustControlJSON(t, map[string]any{
		"schema_version": 1,
		"credentials": []map[string]any{
			credential("codex01", controlToken, "codex@example.com"),
			credential("other02", secondToken, "other@example.com"),
		},
	})
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.LoadPolicy(path, fixedControlNow)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authenticator.Authenticate(controlToken)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := authenticator.Authenticate(secondToken)
	if err != nil {
		t.Fatal(err)
	}
	return authenticator, principal, foreign
}

func validControlRun() controlplane.ControlRun {
	return controlplane.ControlRun{
		SchemaVersion: controlplane.SchemaVersion, ID: "control-1",
		GoalDigest: digestControl("goal"), GraphRevision: 1, Status: controlplane.StatusOpen,
	}
}

func validControlGraph() controlplane.TaskGraph {
	return controlplane.TaskGraph{
		SchemaVersion: controlplane.SchemaVersion, ControlRunID: "control-1", Revision: 1,
		Tasks:            []controlplane.Task{validControlTask("task-a", "service", agentharness.PersistentSession)},
		IntegrationOrder: []string{"task-a"},
		CombinedGates:    []controlplane.Gate{{ID: "combined", Digest: digestControl("combined")}},
	}
}

func validControlTask(id, projectID string, primitive agentharness.Primitive) controlplane.Task {
	placement := controlplane.ExecutionPlacement{
		ParallelismPrimitive: primitive, ExecutionPlacement: "local_control",
		PlacementRationale: "bounded exact placement", LifecycleOwner: "parent", Fallback: "local_sequential",
		CapabilityRequirements: append([]agentharness.Capability(nil), agentharness.RequiredCapabilities(primitive)...),
	}
	if primitive == agentharness.PersistentSession {
		placement.ExecutionPlacement = "worktree_backed_codex_task"
		placement.Fallback = "block"
	} else if primitive == agentharness.LocalSequential {
		placement.Fallback = "block"
	}
	return controlplane.Task{
		ID: id, Goal: "Implement " + id,
		Projects: []controlplane.ProjectRef{{
			ID: projectID, Repository: "https://github.com/example/service.git", BaseRef: "main",
			BaseSHA: strings.Repeat("a", 40), WorkspaceScope: "workspace-" + projectID,
			CredentialScope: "credential-" + projectID, MailboxNamespace: "mail-" + projectID,
			EvidenceNamespace: "evidence-" + projectID,
		}},
		Ownership: controlplane.Ownership{Mutable: []string{"internal/" + id + "/**"}},
		Placement: placement, FrozenInputs: []controlplane.FrozenInput{{ID: "spec", Digest: digestControl("spec-" + id)}},
		Acceptance: []controlplane.Gate{{ID: "test", Digest: digestControl("test-" + id)}}, State: controlplane.TaskReady,
	}
}

func completeControlCapabilities() agentharness.CapabilitySnapshot {
	return agentharness.CapabilitySnapshot{
		HarnessID: "codex",
		Primitives: map[agentharness.Primitive]agentharness.PrimitiveCapabilities{
			agentharness.PersistentSession: {
				Primitive: agentharness.PersistentSession,
				Capabilities: agentharness.CapabilitySet(
					agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait,
					agentharness.CapRuntimeIdentity, agentharness.CapAcknowledge, agentharness.CapSend,
					agentharness.CapCallback, agentharness.CapCursor, agentharness.CapInterrupt,
					agentharness.CapIdempotency, agentharness.CapRestart, agentharness.CapArchive, agentharness.CapIsolation,
				),
				ConcurrencyLimit: 2,
			},
			agentharness.LocalSequential: {
				Primitive: agentharness.LocalSequential, Capabilities: agentharness.CapabilitySet(agentharness.CapLocal), ConcurrencyLimit: 1,
			},
			agentharness.EphemeralSubagent: {
				Primitive: agentharness.EphemeralSubagent,
				Capabilities: agentharness.CapabilitySet(
					agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait,
					agentharness.CapRuntimeIdentity, agentharness.CapRuntimeClose,
				),
				ConcurrencyLimit: 4,
			},
			agentharness.HarnessNativeParallel: {
				Primitive: agentharness.HarnessNativeParallel,
				Capabilities: agentharness.CapabilitySet(
					agentharness.CapDispatch, agentharness.CapWait, agentharness.CapInterrupt,
					agentharness.CapDeterministicAggregation,
				),
				ConcurrencyLimit: 8,
			},
		},
	}
}

func validControlCallback(attemptID string) controlplane.CompletionCallback {
	return controlplane.CompletionCallback{
		AttemptID: attemptID, RuntimeChildID: "runtime-child-1", Status: controlplane.CallbackDone,
		Branch: "codex/task", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		OwnedPathsDigest: digestControl("owned"), TestsDigest: digestControl("tests"),
		RecommendedParentAction: "integrate exact head",
	}
}

func createControl(t *testing.T, fixture controlFixture) {
	t.Helper()
	response := controlRequest(t, fixture, fixture.principal, http.MethodPost, "/v1/control-runs", createControlBody(t, validControlRun(), validControlGraph()), strings.Repeat("a", 32))
	if response.Code != http.StatusAccepted {
		t.Fatalf("create control = %d, body = %s", response.Code, response.Body.String())
	}
}

func createControlBody(t *testing.T, run controlplane.ControlRun, graph controlplane.TaskGraph) []byte {
	return mustControlJSON(t, map[string]any{"run": run, "graph": graph})
}

func controlRequest(t *testing.T, fixture controlFixture, principal submission.Principal, method, path string, body []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request = request.WithContext(auth.ContextWithPrincipal(request.Context(), principal))
	if body != nil && method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	return response
}

func mustControlJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeControlResponse(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
}

func assertControlError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, status, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeControlResponse(t, response, &body)
	if body.Error.Code != code || response.Body.Len() > 512 {
		t.Fatalf("error body = %s", response.Body.String())
	}
}

func digestControl(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestControlSend(message controlplane.Message) string {
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
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestControlSendIncludingAcknowledged(message controlplane.Message) string {
	raw, _ := json.Marshal(message)
	sum := sha256.Sum256(append([]byte("paje-control-send-v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestControlObserve(runID, attemptID, afterCursor string, afterSequence uint64) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"paje-control-observe-v1", runID, attemptID, afterCursor,
		strconv.FormatUint(afterSequence, 10),
	}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func fixedControlNow() time.Time {
	return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func firstAttemptID(snapshot controlplane.Snapshot) string {
	for id := range snapshot.Attempts {
		return id
	}
	return ""
}

func assertControlMutationStateUnchanged(t *testing.T, before, after controlplane.Snapshot) {
	t.Helper()
	if after.Graph.Tasks[0].State != before.Graph.Tasks[0].State ||
		len(after.Attempts) != len(before.Attempts) || len(after.Actions) != len(before.Actions) ||
		after.Run.EventCursor != before.Run.EventCursor || len(after.Events) != len(before.Events) {
		t.Fatalf("control mutation state changed: before = %#v, after = %#v", before, after)
	}
}
