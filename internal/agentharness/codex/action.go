// Package codex validates semantic two-phase lifecycle action documents for
// Codex integrations. It does not know or invoke any Codex tool name.
package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/araihu/paje/internal/agentharness"
)

type PrepareRequest struct {
	ControlRunID    string
	TaskID          string
	AttemptID       string
	GraphRevision   uint64
	Primitive       agentharness.Primitive
	Kind            agentharness.ActionKind
	RequestDigest   string
	ParentRuntimeID string
	RuntimeWorkIDs  []string
}

type ActionDocument struct {
	SchemaVersion   string                  `json:"schema_version"`
	ActionID        string                  `json:"action_id"`
	ControlRunID    string                  `json:"control_run_id"`
	TaskID          string                  `json:"task_id"`
	AttemptID       string                  `json:"attempt_id"`
	GraphRevision   uint64                  `json:"graph_revision"`
	Primitive       agentharness.Primitive  `json:"parallelism_primitive"`
	Kind            agentharness.ActionKind `json:"action"`
	Capability      agentharness.Capability `json:"semantic_capability"`
	RequestDigest   string                  `json:"request_digest"`
	ParentRuntimeID string                  `json:"parent_runtime_id,omitempty"`
	RuntimeWorkIDs  []string                `json:"runtime_work_ids,omitempty"`
}

func Prepare(request PrepareRequest) (ActionDocument, error) {
	capability, err := agentharness.OperationCapability(request.Kind, request.Primitive)
	if err != nil {
		return ActionDocument{}, err
	}
	actionID, err := agentharness.StableActionID(
		request.ControlRunID, request.TaskID, request.AttemptID,
		request.GraphRevision, request.Primitive, request.Kind, request.RequestDigest,
	)
	if err != nil {
		return ActionDocument{}, err
	}
	if strings.ContainsAny(request.ParentRuntimeID, "\r\n\x00") {
		return ActionDocument{}, agentharness.ErrInvalidRequest
	}
	if request.Kind == agentharness.ActionDispatch && len(request.RuntimeWorkIDs) != 0 {
		return ActionDocument{}, agentharness.ErrInvalidRequest
	}
	seenRuntimeIDs := make(map[string]bool, len(request.RuntimeWorkIDs))
	for _, runtimeID := range request.RuntimeWorkIDs {
		if strings.TrimSpace(runtimeID) == "" || strings.ContainsAny(runtimeID, "\r\n\x00") ||
			seenRuntimeIDs[runtimeID] {
			return ActionDocument{}, agentharness.ErrInvalidRequest
		}
		seenRuntimeIDs[runtimeID] = true
	}
	return ActionDocument{
		SchemaVersion: "paje.agent-action/v1",
		ActionID:      actionID, ControlRunID: request.ControlRunID, TaskID: request.TaskID,
		AttemptID: request.AttemptID, GraphRevision: request.GraphRevision,
		Primitive: request.Primitive, Kind: request.Kind, Capability: capability,
		RequestDigest: request.RequestDigest, ParentRuntimeID: request.ParentRuntimeID,
		RuntimeWorkIDs: append([]string(nil), request.RuntimeWorkIDs...),
	}, nil
}

func Complete(
	document ActionDocument,
	result agentharness.ActionResult,
	capabilities agentharness.PrimitiveCapabilities,
) (agentharness.ActionResult, error) {
	if document.SchemaVersion != "paje.agent-action/v1" ||
		document.Primitive != capabilities.Primitive ||
		result.ActionID != document.ActionID {
		return agentharness.ActionResult{}, agentharness.ErrActionMismatch
	}
	capability, err := agentharness.OperationCapability(document.Kind, document.Primitive)
	if err != nil || capability != document.Capability || !capabilities.Supports(capability) {
		return agentharness.ActionResult{}, agentharness.ErrUnsupportedOperation
	}
	if len(result.RuntimeWorkIDs) > 0 && !capabilities.Supports(agentharness.CapRuntimeIdentity) {
		return agentharness.ActionResult{}, agentharness.ErrUnexpectedRuntimeIdentity
	}
	if document.Primitive == agentharness.PersistentSession && document.Kind == agentharness.ActionDispatch &&
		len(result.RuntimeWorkIDs) != 1 {
		return agentharness.ActionResult{}, fmt.Errorf("%w: persistent dispatch must return exactly one runtime ID", agentharness.ErrActionMismatch)
	}
	if document.Primitive == agentharness.HarnessNativeParallel && document.Kind == agentharness.ActionDispatch &&
		len(result.RuntimeWorkIDs) > 0 && !capabilities.Supports(agentharness.CapRuntimeIdentity) {
		return agentharness.ActionResult{}, agentharness.ErrUnexpectedRuntimeIdentity
	}
	if document.Kind != agentharness.ActionDispatch &&
		!sameStrings(document.RuntimeWorkIDs, result.RuntimeWorkIDs) {
		return agentharness.ActionResult{}, agentharness.ErrActionMismatch
	}
	if err := agentharness.ValidateActionResult(
		document.Kind, document.Primitive, document.RuntimeWorkIDs, 0,
		capabilities.Supports(agentharness.CapCursor), result,
	); err != nil {
		if errors.Is(err, agentharness.ErrUnexpectedRuntimeIdentity) {
			return agentharness.ActionResult{}, err
		}
		return agentharness.ActionResult{}, agentharness.ErrActionMismatch
	}
	result.Diagnostic = agentharness.SafeDiagnostic(result.Diagnostic)
	return result, nil
}

func sameStrings(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func (d ActionDocument) CanonicalJSON() []byte {
	encoded, _ := json.Marshal(d)
	return encoded
}
