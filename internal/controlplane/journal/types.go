// Package journal defines the provider-neutral authoritative action and event
// boundary for the Pajé control plane.
package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const SchemaVersion uint32 = 1

const MaxPayloadBytes = 1 << 20

var (
	ErrConflict      = errors.New("control journal conflict")
	ErrInvalidRecord = errors.New("invalid control journal record")
	ErrCursor        = errors.New("invalid control journal cursor")
	ErrNotFound      = errors.New("control journal record not found")
)

type Kind string

const (
	KindDispatch         Kind = "dispatch"
	KindRegisterRuntime  Kind = "register_runtime"
	KindSend             Kind = "send"
	KindAcknowledge      Kind = "acknowledge"
	KindCallback         Kind = "callback"
	KindObserve          Kind = "observe"
	KindWait             Kind = "wait"
	KindInterrupt        Kind = "interrupt"
	KindCancel           Kind = "cancel"
	KindAllocateResource Kind = "allocate_resource"
	KindDisposeResource  Kind = "dispose_resource"
	KindVerifyCandidate  Kind = "verify_candidate"
	KindRunGate          Kind = "run_gate"
	KindIntegrate        Kind = "integrate"
	KindPublish          Kind = "publish"
	KindVerifyTargetTree Kind = "verify_target_tree"
	KindCloseRuntime     Kind = "close_runtime"
	KindArchiveSession   Kind = "archive_session"
	KindCloseRun         Kind = "close_run"
)

type EventKind string

const (
	EventActionReserved     EventKind = "action_reserved"
	EventActionResult       EventKind = "result_bound"
	EventActionNotPerformed EventKind = "not_performed"
	EventActionSuperseded   EventKind = "superseded"
	EventActionAmbiguous    EventKind = "unresolved_ambiguity"
	EventProjectionUpdated  EventKind = "projection_updated"
	EventMigrationStarted   EventKind = "migration_started"
	EventMigrationGraph     EventKind = "migration_graph"
	EventMigrationAttempt   EventKind = "migration_attempt"
	EventMigrationSession   EventKind = "migration_session"
	EventMigrationAction    EventKind = "migration_action"
	EventMigrationEvidence  EventKind = "migration_evidence"
	EventMigrationMessage   EventKind = "migration_message"
	EventMigrationCallback  EventKind = "migration_callback"
	EventMigrationHandoff   EventKind = "migration_handoff"
	EventMigrationClose     EventKind = "migration_close"
	EventMigrationSnapshot  EventKind = "migration_snapshot"
	EventMigrationCompleted EventKind = "migration_completed"
)

type Action struct {
	ID                     string `json:"id"`
	ControlRunID           string `json:"control_run_id"`
	TaskID                 string `json:"task_id,omitempty"`
	AttemptID              string `json:"attempt_id,omitempty"`
	Kind                   Kind   `json:"kind"`
	GraphRevision          uint64 `json:"graph_revision"`
	ExpectedProjection     uint64 `json:"expected_projection"`
	CanonicalRequestDigest string `json:"canonical_request_digest"`
	IdempotencyKey         string `json:"idempotency_key"`
	AuthorityReceiptID     string `json:"authority_receipt_id,omitempty"`
}

type JournalPosition uint64

type RunCursor struct {
	InstallationID string `json:"installation_id"`
	ControlRunID   string `json:"control_run_id"`
	SchemaVersion  uint32 `json:"schema_version"`
	RunSequence    uint64 `json:"run_sequence"`
}

type GlobalCursor struct {
	InstallationID  string          `json:"installation_id"`
	SchemaVersion   uint32          `json:"schema_version"`
	JournalPosition JournalPosition `json:"journal_position"`
}

type Event struct {
	ID              string          `json:"id"`
	ControlRunID    string          `json:"control_run_id"`
	RunSequence     uint64          `json:"run_sequence"`
	JournalPosition JournalPosition `json:"journal_position"`
	ActionID        string          `json:"action_id,omitempty"`
	Kind            EventKind       `json:"kind"`
	PayloadDigest   string          `json:"payload_digest"`
	ProviderReceipt string          `json:"provider_receipt,omitempty"`
	OccurredAt      time.Time       `json:"occurred_at"`
}

// CommitRequest is the authoritative transaction boundary used by semantic
// control-plane services. The payloads are exact canonical JSON bytes whose
// digests are bound by Action and Outcome.
type CommitRequest struct {
	Action         Action       `json:"action"`
	ExpectedRun    RunCursor    `json:"expected_run"`
	ExpectedGlobal GlobalCursor `json:"expected_global"`
	RequestPayload []byte       `json:"request_payload"`
	Outcome        Event        `json:"outcome"`
	OutcomePayload []byte       `json:"outcome_payload"`
}

