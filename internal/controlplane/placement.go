package controlplane

import (
	"fmt"
	"strings"

	"github.com/araihu/paje/internal/agentharness"
)

type TaskFacts struct {
	TaskID                 string
	ProjectIDs             []string
	Short                  bool
	ReadOnly               bool
	Mutating               bool
	NeedsIsolation         bool
	NeedsRestart           bool
	CrossProject           bool
	NeedsSteering          bool
	HomogeneousFanout      bool
	FanoutItems            int
	SharedIntegrationFiles bool
	UncertainBoundary      bool
	Ownership              []string
}

type Capacity struct {
	Active        map[agentharness.Primitive]int
	ProjectActive map[string]int
	ProjectLimits map[string]int
}

type ActiveOwnership struct {
	TaskID     string
	ProjectIDs []string
	Mutable    []string
}

type Promotion struct {
	Checkpoint       EvidenceRef
	Reason           string
	NewOwner         string
	RequiresMutation bool
	RequiresRestart  bool
	Mutable          []string
}

func SelectPlacement(
	facts TaskFacts,
	capabilities agentharness.CapabilitySnapshot,
	capacity Capacity,
) (ExecutionPlacement, error) {
	if facts.TaskID == "" || facts.ReadOnly && facts.Mutating || facts.FanoutItems < 0 {
		return ExecutionPlacement{}, ErrInvalidPlacement
	}
	if err := capabilities.Validate(); err != nil {
		return ExecutionPlacement{}, fmt.Errorf("%w: %v", ErrCapabilityUnavailable, err)
	}

	primitive := agentharness.LocalSequential
	rationale := "current control agent owns dependent or integration-sensitive work"
	placement := "current_control_agent"
	fallback := "block"
	switch {
	case facts.SharedIntegrationFiles || facts.UncertainBoundary:
		primitive = agentharness.LocalSequential
	case facts.HomogeneousFanout && facts.FanoutItems > 1:
		primitive = agentharness.HarnessNativeParallel
		rationale = "bounded homogeneous fan-out has exact inputs and deterministic aggregation"
		placement = "harness_native_parallel"
		fallback = "persistent_session_or_local_sequential"
	case facts.NeedsIsolation || facts.NeedsRestart || facts.CrossProject || facts.NeedsSteering ||
		facts.Mutating && !facts.Short:
		primitive = agentharness.PersistentSession
		rationale = "long, restartable, isolated, independently steered, or cross-project work"
		placement = "isolated_persistent_session"
		fallback = "block"
	case facts.Short:
		primitive = agentharness.EphemeralSubagent
		rationale = "short bounded work benefits from strongly shared context"
		placement = "same_session_subagent"
		fallback = "local_sequential"
	}
	primitiveCaps, ok := capabilities.Primitives[primitive]
	if !ok {
		if primitive == agentharness.PersistentSession && (facts.NeedsIsolation || facts.NeedsRestart || facts.CrossProject) {
			return ExecutionPlacement{}, fmt.Errorf("%w: persistent session cannot be safely downgraded", ErrCapabilityUnavailable)
		}
		if primitive == agentharness.HarnessNativeParallel {
			primitive = agentharness.LocalSequential
			primitiveCaps, ok = capabilities.Primitives[primitive]
			rationale = "native fan-out unavailable; deterministic sequential fallback selected"
			placement = "current_control_agent"
			fallback = "block"
		} else if primitive == agentharness.EphemeralSubagent {
			primitive = agentharness.LocalSequential
			primitiveCaps, ok = capabilities.Primitives[primitive]
			rationale = "subagent unavailable; bounded sequential fallback selected"
			placement = "current_control_agent"
			fallback = "block"
		}
	}
	if !ok {
		return ExecutionPlacement{}, ErrCapabilityUnavailable
	}
	if primitiveCaps.ConcurrencyLimit > 0 && capacity.Active[primitive] >= primitiveCaps.ConcurrencyLimit {
		return ExecutionPlacement{}, ErrConcurrencyExhausted
	}
	for _, projectID := range facts.ProjectIDs {
		if limit := capacity.ProjectLimits[projectID]; limit > 0 &&
			capacity.ProjectActive[projectID] >= limit {
			return ExecutionPlacement{}, fmt.Errorf("%w: project %s", ErrConcurrencyExhausted, projectID)
		}
	}
	requirements := agentharness.RequiredCapabilities(primitive)
	for _, requirement := range requirements {
		if !primitiveCaps.Supports(requirement) {
			return ExecutionPlacement{}, fmt.Errorf("%w: %s lacks %s", ErrCapabilityUnavailable, primitive, requirement)
		}
	}
	return ExecutionPlacement{
		ParallelismPrimitive: primitive, ExecutionPlacement: placement,
		PlacementRationale: rationale, CapabilityRequirements: append([]string(nil), requirements...),
		LifecycleOwner: "control:" + facts.TaskID, Fallback: fallback,
	}, nil
}

