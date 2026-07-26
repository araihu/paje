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
