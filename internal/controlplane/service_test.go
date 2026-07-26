package controlplane_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane"
	controlmock "github.com/araihu/paje/internal/controlplane/mock"
)

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
