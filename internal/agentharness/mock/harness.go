// Package mock provides a controllable, concurrency-safe AgentHarness used by
// domain tests and the reusable lifecycle conformance suite.
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/agentharness/contracttest"
)

type Harness struct {
	mu             sync.Mutex
	capabilities   agentharness.CapabilitySnapshot
	scenario       contracttest.Scenario
	results        map[string]any
	requests       map[string]string
	requestDigests map[string]string
	attempts       map[string]attemptState
	calls          []string
}

type attemptState struct {
	primitive      agentharness.Primitive
	runtimeWorkIDs []string
	itemDigests    []string
}

var _ agentharness.AgentHarness = (*Harness)(nil)

func New(capabilities agentharness.CapabilitySnapshot) *Harness {
	return &Harness{
		capabilities: capabilities, results: make(map[string]any),
		requests: make(map[string]string), requestDigests: make(map[string]string),
		attempts: make(map[string]attemptState),
	}
}

func ContractFixture(scenario contracttest.Scenario) contracttest.Fixture {
	capabilities := allCapabilities()
	harness := New(capabilities)
	harness.scenario = scenario
	if scenario == contracttest.ScenarioUnsupported {
		ephemeral := capabilities.Primitives[agentharness.EphemeralSubagent]
		delete(ephemeral.Capabilities, agentharness.CapSend)
		capabilities.Primitives[agentharness.EphemeralSubagent] = ephemeral
		harness.capabilities = capabilities
	}
	return contracttest.Fixture{Harness: harness, Capabilities: capabilities}
}

func (h *Harness) Capabilities(ctx context.Context, _ agentharness.Principal, _ string) (agentharness.HarnessCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return agentharness.HarnessCapabilities{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "capabilities")
	return cloneCapabilities(h.capabilities), nil
}