func ValidatePlacement(
	task Task,
	capabilities agentharness.CapabilitySnapshot,
	capacity Capacity,
	active []ActiveOwnership,
) error {
	if err := validatePlacementFields(task.Placement); err != nil {
		return err
	}
	if err := capabilities.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrCapabilityUnavailable, err)
	}
	primitiveCapabilities, ok := capabilities.Primitives[task.Placement.ParallelismPrimitive]
	if !ok {
		return ErrCapabilityUnavailable
	}
	for _, requirement := range task.Placement.CapabilityRequirements {
		if !primitiveCapabilities.Supports(requirement) {
			return fmt.Errorf("%w: %s", ErrCapabilityUnavailable, requirement)
		}
	}
	if primitiveCapabilities.ConcurrencyLimit > 0 &&
		capacity.Active[task.Placement.ParallelismPrimitive] >= primitiveCapabilities.ConcurrencyLimit {
		return ErrConcurrencyExhausted
	}
	for _, project := range task.Projects {
		if limit := capacity.ProjectLimits[project.ID]; limit > 0 &&
			capacity.ProjectActive[project.ID] >= limit {
			return fmt.Errorf("%w: project %s", ErrConcurrencyExhausted, project.ID)
		}
	}
	return ValidateOwnershipLease(
		task.ID, task.Ownership.Mutable, task.Placement, active,
	)
}

func ValidateOwnershipLease(
	taskID string,
	mutable []string,
	placement ExecutionPlacement,
	active []ActiveOwnership,
) error {
	if taskID == "" {
		return ErrInvalidPlacement
	}
	if placement.ParallelismPrimitive == agentharness.EphemeralSubagent &&
		strings.TrimSpace(placement.LifecycleOwner) == "" {
		return ErrInvalidPlacement
	}
	for _, writer := range active {
		if writer.TaskID == taskID {
			continue
		}
		if ownershipOverlaps(mutable, writer.Mutable) {
			return fmt.Errorf("%w: %s overlaps %s", ErrOwnershipConflict, taskID, writer.TaskID)
		}
	}
	return nil
}

func sharesString(first, second []string) bool {
	for _, left := range first {
		if contains(second, left) {
			return true
		}
	}
	return false
}

func Promote(
	task Task,
	promotion Promotion,
	capabilities agentharness.CapabilitySnapshot,
) (Task, Handoff, error) {
	if task.Placement.ParallelismPrimitive != agentharness.EphemeralSubagent ||
		promotion.Checkpoint.ID == "" || !validDigest(promotion.Checkpoint.Digest) ||
		promotion.Reason == "" || promotion.NewOwner == "" ||
		(!promotion.RequiresMutation && !promotion.RequiresRestart) {
		return Task{}, Handoff{}, ErrInvalidPlacement
	}
	persistent, ok := capabilities.Primitives[agentharness.PersistentSession]
	if !ok {
		return Task{}, Handoff{}, ErrCapabilityUnavailable
	}
	for _, requirement := range agentharness.RequiredCapabilities(agentharness.PersistentSession) {
		if !persistent.Supports(requirement) {
			return Task{}, Handoff{}, ErrCapabilityUnavailable
		}
	}
	previousOwner := task.Placement.LifecycleOwner
	promoted := task
	promoted.Placement = ExecutionPlacement{
		ParallelismPrimitive:   agentharness.PersistentSession,
		ExecutionPlacement:     "isolated_persistent_session",
		PlacementRationale:     promotion.Reason,
		CapabilityRequirements: append([]string(nil), agentharness.RequiredCapabilities(agentharness.PersistentSession)...),
		LifecycleOwner:         promotion.NewOwner,
		Fallback:               "block",
	}
	if promotion.RequiresMutation {
		promoted.Ownership.Mutable = append([]string(nil), promotion.Mutable...)
	}
	handoff := Handoff{
		ID:             "handoff-" + task.ID + "-" + promotion.Checkpoint.ID,
		ProducerTaskID: task.ID, ConsumerTaskID: task.ID,
		FromOwner: previousOwner, ToOwner: promotion.NewOwner,
		Evidence: promotion.Checkpoint, AcknowledgementRequired: true,
	}
	return promoted, handoff, nil
}
