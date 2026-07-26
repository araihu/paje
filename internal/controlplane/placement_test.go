package controlplane_test

import (
	"context"
	"errors"
	"testing"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane"
	controlmock "github.com/araihu/paje/internal/controlplane/mock"
)

func TestPlacementMapsEveryPrimitiveDeterministically(t *testing.T) {
	t.Parallel()

	capabilities := completeCapabilities()
	tests := []struct {
		name  string
		facts controlplane.TaskFacts
		want  agentharness.Primitive
	}{
		{"persistent", controlplane.TaskFacts{
			TaskID: "long", Mutating: true, NeedsIsolation: true, NeedsRestart: true,
			NeedsSteering: true, Ownership: []string{"internal/long/**"},
		}, agentharness.PersistentSession},
		{"ephemeral", controlplane.TaskFacts{
			TaskID: "review", ReadOnly: true, Short: true,
		}, agentharness.EphemeralSubagent},
		{"native", controlplane.TaskFacts{
			TaskID: "fanout", ReadOnly: true, Short: true, HomogeneousFanout: true, FanoutItems: 4,
		}, agentharness.HarnessNativeParallel},
		{"local", controlplane.TaskFacts{
			TaskID: "integrate", Mutating: true, SharedIntegrationFiles: true,
		}, agentharness.LocalSequential},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := controlplane.SelectPlacement(test.facts, capabilities, controlplane.Capacity{})
			if err != nil {
				t.Fatal(err)
			}
			if got.ParallelismPrimitive != test.want {
				t.Fatalf("SelectPlacement() primitive = %q, want %q", got.ParallelismPrimitive, test.want)
			}
			if got.ExecutionPlacement == "" || got.PlacementRationale == "" ||
				len(got.CapabilityRequirements) == 0 || got.LifecycleOwner == "" || got.Fallback == "" {
				t.Fatalf("SelectPlacement() omitted required field: %#v", got)
			}
		})
	}
}

