// Package contracttest defines reusable conformance tests for provider-neutral
// agent work lifecycle adapters.
package contracttest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/agentharness"
)

type Scenario string

const (
	ScenarioCapabilities       Scenario = "capabilities"
	ScenarioPersistent         Scenario = "persistent_session"
	ScenarioEphemeralIdentity  Scenario = "ephemeral_with_identity"
	ScenarioEphemeralAnonymous Scenario = "ephemeral_without_identity"
	ScenarioNativeFanout       Scenario = "native_fanout"
	ScenarioLocal              Scenario = "local_sequential"
	ScenarioUnsupported        Scenario = "unsupported_operation"
	ScenarioInvalidBinding     Scenario = "invalid_action_binding"
	ScenarioBoundedDiagnostic  Scenario = "bounded_diagnostic"
)

type Fixture struct {
	Harness      agentharness.AgentHarness
	Capabilities agentharness.CapabilitySnapshot
}

type Factory func(*testing.T, Scenario) Fixture

func Run(t *testing.T, factory Factory) {
	t.Helper()
	if factory == nil {
		t.Fatal("agent harness contract factory is required")
	}
	scenarios := []Scenario{
		ScenarioCapabilities, ScenarioPersistent, ScenarioEphemeralIdentity,
		ScenarioEphemeralAnonymous, ScenarioNativeFanout, ScenarioLocal,
		ScenarioUnsupported, ScenarioInvalidBinding, ScenarioBoundedDiagnostic,
	}
	for _, scenario := range scenarios {
		t.Run(string(scenario), func(t *testing.T) {
			fixture := factory(t, scenario)
			if fixture.Harness == nil {
				t.Fatal("nil harness")
			}
			switch scenario {
			case ScenarioCapabilities:
				runCapabilities(t, fixture)
			case ScenarioPersistent:
				runPersistent(t, fixture)
			case ScenarioEphemeralIdentity, ScenarioEphemeralAnonymous:
				runEphemeral(t, fixture, scenario == ScenarioEphemeralIdentity)
			case ScenarioNativeFanout:
				runNative(t, fixture)
			case ScenarioLocal:
				runLocal(t, fixture)
			case ScenarioUnsupported:
				runUnsupported(t, fixture)
			case ScenarioInvalidBinding:
				runInvalidBinding(t, fixture)
			case ScenarioBoundedDiagnostic:
				runBoundedDiagnostic(t, fixture)
			}
		})
	}
}

func runCapabilities(t *testing.T, fixture Fixture) {
	t.Helper()
	got, err := fixture.Harness.Capabilities(context.Background(), agentharness.Principal{ID: "principal"}, "all")
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Capabilities() invalid: %v", err)
	}
}