type CommitReceipt struct {
	Action      Action `json:"action"`
	Reservation Event  `json:"reservation"`
	Outcome     Event  `json:"outcome"`
	Created     bool   `json:"created"`
}

func NewRunCursor(installationID, controlRunID string) RunCursor {
	return RunCursor{
		InstallationID: installationID,
		ControlRunID:   controlRunID,
		SchemaVersion:  SchemaVersion,
	}
}

func NewGlobalCursor(installationID string) GlobalCursor {
	return GlobalCursor{InstallationID: installationID, SchemaVersion: SchemaVersion}
}

func ValidateAction(action Action) error {
	if strings.TrimSpace(action.ID) == "" || strings.TrimSpace(action.ControlRunID) == "" ||
		!validKind(action.Kind) || action.GraphRevision == 0 ||
		!ValidDigest(action.CanonicalRequestDigest) ||
		strings.TrimSpace(action.IdempotencyKey) == "" {
		return fmt.Errorf("%w: action identity, scope, revision, digest, and key are required", ErrInvalidRecord)
	}
	if len(action.ID) > 128 || len(action.ControlRunID) > 128 || len(action.TaskID) > 128 ||
		len(action.AttemptID) > 128 || len(action.IdempotencyKey) > 256 ||
		len(action.AuthorityReceiptID) > 128 {
		return fmt.Errorf("%w: action fields exceed their bounds", ErrInvalidRecord)
	}
	if action.AttemptID != "" && action.TaskID == "" {
		return fmt.Errorf("%w: attempt-scoped action requires a task", ErrInvalidRecord)
	}
	return nil
}

func ValidateEvent(event Event, assigned bool) error {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.ControlRunID) == "" ||
		!validEventKind(event.Kind) || !ValidDigest(event.PayloadDigest) ||
		event.OccurredAt.IsZero() {
		return fmt.Errorf("%w: event identity, scope, kind, digest, and time are required", ErrInvalidRecord)
	}
	if len(event.ID) > 128 || len(event.ControlRunID) > 128 || len(event.ActionID) > 128 ||
		len(event.ProviderReceipt) > 1024 {
		return fmt.Errorf("%w: event fields exceed their bounds", ErrInvalidRecord)
	}
	if assigned {
		if event.RunSequence == 0 || event.JournalPosition == 0 {
			return fmt.Errorf("%w: assigned event positions are required", ErrInvalidRecord)
		}
	} else if event.RunSequence != 0 || event.JournalPosition != 0 {
		return fmt.Errorf("%w: caller cannot assign event positions", ErrInvalidRecord)
	}
	if outcomeKind(event.Kind) && strings.TrimSpace(event.ActionID) == "" {
		return fmt.Errorf("%w: action outcome is not reservation-bound", ErrInvalidRecord)
	}
	if event.Kind == EventActionReserved && strings.TrimSpace(event.ActionID) == "" {
		return fmt.Errorf("%w: reservation event has no action", ErrInvalidRecord)
	}
	return nil
}

// ValidateCommitRequest validates every caller-controlled byte before a store
// acquires durable state. Store implementations additionally compare the
// cursors with their immutable installation and current heads.
func ValidateCommitRequest(request CommitRequest) error {
	if err := ValidateAction(request.Action); err != nil {
		return err
	}
	if strings.TrimSpace(request.ExpectedRun.InstallationID) == "" ||
		len(request.ExpectedRun.InstallationID) > 128 ||
		len(request.ExpectedGlobal.InstallationID) > 128 ||
		request.ExpectedRun.InstallationID != request.ExpectedGlobal.InstallationID ||
		request.ExpectedRun.ControlRunID != request.Action.ControlRunID ||
		request.ExpectedRun.SchemaVersion != SchemaVersion ||
		request.ExpectedGlobal.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: commit cursors do not bind one installation and run", ErrInvalidRecord)
	}
	if request.Outcome.ControlRunID != request.Action.ControlRunID ||
		request.Outcome.ActionID != request.Action.ID {
		return fmt.Errorf("%w: commit outcome is not bound to its action", ErrInvalidRecord)
	}
	if err := ValidateEvent(request.Outcome, false); err != nil {
		return err
	}
	switch request.Outcome.Kind {
	case EventActionResult, EventActionNotPerformed, EventActionAmbiguous:
	default:
		return fmt.Errorf("%w: commit requires an initial terminal outcome", ErrInvalidRecord)
	}
	if err := ValidatePayload(request.RequestPayload, request.Action.CanonicalRequestDigest); err != nil {
		return err
	}
	if err := ValidatePayload(request.OutcomePayload, request.Outcome.PayloadDigest); err != nil {
		return err
	}
	return nil
}

