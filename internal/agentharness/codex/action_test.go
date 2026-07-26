package codex_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/agentharness/codex"
)

func TestTwoPhaseActionIsStableSemanticAndExactlyBound(t *testing.T) {
	t.Parallel()

	request := codex.PrepareRequest{
		ControlRunID: "control-1", TaskID: "task-1", AttemptID: "attempt-1",
		GraphRevision: 3, Primitive: agentharness.PersistentSession,
		Kind: agentharness.ActionDispatch, RequestDigest: digest("request"),
		ParentRuntimeID: "parent-1",
	}
	first, err := codex.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := codex.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.ActionID == "" {
		t.Fatalf("Prepare() is not stable: %#v %#v", first, second)
	}
	encoded := first.CanonicalJSON()
	for _, forbidden := range []string{"send_message_to_thread", "codex_app__", "/bin/sh", "exec_command"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("semantic action leaked runtime tool or command %q: %s", forbidden, encoded)
		}
	}

	result := agentharness.ActionResult{
		ActionID: first.ActionID, RuntimeWorkIDs: []string{"runtime-child-1"},
		Cursor: "cursor-1", CursorSequence: 1, ResultDigest: digest("result"),
	}
	completed, err := codex.Complete(first, result, persistentCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if completed.ActionID != first.ActionID || completed.RuntimeWorkIDs[0] != "runtime-child-1" {
		t.Fatalf("Complete() = %#v", completed)
	}
	result.ActionID = "foreign"
	if _, err := codex.Complete(first, result, persistentCapabilities()); !errors.Is(err, agentharness.ErrActionMismatch) {
		t.Fatalf("Complete(foreign action) error = %v, want ErrActionMismatch", err)
	}
}

func TestTwoPhaseActionRejectsInventedIdentityAndUnsupportedOperations(t *testing.T) {
	t.Parallel()

	document, err := codex.Prepare(codex.PrepareRequest{
		ControlRunID: "control-1", TaskID: "task-1", AttemptID: "attempt-1",
		GraphRevision: 1, Primitive: agentharness.LocalSequential,
		Kind: agentharness.ActionDispatch, RequestDigest: digest("request"),
	})
	if !errors.Is(err, agentharness.ErrUnsupportedOperation) || document.ActionID != "" {
		t.Fatalf("Prepare(local dispatch) = %#v, %v", document, err)
	}

	document, err = codex.Prepare(codex.PrepareRequest{
		ControlRunID: "control-1", TaskID: "task-1", AttemptID: "attempt-1",
		GraphRevision: 1, Primitive: agentharness.EphemeralSubagent,
		Kind: agentharness.ActionClose, RequestDigest: digest("request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	caps := agentharness.PrimitiveCapabilities{
		Primitive: agentharness.EphemeralSubagent,
		Capabilities: agentharness.CapabilitySet(
			agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait,
			agentharness.CapRuntimeClose,
		),
	}
	if _, err := codex.Complete(document, agentharness.ActionResult{
		ActionID: document.ActionID, RuntimeWorkIDs: []string{"derived-from-worktree"},
		ResultDigest: digest("result"), CloseEvidence: agentharness.CloseEvidence{Kind: agentharness.CloseRuntime, Receipt: "closed"},
	}, caps); !errors.Is(err, agentharness.ErrUnexpectedRuntimeIdentity) {
		t.Fatalf("Complete(invented identity) error = %v, want ErrUnexpectedRuntimeIdentity", err)
	}
	if _, err := codex.Complete(document, agentharness.ActionResult{
		ActionID: document.ActionID, ResultDigest: digest("result"),
		CloseEvidence: agentharness.CloseEvidence{Kind: agentharness.CloseRuntime, Receipt: "closed"},
	}, caps); !errors.Is(err, agentharness.ErrActionMismatch) {
		t.Fatalf("Complete(close without evidence digest) error = %v, want ErrActionMismatch", err)
	}
}

func TestTwoPhasePersistentFollowupRejectsForeignRuntimeIdentity(t *testing.T) {
	t.Parallel()

	document, err := codex.Prepare(codex.PrepareRequest{
		ControlRunID: "control-1", TaskID: "task-1", AttemptID: "attempt-1",
		GraphRevision: 1, Primitive: agentharness.PersistentSession,
		Kind: agentharness.ActionAcknowledge, RequestDigest: digest("acknowledge"),
		RuntimeWorkIDs: []string{"runtime-child-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codex.Complete(document, agentharness.ActionResult{
		ActionID: document.ActionID, RuntimeWorkIDs: []string{"runtime-child-foreign"},
		ResultDigest: digest("result"),
	}, persistentCapabilities()); !errors.Is(err, agentharness.ErrActionMismatch) {
		t.Fatalf("Complete(foreign persistent child) error = %v, want ErrActionMismatch", err)
	}
}

func TestPersistentCapabilitiesRequireRestartIsolationAndIdempotency(t *testing.T) {
	t.Parallel()

	for _, missing := range []agentharness.Capability{
		agentharness.CapRestart,
		agentharness.CapIsolation,
		agentharness.CapIdempotency,
	} {
		t.Run(missing, func(t *testing.T) {
			capabilities := persistentCapabilities()
			delete(capabilities.Capabilities, missing)
			if err := capabilities.Validate(); !errors.Is(err, agentharness.ErrInvalidCapabilities) {
				t.Fatalf("Validate(missing %s) error = %v, want ErrInvalidCapabilities", missing, err)
			}
		})
	}
}

func persistentCapabilities() agentharness.PrimitiveCapabilities {
	return agentharness.PrimitiveCapabilities{
		Primitive: agentharness.PersistentSession,
		Capabilities: agentharness.CapabilitySet(
			agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait,
			agentharness.CapRuntimeIdentity, agentharness.CapAcknowledge,
			agentharness.CapSend, agentharness.CapCallback, agentharness.CapCursor,
			agentharness.CapInterrupt, agentharness.CapArchive,
			agentharness.CapRestart, agentharness.CapIsolation, agentharness.CapIdempotency,
		),
	}
}

func digest(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}