func runPersistent(t *testing.T, fixture Fixture) {
	t.Helper()
	request := dispatchRequest(agentharness.PersistentSession)
	dispatched, err := fixture.Harness.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched.ActionID != request.ActionID || len(dispatched.RuntimeWorkIDs) != 1 || dispatched.CursorSequence == 0 {
		t.Fatalf("persistent Dispatch() = %#v", dispatched)
	}
	changed := request
	changed.PromptDigest = digest("changed-prompt")
	if _, err := fixture.Harness.Dispatch(context.Background(), changed); !errors.Is(err, agentharness.ErrActionConflict) {
		t.Fatalf("Dispatch(changed request under same action) error = %v, want ErrActionConflict", err)
	}
	observed, err := fixture.Harness.Observe(context.Background(), agentharness.ObserveWorkRequest{
		ActionID: "observe-1", ControlRunID: request.ControlRunID, TaskID: request.TaskID,
		AttemptID: request.AttemptID, AfterCursor: dispatched.Cursor,
		AfterCursorSequence: dispatched.CursorSequence,
	})
	if err != nil || observed.NextCursor == "" || observed.NextCursorSequence <= dispatched.CursorSequence {
		t.Fatalf("Observe() = %#v, %v", observed, err)
	}
	message, err := fixture.Harness.Send(context.Background(), agentharness.SendWorkRequest{
		ActionID: "send-1", ControlRunID: request.ControlRunID, TaskID: request.TaskID,
		AttemptID: request.AttemptID, RuntimeWorkID: dispatched.RuntimeWorkIDs[0],
		MessageDigest: digest("message"),
	})
	if err != nil || message.Receipt == "" || message.CursorSequence <= observed.NextCursorSequence {
		t.Fatalf("Send() = %#v, %v", message, err)
	}
	waited, err := fixture.Harness.Wait(context.Background(), agentharness.WaitWorkRequest{
		ActionID: "wait-1", ControlRunID: request.ControlRunID,
		AttemptIDs: []string{request.AttemptID}, AfterCursor: observed.NextCursor,
		AfterCursorSequence: observed.NextCursorSequence,
		Timeout:             time.Second,
	})
	if err != nil || !waited.Terminal || waited.NextCursorSequence <= observed.NextCursorSequence {
		t.Fatalf("Wait() = %#v, %v", waited, err)
	}
	interrupt, err := fixture.Harness.Interrupt(context.Background(), agentharness.InterruptWorkRequest{
		ActionID: "interrupt-1", ControlRunID: request.ControlRunID, TaskID: request.TaskID,
		AttemptID: request.AttemptID, RuntimeWorkIDs: dispatched.RuntimeWorkIDs,
	})
	if err != nil || interrupt.Receipt == "" {
		t.Fatalf("Interrupt() = %#v, %v", interrupt, err)
	}
	for range 2 {
		if _, err := fixture.Harness.Close(context.Background(), agentharness.CloseWorkRequest{
			ActionID: "close-foreign", ControlRunID: request.ControlRunID, TaskID: request.TaskID,
			AttemptID: request.AttemptID, Primitive: request.Primitive,
			RuntimeWorkIDs: []string{"runtime-child-foreign"},
		}); !errors.Is(err, agentharness.ErrActionMismatch) {
			t.Fatalf("Close(foreign runtime ID) error = %v, want ErrActionMismatch", err)
		}
		closed, err := fixture.Harness.Close(context.Background(), agentharness.CloseWorkRequest{
			ActionID: "close-1", ControlRunID: request.ControlRunID, TaskID: request.TaskID,
			AttemptID: request.AttemptID, Primitive: request.Primitive,
			RuntimeWorkIDs: dispatched.RuntimeWorkIDs,
		})
		if err != nil || closed.Evidence.Kind != agentharness.CloseArchive || closed.Evidence.Receipt == "" {
			t.Fatalf("Close() = %#v, %v", closed, err)
		}
	}
}

func runEphemeral(t *testing.T, fixture Fixture, identity bool) {
	t.Helper()
	request := dispatchRequest(agentharness.EphemeralSubagent)
	result, err := fixture.Harness.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if identity != (len(result.RuntimeWorkIDs) == 1) {
		t.Fatalf("ephemeral identity = %#v, want returned %t", result.RuntimeWorkIDs, identity)
	}
	waited, err := fixture.Harness.Wait(context.Background(), agentharness.WaitWorkRequest{
		ActionID: "wait-ephemeral", ControlRunID: request.ControlRunID,
		AttemptIDs: []string{request.AttemptID}, Timeout: time.Second,
	})
	if err != nil || !waited.Terminal {
		t.Fatalf("Wait() = %#v, %v", waited, err)
	}
	closed, err := fixture.Harness.Close(context.Background(), agentharness.CloseWorkRequest{
		ActionID: "close-ephemeral", ControlRunID: request.ControlRunID,
		TaskID: request.TaskID, AttemptID: request.AttemptID, Primitive: request.Primitive,
		RuntimeWorkIDs: result.RuntimeWorkIDs,
	})
	if err != nil || closed.Evidence.Kind != agentharness.CloseRuntime {
		t.Fatalf("Close() = %#v, %v", closed, err)
	}
}

