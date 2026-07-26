package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
)

func TestScopeSeparatesEqualProjectAndCredentialIDsAcrossRuns(t *testing.T) {
	t.Parallel()

	runA := mustRunScope(t, "installation-1", "run-a")
	runB := mustRunScope(t, "installation-1", "run-b")
	projectA, err := NewProjectScope(
		runA,
		"project",
		"https://github.com/example/service.git",
		"0123456789abcdef0123456789abcdef01234567",
	)
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := NewProjectScope(
		runB,
		"project",
		"https://github.com/example/service.git",
		"0123456789abcdef0123456789abcdef01234567",
	)
	if err != nil {
		t.Fatal(err)
	}
	if projectA.Key() == projectB.Key() {
		t.Fatal("equal project-local IDs in different runs share an identity")
	}

	credentialA, err := NewCredentialScope(
		projectA,
		"verification",
		"opaque-provider-handle",
		mustDigest(t, map[string]string{"policy": "read-only"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	credentialB, err := NewCredentialScope(
		projectB,
		"verification",
		"opaque-provider-handle",
		mustDigest(t, map[string]string{"policy": "read-only"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentialA.ID() == credentialB.ID() {
		t.Fatal("equal opaque credential handles in different runs share an identity")
	}
	if credentialA.ID() == "opaque-provider-handle" {
		t.Fatal("credential scope exposes its provider handle as its identity")
	}
}

func TestScopeRejectsIncompleteOrUnboundIdentity(t *testing.T) {
	t.Parallel()

	if _, err := NewRunScope("", "run-a"); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("NewRunScope() error = %v, want invalid scope", err)
	}
	run := mustRunScope(t, "installation-1", "run-a")
	if _, err := NewProjectScope(run, "project", "", "0123456789abcdef0123456789abcdef01234567"); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("NewProjectScope() error = %v, want invalid scope", err)
	}
	project, err := NewProjectScope(
		run,
		"project",
		"https://github.com/example/service.git",
		"0123456789abcdef0123456789abcdef01234567",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCredentialScope(project, "verification", "clear token with spaces", mustDigest(t, "policy")); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("NewCredentialScope() error = %v, want invalid scope", err)
	}
}

func TestPhaseAcceptsCanonicalProgressionAndRejectsSkippedTransition(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-phase")
	revision := uint64(0)
	phases := []TaskPhase{
		TaskDiscovered,
		TaskAuditingReadOnly,
		TaskReadyForOwnership,
		TaskOwned,
		TaskExecuting,
		TaskVerifying,
		TaskAccepted,
	}
	from := TaskPhase("")
	for index, to := range phases {
		result, err := service.TransitionTask(context.Background(), TaskPhaseRequest{
			Scope:            scope,
			OperationID:      operationID("phase", index),
			GraphRevision:    1,
			ExpectedRevision: revision,
			TaskID:           "task",
			AttemptID:        "attempt",
			From:             from,
			To:               to,
		})
		if err != nil {
			t.Fatalf("TransitionTask(%s -> %s): %v", from, to, err)
		}
		revision = result.Projection.Revision
		state, ok := result.Projection.Task("task")
		if !ok || state.Phase != to {
			t.Fatalf("task after transition = %#v, %v, want phase %s", state, ok, to)
		}
		from = to
	}

	invalidService, _ := newTestServiceWithInstallation(t, "installation-2")
	invalidScope := mustRunScope(t, "installation-2", "run-skipped")
	_, err := invalidService.TransitionTask(context.Background(), TaskPhaseRequest{
		Scope:            invalidScope,
		OperationID:      "skip-to-executing",
		GraphRevision:    1,
		ExpectedRevision: 0,
		TaskID:           "task",
		AttemptID:        "attempt",
		To:               TaskExecuting,
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("skipped TransitionTask() error = %v, want invalid transition", err)
	}
}

func TestPhaseExceptionalTransitionsAreExplicitAndTerminal(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-exceptional")
	discovered, err := service.TransitionTask(context.Background(), TaskPhaseRequest{
		Scope: scope, OperationID: "discover", GraphRevision: 1,
		ExpectedRevision: 0, TaskID: "task", AttemptID: "attempt", To: TaskDiscovered,
	})
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := service.TransitionTask(context.Background(), TaskPhaseRequest{
		Scope: scope, OperationID: "defer", GraphRevision: 1,
		ExpectedRevision: discovered.Projection.Revision, TaskID: "task", AttemptID: "attempt",
		From: TaskDiscovered, To: TaskDeferred,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.TransitionTask(context.Background(), TaskPhaseRequest{
		Scope: scope, OperationID: "fail", GraphRevision: 1,
		ExpectedRevision: deferred.Projection.Revision, TaskID: "task", AttemptID: "attempt",
		From: TaskDeferred, To: TaskFailed,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.TransitionTask(context.Background(), TaskPhaseRequest{
		Scope: scope, OperationID: "reopen", GraphRevision: 1,
		ExpectedRevision: failed.Projection.Revision, TaskID: "task", AttemptID: "attempt",
		From: TaskFailed, To: TaskReadyForOwnership,
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal reopen error = %v, want invalid transition", err)
	}
}

func TestPhaseRunFreezeIsJournalBackedAndObservationIsInert(t *testing.T) {
	t.Parallel()

	service, store := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-freeze")
	result, err := service.TransitionRun(context.Background(), RunPhaseRequest{
		Scope: scope, OperationID: "freeze", GraphRevision: 1, ExpectedRevision: 0,
		From: RunActive, To: RunFrozenSecurity, Authority: "security-controller",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.RunPhase != RunFrozenSecurity {
		t.Fatalf("run phase = %s, want %s", result.Projection.RunPhase, RunFrozenSecurity)
	}
	before := feedLength(t, store)
	observed, err := service.Observe(context.Background(), ObservationRequest{
		Scope: scope, Source: ObservationProvider, Status: "terminal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.RunPhase != RunFrozenSecurity || observed.Revision != result.Projection.Revision {
		t.Fatalf("provider observation mutated projection: %#v", observed)
	}
	if after := feedLength(t, store); after != before {
		t.Fatalf("provider observation appended %d journal events", after-before)
	}
}

func TestPhaseResumeReturnsDeferredRunToJournalDerivedQuiescent(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	scope := mustRunScope(t, "installation-1", "run-resume-quiescent")
	revision := deferTask(t, service, scope)
	registered, err := service.RegisterGate(context.Background(), RegisterGateRequest{
		Scope: scope, OperationID: "register", GraphRevision: 1,
		ExpectedRevision: revision,
		Gate: PendingWorkGate{
			ID: "gate", TaskID: "task", Kind: GateTimeNotBefore,
			ResolverAuthority: "clock",
			WakeAt:            time.Date(2026, 7, 26, 13, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := service.TransitionRun(context.Background(), RunPhaseRequest{
		Scope: scope, OperationID: "freeze", GraphRevision: 1,
		ExpectedRevision: registered.Projection.Revision,
		From:             RunQuiescent,
		To:               RunFrozenSecurity,
		Authority:        "security-controller",
	})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := service.TransitionRun(context.Background(), RunPhaseRequest{
		Scope: scope, OperationID: "resume", GraphRevision: 1,
		ExpectedRevision: frozen.Projection.Revision,
		From:             RunFrozenSecurity,
		To:               RunActive,
		Authority:        "security-controller",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Projection.RunPhase != RunQuiescent {
		t.Fatalf("resumed run phase = %s, want journal-derived %s", resumed.Projection.RunPhase, RunQuiescent)
	}
}

func mustRunScope(t *testing.T, installationID, runID string) RunScope {
	t.Helper()
	scope, err := NewRunScope(installationID, runID)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func mustDigest(t *testing.T, value any) string {
	t.Helper()
	digest, err := journal.Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func operationID(prefix string, index int) string {
	return prefix + "-" + time.Unix(int64(index), 0).UTC().Format("150405")
}
