// Package agentharness defines the provider-neutral lifecycle used to place
// and control agent work. It deliberately contains no runtime tool names,
// executable commands, credentials, or provider-native values.
package agentharness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalidRequest            = errors.New("invalid agent harness request")
	ErrInvalidCapabilities       = errors.New("invalid agent harness capabilities")
	ErrUnsupportedOperation      = errors.New("unsupported agent harness operation")
	ErrActionMismatch            = errors.New("agent harness action mismatch")
	ErrActionConflict            = errors.New("agent harness action conflict")
	ErrActionOutcomeUnknown      = errors.New("agent harness action outcome unknown")
	ErrUnexpectedRuntimeIdentity = errors.New("unexpected runtime identity")
	ErrCursorRegression          = errors.New("agent harness cursor regression")
	ErrProviderUnavailable       = errors.New("agent harness provider unavailable")
)

type Primitive string

const (
	PersistentSession     Primitive = "persistent_session"
	EphemeralSubagent     Primitive = "ephemeral_subagent"
	HarnessNativeParallel Primitive = "harness_native_parallel"
	LocalSequential       Primitive = "local_sequential"
)

// Capability is an alias so durable JSON models can expose capabilities
// without conversion while constants still document the supported vocabulary.
type Capability = string

const (
	CapDispatch                 Capability = "dispatch"
	CapObserve                  Capability = "observe"
	CapWait                     Capability = "wait"
	CapRuntimeIdentity          Capability = "runtime_identity"
	CapAcknowledge              Capability = "acknowledge"
	CapSend                     Capability = "send"
	CapCallback                 Capability = "callback"
	CapCursor                   Capability = "cursor"
	CapInterrupt                Capability = "interrupt"
	CapIdempotency              Capability = "idempotency"
	CapRestart                  Capability = "restart"
	CapRuntimeClose             Capability = "runtime_close"
	CapArchive                  Capability = "persistent_archive"
	CapDeterministicAggregation Capability = "deterministic_aggregation"
	CapIsolation                Capability = "isolation"
	CapLocal                    Capability = "local"
)

type ActionKind string

const (
	ActionDispatch    ActionKind = "dispatch"
	ActionObserve     ActionKind = "observe"
	ActionSend        ActionKind = "send"
	ActionWait        ActionKind = "wait"
	ActionInterrupt   ActionKind = "interrupt"
	ActionClose       ActionKind = "close"
	ActionAcknowledge ActionKind = "acknowledge"
	ActionCallback    ActionKind = "callback"
)

type CloseKind string

const (
	CloseArchive   CloseKind = "archive"
	CloseRuntime   CloseKind = "runtime_close"
	CloseAggregate CloseKind = "aggregate"
	CloseCanceled  CloseKind = "cancel"
	CloseInactive  CloseKind = "inactive"
)

type Principal struct {
	ID string `json:"id"`
}

type PrimitiveCapabilities struct {
	Primitive        Primitive           `json:"primitive"`
	Capabilities     map[Capability]bool `json:"capabilities"`
	ConcurrencyLimit int                 `json:"concurrency_limit"`
}

func (c PrimitiveCapabilities) Supports(capability Capability) bool {
	return c.Capabilities[capability]
}

func (c PrimitiveCapabilities) Validate() error {
	if !ValidPrimitive(c.Primitive) {
		return fmt.Errorf("%w: unknown primitive %q", ErrInvalidCapabilities, c.Primitive)
	}
	if c.ConcurrencyLimit < 0 {
		return fmt.Errorf("%w: negative concurrency limit", ErrInvalidCapabilities)
	}
	for name := range c.Capabilities {
		if !validCapability(name) {
			return fmt.Errorf("%w: unknown capability %q", ErrInvalidCapabilities, name)
		}
	}
	for _, required := range RequiredCapabilities(c.Primitive) {
		if !c.Supports(required) {
			return fmt.Errorf("%w: %s requires %s", ErrInvalidCapabilities, c.Primitive, required)
		}
	}
	return nil
}

type CapabilitySnapshot struct {
	HarnessID  string                              `json:"harness_id"`
	Revision   string                              `json:"revision,omitempty"`
	Primitives map[Primitive]PrimitiveCapabilities `json:"primitives"`
}

// HarnessCapabilities is the runtime discovery result. CapabilitySnapshot is
// its durable representation and the two intentionally have the same shape.
type HarnessCapabilities = CapabilitySnapshot