func runNative(t *testing.T, fixture Fixture) {
	t.Helper()
	request := dispatchRequest(agentharness.HarnessNativeParallel)
	request.ExpectedItemDigests = []string{digest("item-1"), digest("item-2")}
	result, err := fixture.Harness.Dispatch(context.Background(), request)
	if err != nil || len(result.RuntimeWorkIDs) != 0 {
		t.Fatalf("Dispatch() = %#v, %v", result, err)
	}
	if _, err := fixture.Harness.Close(context.Background(), agentharness.CloseWorkRequest{
		ActionID: "close-native-wrong", ControlRunID: request.ControlRunID,
		TaskID: request.TaskID, AttemptID: request.AttemptID, Primitive: request.Primitive,
		ResultDigests: []string{digest("wrong-1"), digest("wrong-2")},
	}); !errors.Is(err, agentharness.ErrActionMismatch) {
		t.Fatalf("Close(wrong aggregation) error = %v, want ErrActionMismatch", err)
	}
	closed, err := fixture.Harness.Close(context.Background(), agentharness.CloseWorkRequest{
		ActionID: "close-native", ControlRunID: request.ControlRunID,
		TaskID: request.TaskID, AttemptID: request.AttemptID, Primitive: request.Primitive,
		ResultDigests: request.ExpectedItemDigests,
	})
	if err != nil || closed.Evidence.Kind != agentharness.CloseAggregate {
		t.Fatalf("Close() = %#v, %v", closed, err)
	}
}

func runLocal(t *testing.T, fixture Fixture) {
	t.Helper()
	request := dispatchRequest(agentharness.LocalSequential)
	if _, err := fixture.Harness.Dispatch(context.Background(), request); !errors.Is(err, agentharness.ErrUnsupportedOperation) {
		t.Fatalf("local Dispatch() error = %v", err)
	}
}

func runUnsupported(t *testing.T, fixture Fixture) {
	t.Helper()
	_, err := fixture.Harness.Send(context.Background(), agentharness.SendWorkRequest{
		ActionID: "foreign-send", ControlRunID: "control-1", TaskID: "task-1",
		AttemptID: "attempt-1", MessageDigest: digest("message"),
	})
	if !errors.Is(err, agentharness.ErrUnsupportedOperation) {
		t.Fatalf("unsupported Send() error = %v", err)
	}
}

func runInvalidBinding(t *testing.T, fixture Fixture) {
	t.Helper()
	request := dispatchRequest(agentharness.PersistentSession)
	request.ActionID = ""
	if _, err := fixture.Harness.Dispatch(context.Background(), request); !errors.Is(err, agentharness.ErrInvalidRequest) {
		t.Fatalf("Dispatch(invalid binding) error = %v, want ErrInvalidRequest", err)
	}
}

func runBoundedDiagnostic(t *testing.T, fixture Fixture) {
	t.Helper()
	result, err := fixture.Harness.Dispatch(context.Background(), dispatchRequest(agentharness.EphemeralSubagent))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostic) > 1024 || strings.Contains(result.Diagnostic, "provider-secret") {
		t.Fatalf("unsafe diagnostic = %q", result.Diagnostic)
	}
}

func dispatchRequest(primitive agentharness.Primitive) agentharness.DispatchWorkRequest {
	return agentharness.DispatchWorkRequest{
		ActionID: "dispatch-1", ControlRunID: "control-1", TaskID: "task-1",
		AttemptID: "attempt-1", Primitive: primitive,
		RequestDigest: digest("dispatch-request"),
		ProjectRefIDs: []string{"project-1"}, PromptDigest: digest("prompt"),
		OwnershipDigest: digest("ownership"), FrozenInputDigest: digest("frozen"),
		Timeout: time.Second,
	}
}

func digest(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}