func (h *Harness) Dispatch(ctx context.Context, request agentharness.DispatchWorkRequest) (agentharness.DispatchWorkResult, error) {
	if err := ctx.Err(); err != nil {
		return agentharness.DispatchWorkResult{}, err
	}
	if err := request.Validate(); err != nil {
		return agentharness.DispatchWorkResult{}, err
	}
	if request.Primitive == agentharness.LocalSequential {
		return agentharness.DispatchWorkResult{}, agentharness.ErrUnsupportedOperation
	}
	capabilities, ok := h.capabilities.Primitives[request.Primitive]
	if !ok || !capabilities.Supports(agentharness.CapDispatch) {
		return agentharness.DispatchWorkResult{}, agentharness.ErrUnsupportedOperation
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.bindLocked(request.ActionID, request); err != nil {
		return agentharness.DispatchWorkResult{}, err
	}
	if existing, ok := h.results[request.ActionID]; ok {
		return existing.(agentharness.DispatchWorkResult), nil
	}
	result := agentharness.DispatchWorkResult{ActionID: request.ActionID, ResultDigest: digest("dispatch")}
	if capabilities.Supports(agentharness.CapCursor) {
		result.Cursor = "cursor-dispatch"
		result.CursorSequence = 1
	}
	if request.Primitive == agentharness.PersistentSession ||
		request.Primitive == agentharness.EphemeralSubagent && h.scenario == contracttest.ScenarioEphemeralIdentity {
		result.RuntimeWorkIDs = []string{"runtime-child-1"}
	}
	if h.scenario == contracttest.ScenarioBoundedDiagnostic {
		result.Diagnostic = agentharness.SafeDiagnostic(strings.Repeat("safe-", 400))
	}
	h.results[request.ActionID] = result
	h.requestDigests[request.ActionID] = request.RequestDigest
	h.attempts[request.AttemptID] = attemptState{
		primitive: request.Primitive, runtimeWorkIDs: append([]string(nil), result.RuntimeWorkIDs...),
		itemDigests: append([]string(nil), request.ExpectedItemDigests...),
	}
	h.calls = append(h.calls, "dispatch")
	return result, nil
}

func (h *Harness) Observe(ctx context.Context, request agentharness.ObserveWorkRequest) (agentharness.WorkEvents, error) {
	if err := ctx.Err(); err != nil {
		return agentharness.WorkEvents{}, err
	}
	if err := request.Validate(); err != nil {
		return agentharness.WorkEvents{}, err
	}
	if !h.supports(agentharness.CapObserve) {
		return agentharness.WorkEvents{}, agentharness.ErrUnsupportedOperation
	}
	if request.ReconcileActionID != "" {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.requestDigests[request.ReconcileActionID] != request.ReconcileRequestDigest {
			return agentharness.WorkEvents{}, agentharness.ErrActionOutcomeUnknown
		}
		dispatched, ok := h.results[request.ReconcileActionID].(agentharness.DispatchWorkResult)
		if !ok {
			return agentharness.WorkEvents{}, agentharness.ErrActionOutcomeUnknown
		}
		result := &agentharness.ActionResult{
			ActionID: dispatched.ActionID, RuntimeWorkIDs: append([]string(nil), dispatched.RuntimeWorkIDs...),
			Cursor: dispatched.Cursor, CursorSequence: dispatched.CursorSequence,
			ResultDigest: dispatched.ResultDigest, Diagnostic: dispatched.Diagnostic,
		}
		h.calls = append(h.calls, "observe")
		return agentharness.WorkEvents{ActionID: request.ActionID, ReconciledResult: result}, nil
	}
	if err := h.bind(request.ActionID, request); err != nil {
		return agentharness.WorkEvents{}, err
	}
	h.mu.Lock()
	attempt := h.attempts[request.AttemptID]
	h.mu.Unlock()
	runtimeWorkID := ""
	if len(attempt.runtimeWorkIDs) > 0 {
		runtimeWorkID = attempt.runtimeWorkIDs[0]
	}
	h.record("observe")
	return agentharness.WorkEvents{
		ActionID: request.ActionID, NextCursor: "cursor-observe", NextCursorSequence: 2,
		Events: []agentharness.WorkEvent{{ID: "event-1", RuntimeWorkID: runtimeWorkID, Cursor: "cursor-observe", CursorSequence: 2, Kind: "progress", ResultDigest: digest("event")}},
	}, nil
}

func (h *Harness) Send(ctx context.Context, request agentharness.SendWorkRequest) (agentharness.MessageReceipt, error) {
	if err := ctx.Err(); err != nil {
		return agentharness.MessageReceipt{}, err
	}
	if err := request.Validate(); err != nil {
		return agentharness.MessageReceipt{}, err
	}
	if h.scenario == contracttest.ScenarioUnsupported {
		return agentharness.MessageReceipt{}, agentharness.ErrUnsupportedOperation
	}
	if !h.supports(agentharness.CapSend) {
		return agentharness.MessageReceipt{}, agentharness.ErrUnsupportedOperation
	}
	if err := h.bind(request.ActionID, request); err != nil {
		return agentharness.MessageReceipt{}, err
	}
	h.record("send")
	return agentharness.MessageReceipt{ActionID: request.ActionID, Receipt: "message-1", Cursor: "cursor-send", CursorSequence: 3}, nil
}

func (h *Harness) Wait(ctx context.Context, request agentharness.WaitWorkRequest) (agentharness.WorkEvents, error) {
	if err := ctx.Err(); err != nil {
		return agentharness.WorkEvents{}, err
	}
	if err := request.Validate(); err != nil {
		return agentharness.WorkEvents{}, err
	}
	if !h.supports(agentharness.CapWait) {
		return agentharness.WorkEvents{}, agentharness.ErrUnsupportedOperation
	}
	if err := h.bind(request.ActionID, request); err != nil {
		return agentharness.WorkEvents{}, err
	}
	h.mu.Lock()
	attempt := h.attempts[request.AttemptIDs[0]]
	h.mu.Unlock()
	runtimeWorkID := ""
	if len(attempt.runtimeWorkIDs) > 0 {
		runtimeWorkID = attempt.runtimeWorkIDs[0]
	}
	cursor := ""
	var cursorSequence uint64
	if h.capabilities.Primitives[attempt.primitive].Supports(agentharness.CapCursor) {
		cursor = "cursor-terminal"
		cursorSequence = 4
	}
	h.record("wait")
	return agentharness.WorkEvents{
		ActionID: request.ActionID, NextCursor: cursor, NextCursorSequence: cursorSequence, Terminal: true,
		Events: []agentharness.WorkEvent{{ID: "event-terminal", RuntimeWorkID: runtimeWorkID, Cursor: cursor, CursorSequence: cursorSequence, Kind: "completed", ResultDigest: digest("terminal"), Terminal: true}},
	}, nil
}

func (h *Harness) Interrupt(ctx context.Context, request agentharness.InterruptWorkRequest) (agentharness.InterruptReceipt, error) {
	if err := ctx.Err(); err != nil {
		return agentharness.InterruptReceipt{}, err
	}
	if err := request.Validate(); err != nil {
		return agentharness.InterruptReceipt{}, err
	}
	if !h.supports(agentharness.CapInterrupt) {
		return agentharness.InterruptReceipt{}, agentharness.ErrUnsupportedOperation
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.bindLocked(request.ActionID, request); err != nil {
		return agentharness.InterruptReceipt{}, err
	}
	attempt, exists := h.attempts[request.AttemptID]
	if !exists || !sameStrings(attempt.runtimeWorkIDs, request.RuntimeWorkIDs) {
		return agentharness.InterruptReceipt{}, agentharness.ErrActionMismatch
	}
	if existing, ok := h.results[request.ActionID]; ok {
		return existing.(agentharness.InterruptReceipt), nil
	}
	result := agentharness.InterruptReceipt{ActionID: request.ActionID, Receipt: "interrupt-1"}
	if h.capabilities.Primitives[attempt.primitive].Supports(agentharness.CapCursor) {
		result.Cursor = "cursor-interrupt"
		result.CursorSequence = 5
	}
	h.results[request.ActionID] = result
	h.calls = append(h.calls, "interrupt")
	return result, nil
}

func (h *Harness) Close(ctx context.Context, request agentharness.CloseWorkRequest) (agentharness.WorkCloseEvidence, error) {
	if err := ctx.Err(); err != nil {
		return agentharness.WorkCloseEvidence{}, err
	}
	if err := request.Validate(); err != nil {
		return agentharness.WorkCloseEvidence{}, err
	}
	if request.Primitive == agentharness.LocalSequential {
		return agentharness.WorkCloseEvidence{}, agentharness.ErrUnsupportedOperation
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.bindLocked(request.ActionID, request); err != nil {
		return agentharness.WorkCloseEvidence{}, err
	}
	attempt, exists := h.attempts[request.AttemptID]
	if !exists || attempt.primitive != request.Primitive ||
		!sameStrings(attempt.runtimeWorkIDs, request.RuntimeWorkIDs) {
		return agentharness.WorkCloseEvidence{}, agentharness.ErrActionMismatch
	}
	if request.Primitive == agentharness.HarnessNativeParallel &&
		!sameStrings(attempt.itemDigests, request.ResultDigests) {
		return agentharness.WorkCloseEvidence{}, agentharness.ErrActionMismatch
	}
	if existing, ok := h.results[request.ActionID]; ok {
		return existing.(agentharness.WorkCloseEvidence), nil
	}
	kind := agentharness.CloseInactive
	switch request.Primitive {
	case agentharness.PersistentSession:
		kind = agentharness.CloseArchive
	case agentharness.EphemeralSubagent:
		kind = agentharness.CloseRuntime
	case agentharness.HarnessNativeParallel:
		kind = agentharness.CloseAggregate
	default:
		return agentharness.WorkCloseEvidence{}, agentharness.ErrUnsupportedOperation
	}
	result := agentharness.WorkCloseEvidence{
		ActionID: request.ActionID,
		Evidence: agentharness.CloseEvidence{Kind: kind, Receipt: "close-1", Digest: digest("close")},
	}
	h.results[request.ActionID] = result
	h.calls = append(h.calls, "close")
	return result, nil
}

func (h *Harness) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *Harness) supports(capability agentharness.Capability) bool {
	for _, primitive := range h.capabilities.Primitives {
		if primitive.Supports(capability) {
			return true
		}
	}
	return false
}

func (h *Harness) record(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, name)
}

func (h *Harness) bind(actionID string, request any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bindLocked(actionID, request)
}

func (h *Harness) bindLocked(actionID string, request any) error {
	encoded, err := json.Marshal(request)
	if err != nil {
		return agentharness.ErrInvalidRequest
	}
	sum := sha256.Sum256(encoded)
	fingerprint := fmt.Sprintf("%x", sum[:])
	if existing, ok := h.requests[actionID]; ok {
		if existing != fingerprint {
			return agentharness.ErrActionConflict
		}
		return nil
	}
	h.requests[actionID] = fingerprint
	return nil
}

func allCapabilities() agentharness.CapabilitySnapshot {
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
				ConcurrencyLimit: 4,
			},
			agentharness.EphemeralSubagent: {
				Primitive: agentharness.EphemeralSubagent,
				Capabilities: agentharness.CapabilitySet(
					agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait,
					agentharness.CapRuntimeIdentity, agentharness.CapRuntimeClose,
				),
				ConcurrencyLimit: 8,
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

func cloneCapabilities(source agentharness.CapabilitySnapshot) agentharness.CapabilitySnapshot {
	result := source
	result.Primitives = make(map[agentharness.Primitive]agentharness.PrimitiveCapabilities, len(source.Primitives))
	for key, value := range source.Primitives {
		cloned := value
		cloned.Capabilities = make(map[agentharness.Capability]bool, len(value.Capabilities))
		for capability, supported := range value.Capabilities {
			cloned.Capabilities[capability] = supported
		}
		result.Primitives[key] = cloned
	}
	return result
}

func digest(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
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