func (c CapabilitySnapshot) Validate() error {
	if strings.TrimSpace(c.HarnessID) == "" {
		return fmt.Errorf("%w: harness ID is required", ErrInvalidCapabilities)
	}
	if len(c.Primitives) == 0 {
		return fmt.Errorf("%w: at least one primitive is required", ErrInvalidCapabilities)
	}
	for primitive, capabilities := range c.Primitives {
		if primitive != capabilities.Primitive {
			return fmt.Errorf("%w: primitive key mismatch", ErrInvalidCapabilities)
		}
		if err := capabilities.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func CapabilitySet(capabilities ...Capability) map[Capability]bool {
	result := make(map[Capability]bool, len(capabilities))
	for _, capability := range capabilities {
		result[capability] = true
	}
	return result
}

func RequiredCapabilities(primitive Primitive) []Capability {
	switch primitive {
	case PersistentSession:
		return []Capability{
			CapDispatch, CapObserve, CapWait, CapRuntimeIdentity, CapAcknowledge,
			CapSend, CapCallback, CapCursor, CapInterrupt, CapIdempotency,
			CapRestart, CapArchive, CapIsolation,
		}
	case EphemeralSubagent:
		return []Capability{CapDispatch, CapObserve, CapWait, CapRuntimeClose}
	case HarnessNativeParallel:
		return []Capability{CapDispatch, CapWait, CapInterrupt, CapDeterministicAggregation}
	case LocalSequential:
		return []Capability{CapLocal}
	default:
		return nil
	}
}

func ValidPrimitive(primitive Primitive) bool {
	switch primitive {
	case PersistentSession, EphemeralSubagent, HarnessNativeParallel, LocalSequential:
		return true
	default:
		return false
	}
}

type DispatchWorkRequest struct {
	ActionID            string        `json:"action_id"`
	RequestDigest       string        `json:"request_digest"`
	ControlRunID        string        `json:"control_run_id"`
	TaskID              string        `json:"task_id"`
	AttemptID           string        `json:"attempt_id"`
	Primitive           Primitive     `json:"primitive"`
	ProjectRefIDs       []string      `json:"project_ref_ids"`
	PromptDigest        string        `json:"prompt_digest"`
	OwnershipDigest     string        `json:"ownership_digest"`
	FrozenInputDigest   string        `json:"frozen_input_digest"`
	ExpectedItemDigests []string      `json:"expected_item_digests,omitempty"`
	Timeout             time.Duration `json:"timeout"`
}

type DispatchWorkResult struct {
	ActionID       string   `json:"action_id"`
	RuntimeWorkIDs []string `json:"runtime_work_ids,omitempty"`
	Cursor         string   `json:"cursor,omitempty"`
	CursorSequence uint64   `json:"cursor_sequence,omitempty"`
	ResultDigest   string   `json:"result_digest"`
	Diagnostic     string   `json:"diagnostic,omitempty"`
}

type ObserveWorkRequest struct {
	ActionID               string `json:"action_id"`
	ControlRunID           string `json:"control_run_id"`
	TaskID                 string `json:"task_id"`
	AttemptID              string `json:"attempt_id"`
	AfterCursor            string `json:"after_cursor,omitempty"`
	AfterCursorSequence    uint64 `json:"after_cursor_sequence,omitempty"`
	ReconcileActionID      string `json:"reconcile_action_id,omitempty"`
	ReconcileRequestDigest string `json:"reconcile_request_digest,omitempty"`
}

type WorkEvent struct {
	ID             string `json:"id"`
	RuntimeWorkID  string `json:"runtime_work_id,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
	CursorSequence uint64 `json:"cursor_sequence,omitempty"`
	Kind           string `json:"kind"`
	ResultDigest   string `json:"result_digest"`
	Terminal       bool   `json:"terminal"`
}

type WorkEvents struct {
	ActionID           string        `json:"action_id"`
	Events             []WorkEvent   `json:"events"`
	NextCursor         string        `json:"next_cursor,omitempty"`
	NextCursorSequence uint64        `json:"next_cursor_sequence,omitempty"`
	Terminal           bool          `json:"terminal"`
	ReconciledResult   *ActionResult `json:"reconciled_result,omitempty"`
}

type SendWorkRequest struct {
	ActionID      string `json:"action_id"`
	ControlRunID  string `json:"control_run_id"`
	TaskID        string `json:"task_id"`
	AttemptID     string `json:"attempt_id"`
	RuntimeWorkID string `json:"runtime_work_id,omitempty"`
	MessageDigest string `json:"message_digest"`
}

type MessageReceipt struct {
	ActionID       string `json:"action_id"`
	Receipt        string `json:"receipt"`
	Cursor         string `json:"cursor,omitempty"`
	CursorSequence uint64 `json:"cursor_sequence,omitempty"`
}

type WaitWorkRequest struct {
	ActionID            string        `json:"action_id"`
	ControlRunID        string        `json:"control_run_id"`
	AttemptIDs          []string      `json:"attempt_ids"`
	AfterCursor         string        `json:"after_cursor,omitempty"`
	AfterCursorSequence uint64        `json:"after_cursor_sequence,omitempty"`
	Timeout             time.Duration `json:"timeout"`
}

type InterruptWorkRequest struct {
	ActionID       string   `json:"action_id"`
	ControlRunID   string   `json:"control_run_id"`
	TaskID         string   `json:"task_id"`
	AttemptID      string   `json:"attempt_id"`
	RuntimeWorkIDs []string `json:"runtime_work_ids,omitempty"`
}

type InterruptReceipt struct {
	ActionID       string `json:"action_id"`
	Receipt        string `json:"receipt"`
	Cursor         string `json:"cursor,omitempty"`
	CursorSequence uint64 `json:"cursor_sequence,omitempty"`
}

type CloseWorkRequest struct {
	ActionID       string    `json:"action_id"`
	ControlRunID   string    `json:"control_run_id"`
	TaskID         string    `json:"task_id"`
	AttemptID      string    `json:"attempt_id"`
	Primitive      Primitive `json:"primitive"`
	RuntimeWorkIDs []string  `json:"runtime_work_ids,omitempty"`
	ResultDigests  []string  `json:"result_digests,omitempty"`
}

type CloseEvidence struct {
	Kind    CloseKind `json:"kind"`
	Receipt string    `json:"receipt"`
	Digest  string    `json:"digest,omitempty"`
}

type WorkCloseEvidence struct {
	ActionID string        `json:"action_id"`
	Evidence CloseEvidence `json:"evidence"`
}

type ActionResult struct {
	ActionID       string        `json:"action_id"`
	RuntimeWorkIDs []string      `json:"runtime_work_ids,omitempty"`
	Cursor         string        `json:"cursor,omitempty"`
	CursorSequence uint64        `json:"cursor_sequence,omitempty"`
	Events         []WorkEvent   `json:"events,omitempty"`
	ResultDigest   string        `json:"result_digest"`
	MessageReceipt string        `json:"message_receipt,omitempty"`
	CloseEvidence  CloseEvidence `json:"close_evidence,omitempty"`
	Diagnostic     string        `json:"diagnostic,omitempty"`
}

func ValidateActionResult(
	kind ActionKind,
	primitive Primitive,
	expectedRuntimeWorkIDs []string,
	previousCursorSequence uint64,
	cursorRequired bool,
	result ActionResult,
) error {
	if strings.TrimSpace(result.ActionID) == "" || !validDigest(result.ResultDigest) ||
		!ValidPrimitive(primitive) || !validAction(kind) {
		return ErrActionMismatch
	}
	if (result.Cursor == "") != (result.CursorSequence == 0) {
		return ErrActionMismatch
	}
	if cursorRequired && (kind == ActionObserve || kind == ActionWait) && result.Cursor == "" {
		return ErrActionMismatch
	}
	if result.CursorSequence > 0 && result.CursorSequence <= previousCursorSequence {
		return ErrCursorRegression
	}
	seenRuntimeIDs := make(map[string]bool, len(result.RuntimeWorkIDs))
	for _, runtimeID := range result.RuntimeWorkIDs {
		if strings.TrimSpace(runtimeID) == "" || strings.ContainsAny(runtimeID, "\r\n\x00") || seenRuntimeIDs[runtimeID] {
			return ErrUnexpectedRuntimeIdentity
		}
		seenRuntimeIDs[runtimeID] = true
	}
	if kind != ActionDispatch && !sameStringSlice(expectedRuntimeWorkIDs, result.RuntimeWorkIDs) {
		return ErrUnexpectedRuntimeIdentity
	}
	if len(result.Events) > 0 && kind != ActionObserve && kind != ActionWait {
		return ErrActionMismatch
	}
	seenEvents := make(map[string]string, len(result.Events))
	sequence := previousCursorSequence
	for _, event := range result.Events {
		if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.Kind) == "" ||
			!validDigest(event.ResultDigest) || (event.Cursor == "") != (event.CursorSequence == 0) {
			return ErrActionMismatch
		}
		if cursorRequired && event.Cursor == "" {
			return ErrActionMismatch
		}
		if previous, duplicate := seenEvents[event.ID]; duplicate {
			if previous != event.ResultDigest {
				return ErrActionConflict
			}
			return ErrActionMismatch
		}
		seenEvents[event.ID] = event.ResultDigest
		if event.CursorSequence > 0 {
			if event.CursorSequence <= sequence {
				return ErrCursorRegression
			}
			sequence = event.CursorSequence
		}
		if len(expectedRuntimeWorkIDs) > 0 &&
			(event.RuntimeWorkID == "" || !containsString(expectedRuntimeWorkIDs, event.RuntimeWorkID)) {
			return ErrUnexpectedRuntimeIdentity
		}
		if len(expectedRuntimeWorkIDs) == 0 && event.RuntimeWorkID != "" {
			return ErrUnexpectedRuntimeIdentity
		}
	}
	if len(result.Events) > 0 {
		last := result.Events[len(result.Events)-1]
		if last.Cursor != "" && (result.Cursor != last.Cursor || result.CursorSequence != last.CursorSequence) {
			return ErrActionMismatch
		}
	}
	if kind == ActionClose {
		if err := ValidateCloseEvidence(primitive, result.CloseEvidence); err != nil {
			return err
		}
	} else if result.CloseEvidence != (CloseEvidence{}) {
		return ErrActionMismatch
	}
	return nil
}

func ValidateCloseEvidence(primitive Primitive, evidence CloseEvidence) error {
	if strings.TrimSpace(evidence.Receipt) == "" || !validDigest(evidence.Digest) {
		return ErrActionMismatch
	}
	switch primitive {
	case PersistentSession:
		if evidence.Kind != CloseArchive {
			return ErrActionMismatch
		}
	case EphemeralSubagent:
		if evidence.Kind != CloseRuntime {
			return ErrActionMismatch
		}
	case HarnessNativeParallel:
		if evidence.Kind != CloseAggregate && evidence.Kind != CloseCanceled {
			return ErrActionMismatch
		}
	case LocalSequential:
		if evidence.Kind != CloseInactive {
			return ErrActionMismatch
		}
	default:
		return ErrUnsupportedOperation
	}
	return nil
}

type AgentHarness interface {
	Capabilities(context.Context, Principal, string) (HarnessCapabilities, error)
	Dispatch(context.Context, DispatchWorkRequest) (DispatchWorkResult, error)
	Observe(context.Context, ObserveWorkRequest) (WorkEvents, error)
	Send(context.Context, SendWorkRequest) (MessageReceipt, error)
	Wait(context.Context, WaitWorkRequest) (WorkEvents, error)
	Interrupt(context.Context, InterruptWorkRequest) (InterruptReceipt, error)
	Close(context.Context, CloseWorkRequest) (WorkCloseEvidence, error)
}

func StableActionID(
	controlRunID, taskID, attemptID string,
	graphRevision uint64,
	primitive Primitive,
	kind ActionKind,
	requestDigest string,
) (string, error) {
	if strings.TrimSpace(controlRunID) == "" || strings.TrimSpace(taskID) == "" ||
		strings.TrimSpace(attemptID) == "" || graphRevision == 0 ||
		!ValidPrimitive(primitive) || !validAction(kind) || !validDigest(requestDigest) {
		return "", ErrInvalidRequest
	}
	parts := []string{
		"paje-agent-action-v1", controlRunID, taskID, attemptID,
		fmt.Sprintf("%d", graphRevision), string(primitive), string(kind), requestDigest,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "action_" + hex.EncodeToString(sum[:]), nil
}

func CanonicalResultDigest(result ActionResult) (string, error) {
	cloned := result
	cloned.Diagnostic = SafeDiagnostic(cloned.Diagnostic)
	cloned.RuntimeWorkIDs = append([]string(nil), result.RuntimeWorkIDs...)
	sort.Strings(cloned.RuntimeWorkIDs)
	encoded, err := json.Marshal(cloned)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func SafeDiagnostic(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if len(value) > 1024 {
		value = value[:1024]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func (r DispatchWorkRequest) Validate() error {
	if !validBinding(r.ActionID, r.ControlRunID, r.TaskID, r.AttemptID) ||
		!ValidPrimitive(r.Primitive) ||
		len(r.ProjectRefIDs) == 0 || !validDigest(r.RequestDigest) || !validDigest(r.PromptDigest) ||
		!validDigest(r.OwnershipDigest) || !validDigest(r.FrozenInputDigest) ||
		r.Timeout <= 0 {
		return ErrInvalidRequest
	}
	if r.Primitive == HarnessNativeParallel && len(r.ExpectedItemDigests) == 0 {
		return ErrInvalidRequest
	}
	for _, digest := range r.ExpectedItemDigests {
		if !validDigest(digest) {
			return ErrInvalidRequest
		}
	}
	return nil
}

func (r ObserveWorkRequest) Validate() error {
	if !validBinding(r.ActionID, r.ControlRunID, r.TaskID, r.AttemptID) {
		return ErrInvalidRequest
	}
	if (r.AfterCursor == "") != (r.AfterCursorSequence == 0) {
		return ErrInvalidRequest
	}
	if (r.ReconcileActionID == "") != (r.ReconcileRequestDigest == "") {
		return ErrInvalidRequest
	}
	if r.ReconcileActionID != "" && (!validBinding(r.ReconcileActionID) || !validDigest(r.ReconcileRequestDigest)) {
		return ErrInvalidRequest
	}
	return nil
}

func (r SendWorkRequest) Validate() error {
	if !validBinding(r.ActionID, r.ControlRunID, r.TaskID, r.AttemptID) ||
		!validDigest(r.MessageDigest) {
		return ErrInvalidRequest
	}
	return nil
}

func (r WaitWorkRequest) Validate() error {
	if strings.TrimSpace(r.ActionID) == "" || strings.TrimSpace(r.ControlRunID) == "" ||
		len(r.AttemptIDs) == 0 || r.Timeout <= 0 {
		return ErrInvalidRequest
	}
	if (r.AfterCursor == "") != (r.AfterCursorSequence == 0) {
		return ErrInvalidRequest
	}
	return nil
}

func (r InterruptWorkRequest) Validate() error {
	if !validBinding(r.ActionID, r.ControlRunID, r.TaskID, r.AttemptID) {
		return ErrInvalidRequest
	}
	return nil
}

func (r CloseWorkRequest) Validate() error {
	if !validBinding(r.ActionID, r.ControlRunID, r.TaskID, r.AttemptID) ||
		!ValidPrimitive(r.Primitive) || r.Primitive == LocalSequential {
		return ErrInvalidRequest
	}
	return nil
}

func OperationCapability(kind ActionKind, primitive Primitive) (Capability, error) {
	switch kind {
	case ActionDispatch:
		if primitive == LocalSequential {
			return "", ErrUnsupportedOperation
		}
		return CapDispatch, nil
	case ActionObserve:
		return CapObserve, nil
	case ActionSend:
		return CapSend, nil
	case ActionWait:
		return CapWait, nil
	case ActionInterrupt:
		return CapInterrupt, nil
	case ActionAcknowledge:
		return CapAcknowledge, nil
	case ActionCallback:
		return CapCallback, nil
	case ActionClose:
		switch primitive {
		case PersistentSession:
			return CapArchive, nil
		case EphemeralSubagent:
			return CapRuntimeClose, nil
		case HarnessNativeParallel:
			return CapDeterministicAggregation, nil
		case LocalSequential:
			return CapLocal, nil
		}
	}
	return "", ErrUnsupportedOperation
}

func validCapability(capability Capability) bool {
	switch capability {
	case CapDispatch, CapObserve, CapWait, CapRuntimeIdentity, CapAcknowledge,
		CapSend, CapCallback, CapCursor, CapInterrupt, CapIdempotency, CapRestart,
		CapRuntimeClose, CapArchive, CapDeterministicAggregation, CapIsolation, CapLocal:
		return true
	default:
		return false
	}
}

func validAction(kind ActionKind) bool {
	switch kind {
	case ActionDispatch, ActionObserve, ActionSend, ActionWait, ActionInterrupt,
		ActionClose, ActionAcknowledge, ActionCallback:
		return true
	default:
		return false
	}
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func validBinding(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return false
		}
	}
	return true
}

func sameStringSlice(first, second []string) bool {
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