func TestPlacementFailsClosedForCapabilitiesConcurrencyAndOverlappingMutation(t *testing.T) {
	t.Parallel()

	capabilities := completeCapabilities()
	delete(capabilities.Primitives, agentharness.PersistentSession)
	facts := controlplane.TaskFacts{
		TaskID: "isolated", Mutating: true, NeedsIsolation: true, NeedsRestart: true,
		Ownership: []string{"internal/isolated/**"},
	}
	if _, err := controlplane.SelectPlacement(facts, capabilities, controlplane.Capacity{}); !errors.Is(err, controlplane.ErrCapabilityUnavailable) {
		t.Fatalf("SelectPlacement(missing persistent) error = %v, want ErrCapabilityUnavailable", err)
	}

	capabilities = completeCapabilities()
	if _, err := controlplane.SelectPlacement(
		facts, capabilities,
		controlplane.Capacity{Active: map[agentharness.Primitive]int{agentharness.PersistentSession: 2}},
	); !errors.Is(err, controlplane.ErrConcurrencyExhausted) {
		t.Fatalf("SelectPlacement(exhausted) error = %v, want ErrConcurrencyExhausted", err)
	}

	placement, err := controlplane.SelectPlacement(
		controlplane.TaskFacts{TaskID: "subagent", Mutating: true, Short: true, Ownership: []string{"internal/a/**"}},
		capabilities, controlplane.Capacity{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controlplane.ValidateOwnershipLease(
		"subagent", []string{"internal/a/**"}, placement,
		[]controlplane.ActiveOwnership{{TaskID: "other", Mutable: []string{"internal/a/file.go"}}},
	); !errors.Is(err, controlplane.ErrOwnershipConflict) {
		t.Fatalf("ValidateOwnershipLease() error = %v, want ErrOwnershipConflict", err)
	}
}

func TestPlacementPromotesGrowingSubagentWithExplicitHandoff(t *testing.T) {
	t.Parallel()

	task := validGraph().Tasks[0]
	task.Placement = controlplane.ExecutionPlacement{
		ParallelismPrimitive:   agentharness.EphemeralSubagent,
		ExecutionPlacement:     "codex_local_subagent",
		PlacementRationale:     "short read-only review",
		CapabilityRequirements: []agentharness.Capability{agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait, agentharness.CapRuntimeClose},
		LifecycleOwner:         "subagent:child-1",
		Fallback:               "local_sequential",
	}
	task.Ownership.Mutable = nil
	promoted, handoff, err := controlplane.Promote(
		task,
		controlplane.Promotion{
			Checkpoint:       controlplane.EvidenceRef{ID: "checkpoint-1", Digest: digest("checkpoint")},
			Reason:           "scope now requires mutation and restart survival",
			NewOwner:         "session:reserved-1",
			RequiresMutation: true,
			RequiresRestart:  true,
			Mutable:          []string{"internal/a/**"},
		},
		completeCapabilities(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Placement.ParallelismPrimitive != agentharness.PersistentSession ||
		promoted.Placement.LifecycleOwner != "session:reserved-1" ||
		handoff.Evidence.ID != "checkpoint-1" || handoff.FromOwner != "subagent:child-1" ||
		handoff.ToOwner != "session:reserved-1" || !handoff.AcknowledgementRequired {
		t.Fatalf("Promote() = task %#v, handoff %#v", promoted, handoff)
	}
	if len(promoted.Ownership.Mutable) != 1 || promoted.Ownership.Mutable[0] != "internal/a/**" {
		t.Fatalf("Promote() ownership = %#v", promoted.Ownership)
	}
}

func TestPromotionAtomicallyTransfersLifecycleAfterEphemeralClose(t *testing.T) {
	t.Parallel()

	store := controlmock.NewStore()
	service := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	graph := validGraph()
	graph.Tasks = graph.Tasks[:1]
	graph.IntegrationOrder = graph.IntegrationOrder[:1]
	graph.Tasks[0].State = controlplane.TaskReady
	graph.Tasks[0].Ownership.Mutable = nil
	graph.Tasks[0].Placement = controlplane.ExecutionPlacement{
		ParallelismPrimitive: agentharness.EphemeralSubagent,
		ExecutionPlacement:   "codex_local_subagent", PlacementRationale: "short read-only review",
		CapabilityRequirements: append([]string(nil), agentharness.RequiredCapabilities(agentharness.EphemeralSubagent)...),
		LifecycleOwner:         "subagent:runtime-ephemeral", Fallback: "local_sequential",
	}
	run := validRun(graph)
	if _, err := service.Create(context.Background(), run, graph); err != nil {
		t.Fatal(err)
	}
	attempt, err := service.CreateAttempt(context.Background(), run.ID, "task-a", completeCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionDispatch, digest("promotion-dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), run.ID, dispatch.ID, agentharness.ActionResult{
		ActionID: dispatch.ID, RuntimeWorkIDs: []string{"runtime-ephemeral"}, ResultDigest: digest("promotion-dispatch-result"),
	}); err != nil {
		t.Fatal(err)
	}
	wait, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionWait, digest("promotion-wait"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), run.ID, wait.ID, agentharness.ActionResult{
		ActionID: wait.ID, RuntimeWorkIDs: []string{"runtime-ephemeral"}, ResultDigest: digest("promotion-wait-result"),
		Events: []agentharness.WorkEvent{{
			ID: "promotion-terminal", RuntimeWorkID: "runtime-ephemeral",
			Kind: "completed", ResultDigest: digest("promotion-terminal"), Terminal: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	evidence := controlplane.Evidence{
		ID: "promotion-checkpoint", TaskID: "task-a", AttemptID: attempt.ID,
		BaseSHA: digest("base-a"), HeadSHA: digest("promotion-head"), OwnedPathsDigest: digest("promotion-paths"),
		Tests: []controlplane.TestEvidence{{CommandDigest: digest("promotion-test"), ResultDigest: digest("promotion-pass"), Passed: true}},
	}
	if _, err := service.RecordEvidence(context.Background(), run.ID, evidence); err != nil {
		t.Fatal(err)
	}
	reference := controlplane.EvidenceRef{ID: evidence.ID, Digest: controlplane.EvidenceDigest(evidence)}
	if _, err := service.AttachTerminalEvidence(context.Background(), run.ID, attempt.ID, reference); err != nil {
		t.Fatal(err)
	}
	closeAction, err := service.PrepareAction(context.Background(), run.ID, attempt.ID, agentharness.ActionClose, digest("promotion-close"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteAction(context.Background(), run.ID, closeAction.ID, agentharness.ActionResult{
		ActionID: closeAction.ID, RuntimeWorkIDs: []string{"runtime-ephemeral"}, ResultDigest: digest("promotion-close-result"),
		CloseEvidence: agentharness.CloseEvidence{Kind: agentharness.CloseRuntime, Receipt: "promotion-runtime-close", Digest: digest("promotion-runtime-close")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetDisposition(context.Background(), run.ID, attempt.ID, controlplane.Disposition{
		Kind: controlplane.DispositionHandedOff, EvidenceID: evidence.ID,
	}); err != nil {
		t.Fatal(err)
	}

	replacement, handoff, err := service.PromoteAttempt(
		context.Background(), run.ID, attempt.ID,
		controlplane.Promotion{
			Checkpoint: reference, Reason: "scope requires mutation and restart survival",
			NewOwner: "session:replacement", RequiresMutation: true, RequiresRestart: true,
			Mutable: []string{"internal/a/**"},
		},
		completeCapabilities(),
	)
	if err != nil {
		t.Fatal(err)
	}
	restarted := controlplane.NewService(store, controlplane.WithClock(fixedClock))
	snapshot, err := restarted.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := snapshot.Attempts[attempt.ID]
	if old.State != controlplane.AttemptCompleted || old.CloseEvidence.Kind != controlplane.CloseRuntime ||
		replacement.Primitive != agentharness.PersistentSession || replacement.PromotedFromAttemptID != attempt.ID ||
		replacement.HandoffID != handoff.ID || snapshot.Graph.Tasks[0].Placement.LifecycleOwner != "session:replacement" ||
		len(snapshot.Graph.Tasks[0].Ownership.Mutable) != 1 {
		t.Fatalf("promotion transition = old %#v replacement %#v handoff %#v task %#v", old, replacement, handoff, snapshot.Graph.Tasks[0])
	}
	activeWriters := 0
	for _, candidate := range snapshot.Attempts {
		if candidate.TaskID == "task-a" && candidate.State != controlplane.AttemptCompleted &&
			candidate.State != controlplane.AttemptFailed && candidate.State != controlplane.AttemptCanceled {
			activeWriters++
		}
	}
	if activeWriters != 1 {
		t.Fatalf("active writers after promotion = %d, want 1", activeWriters)
	}
	forged := controlplane.CloneSnapshot(snapshot)
	duplicate := replacement
	duplicate.ID = "attempt-forged-overlapping-promotion"
	forged.Attempts[duplicate.ID] = duplicate
	if err := controlplane.ValidateSnapshot(forged); !errors.Is(err, controlplane.ErrInvalidRecord) {
		t.Fatalf("ValidateSnapshot(overlapping promoted writers) error = %v, want ErrInvalidRecord", err)
	}
	if len(snapshot.Events) < 2 || snapshot.Events[len(snapshot.Events)-2].Kind != controlplane.EventHandoff ||
		snapshot.Events[len(snapshot.Events)-1].Kind != controlplane.EventPlacement {
		t.Fatalf("promotion event order = %#v", snapshot.Events)
	}
}

func TestPlacementUsesDeterministicSafeFallbacks(t *testing.T) {
	t.Parallel()

	capabilities := completeCapabilities()
	delete(capabilities.Primitives, agentharness.HarnessNativeParallel)
	got, err := controlplane.SelectPlacement(controlplane.TaskFacts{
		TaskID: "fanout", ReadOnly: true, Short: true, HomogeneousFanout: true, FanoutItems: 3,
	}, capabilities, controlplane.Capacity{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ParallelismPrimitive != agentharness.LocalSequential {
		t.Fatalf("native fallback = %q, want local_sequential", got.ParallelismPrimitive)
	}

	capabilities = completeCapabilities()
	delete(capabilities.Primitives, agentharness.EphemeralSubagent)
	got, err = controlplane.SelectPlacement(controlplane.TaskFacts{
		TaskID: "review", ReadOnly: true, Short: true,
	}, capabilities, controlplane.Capacity{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ParallelismPrimitive != agentharness.LocalSequential {
		t.Fatalf("subagent fallback = %q, want local_sequential", got.ParallelismPrimitive)
	}
}

func TestPlacementValidationRejectsUnsafeFallbackUnsupportedLabelAndProjectCapacity(t *testing.T) {
	t.Parallel()

	task := validGraph().Tasks[0]
	task.Placement.Fallback = "ephemeral_subagent"
	if err := controlplane.ValidatePlacement(
		task, completeCapabilities(), controlplane.Capacity{}, nil,
	); !errors.Is(err, controlplane.ErrInvalidPlacement) {
		t.Fatalf("ValidatePlacement(unsafe fallback) error = %v, want ErrInvalidPlacement", err)
	}

	task = validGraph().Tasks[0]
	capabilities := completeCapabilities()
	persistent := capabilities.Primitives[agentharness.PersistentSession]
	delete(persistent.Capabilities, agentharness.CapArchive)
	capabilities.Primitives[agentharness.PersistentSession] = persistent
	if err := controlplane.ValidatePlacement(
		task, capabilities, controlplane.Capacity{}, nil,
	); !errors.Is(err, controlplane.ErrCapabilityUnavailable) {
		t.Fatalf("ValidatePlacement(unsupported primitive label) error = %v, want ErrCapabilityUnavailable", err)
	}

	if _, err := controlplane.SelectPlacement(controlplane.TaskFacts{
		TaskID: "project-limited", ProjectIDs: []string{"project-a"},
		Mutating: true, NeedsRestart: true, Ownership: []string{"internal/a/**"},
	}, completeCapabilities(), controlplane.Capacity{
		ProjectActive: map[string]int{"project-a": 1},
		ProjectLimits: map[string]int{"project-a": 1},
	}); !errors.Is(err, controlplane.ErrConcurrencyExhausted) {
		t.Fatalf("SelectPlacement(project capacity) error = %v, want ErrConcurrencyExhausted", err)
	}
}

func completeCapabilities() agentharness.CapabilitySnapshot {
	return agentharness.CapabilitySnapshot{
		HarnessID: "codex",
		Primitives: map[agentharness.Primitive]agentharness.PrimitiveCapabilities{
			agentharness.PersistentSession: {
				Primitive: agentharness.PersistentSession,
				Capabilities: agentharness.CapabilitySet(
					agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait,
					agentharness.CapRuntimeIdentity, agentharness.CapAcknowledge,
					agentharness.CapSend, agentharness.CapCallback, agentharness.CapCursor,
					agentharness.CapInterrupt, agentharness.CapIdempotency,
					agentharness.CapRestart, agentharness.CapArchive, agentharness.CapIsolation,
				),
				ConcurrencyLimit: 2,
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
			agentharness.LocalSequential: {
				Primitive:        agentharness.LocalSequential,
				Capabilities:     agentharness.CapabilitySet(agentharness.CapLocal),
				ConcurrencyLimit: 1,
			},
		},
	}
}