// ValidatePayload requires one bounded canonical JSON value terminated by the
// newline emitted by CanonicalJSON and bound to the supplied SHA-256 digest.
func ValidatePayload(encoded []byte, wantDigest string) error {
	if len(encoded) == 0 || len(encoded) > MaxPayloadBytes || !ValidDigest(wantDigest) {
		return fmt.Errorf("%w: payload size or digest is invalid", ErrInvalidRecord)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%w: decode payload JSON: %v", ErrInvalidRecord, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing payload JSON", ErrInvalidRecord)
	}
	canonical, err := CanonicalJSON(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(encoded, canonical) {
		return fmt.Errorf("%w: payload is not canonical JSON", ErrInvalidRecord)
	}
	sum := sha256.Sum256(encoded)
	if "sha256:"+hex.EncodeToString(sum[:]) != wantDigest {
		return fmt.Errorf("%w: payload digest mismatch", ErrInvalidRecord)
	}
	return nil
}

func IsOutcome(kind EventKind) bool {
	return outcomeKind(kind)
}

// ValidateOutcomeTransition enforces the reservation outcome state machine.
// Ambiguity may resolve to an exact result or not-performed proof, and only a
// not-performed action may be superseded by an authorized new generation.
func ValidateOutcomeTransition(prior []Event, next Event) error {
	if !IsOutcome(next.Kind) {
		return nil
	}
	var state EventKind
	for _, event := range prior {
		if event.ActionID != next.ActionID || !IsOutcome(event.Kind) {
			continue
		}
		switch {
		case state == "" && (event.Kind == EventActionResult ||
			event.Kind == EventActionNotPerformed || event.Kind == EventActionAmbiguous):
			state = event.Kind
		case state == EventActionAmbiguous &&
			(event.Kind == EventActionResult || event.Kind == EventActionNotPerformed):
			state = event.Kind
		case state == EventActionNotPerformed && event.Kind == EventActionSuperseded:
			state = event.Kind
		default:
			return fmt.Errorf("%w: invalid prior action outcome history", ErrInvalidRecord)
		}
	}
	valid := state == "" && (next.Kind == EventActionResult ||
		next.Kind == EventActionNotPerformed || next.Kind == EventActionAmbiguous) ||
		state == EventActionAmbiguous &&
			(next.Kind == EventActionResult || next.Kind == EventActionNotPerformed) ||
		state == EventActionNotPerformed && next.Kind == EventActionSuperseded
	if !valid {
		return ErrConflict
	}
	return nil
}

func ValidDigest(value string) bool {
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

func CanonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: encode canonical JSON: %v", ErrInvalidRecord, err)
	}
	return append(encoded, '\n'), nil
}

func DecodeStrict(encoded []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: decode strict JSON: %v", ErrInvalidRecord, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON data", ErrInvalidRecord)
	}
	return nil
}

func Digest(value any) (string, error) {
	encoded, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validKind(kind Kind) bool {
	switch kind {
	case KindDispatch, KindRegisterRuntime, KindSend, KindAcknowledge, KindCallback, KindObserve, KindWait,
		KindInterrupt, KindCancel, KindAllocateResource, KindDisposeResource,
		KindVerifyCandidate, KindRunGate, KindIntegrate, KindPublish,
		KindVerifyTargetTree, KindCloseRuntime, KindArchiveSession, KindCloseRun:
		return true
	default:
		return false
	}
}

func validEventKind(kind EventKind) bool {
	switch kind {
	case EventActionReserved, EventActionResult, EventActionNotPerformed,
		EventActionSuperseded, EventActionAmbiguous, EventProjectionUpdated,
		EventMigrationStarted, EventMigrationGraph, EventMigrationAttempt,
		EventMigrationSession, EventMigrationAction, EventMigrationEvidence,
		EventMigrationMessage, EventMigrationCallback, EventMigrationHandoff,
		EventMigrationClose, EventMigrationSnapshot, EventMigrationCompleted:
		return true
	default:
		return false
	}
}

func outcomeKind(kind EventKind) bool {
	switch kind {
	case EventActionResult, EventActionNotPerformed, EventActionSuperseded, EventActionAmbiguous:
		return true
	default:
		return false
	}
}
