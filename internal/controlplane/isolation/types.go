// Package isolation owns journal-backed control-run scopes, operational phases,
// inbox facts, and pending-work wake gates.
package isolation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
)

var (
	ErrInvalidScope      = errors.New("invalid control-run scope")
	ErrInvalidOperation  = errors.New("invalid isolation operation")
	ErrInvalidTransition = errors.New("invalid operational phase transition")
	ErrRevisionConflict  = errors.New("isolation projection revision conflict")
	ErrNotFound          = errors.New("isolation record not found")
	ErrResolverAuthority = errors.New("pending-work gate resolver authority mismatch")
	ErrWakeFactMissing   = errors.New("pending-work gate wake fact missing")
	ErrWakeNotReady      = errors.New("pending-work gate wake time not reached")
	ErrGateResolved      = errors.New("pending-work gate already resolved")
)

type RunScope struct {
	InstallationID string `json:"installation_id"`
	ControlRunID   string `json:"control_run_id"`
}

func NewRunScope(installationID, controlRunID string) (RunScope, error) {
	scope := RunScope{InstallationID: installationID, ControlRunID: controlRunID}
	if err := validateRunScope(scope); err != nil {
		return RunScope{}, err
	}
	return scope, nil
}

func (s RunScope) Key() string {
	return scopedID("run", s.InstallationID, s.ControlRunID)
}

type ProjectScope struct {
	Run                 RunScope `json:"run"`
	ProjectID           string   `json:"project_id"`
	CanonicalRepository string   `json:"canonical_repository"`
	BaseSHA             string   `json:"base_sha"`
}

func NewProjectScope(
	run RunScope,
	projectID, canonicalRepository, baseSHA string,
) (ProjectScope, error) {
	scope := ProjectScope{
		Run:                 run,
		ProjectID:           projectID,
		CanonicalRepository: canonicalRepository,
		BaseSHA:             baseSHA,
	}
	if err := validateProjectScope(scope); err != nil {
		return ProjectScope{}, err
	}
	return scope, nil
}

func (s ProjectScope) Key() string {
	return scopedID(
		"project",
		s.Run.InstallationID,
		s.Run.ControlRunID,
		s.ProjectID,
		s.CanonicalRepository,
		s.BaseSHA,
	)
}

// CredentialScope intentionally retains only an opaque, scope-derived
// identity. Clear credential material and provider handles are never exposed
// through this value or admitted to the journal.
type CredentialScope struct {
	id string
}

func NewCredentialScope(
	project ProjectScope,
	purpose, opaqueHandleID, policyDigest string,
) (CredentialScope, error) {
	if err := validateProjectScope(project); err != nil ||
		!validIdentifier(purpose, 128) ||
		!validOpaqueHandle(opaqueHandleID) ||
		!journal.ValidDigest(policyDigest) {
		return CredentialScope{}, ErrInvalidScope
	}
	return CredentialScope{id: scopedID(
		"credential",
		project.Key(),
		purpose,
		opaqueHandleID,
		policyDigest,
	)}, nil
}

func (s CredentialScope) ID() string {
	return s.id
}

type TaskPhase string

const (
	TaskDiscovered        TaskPhase = "DISCOVERED"
	TaskAuditingReadOnly  TaskPhase = "AUDITING_READ_ONLY"
	TaskReadyForOwnership TaskPhase = "READY_FOR_OWNERSHIP"
	TaskOwned             TaskPhase = "OWNED"
	TaskExecuting         TaskPhase = "EXECUTING"
	TaskVerifying         TaskPhase = "VERIFYING"
	TaskAccepted          TaskPhase = "ACCEPTED"
	TaskDeferred          TaskPhase = "DEFERRED"
	TaskNeedsInput        TaskPhase = "NEEDS_INPUT"
	TaskRollbackRequired  TaskPhase = "ROLLBACK_REQUIRED"
	TaskFailed            TaskPhase = "FAILED"
)

type RunPhase string

const (
	RunActive         RunPhase = "ACTIVE"
	RunFrozenSecurity RunPhase = "FROZEN_SECURITY"
	RunQuiescent      RunPhase = "QUIESCENT"
)

type ObservationSource string

const (
	ObservationProvider ObservationSource = "provider"
	ObservationTerminal ObservationSource = "terminal"
	ObservationYAML     ObservationSource = "yaml"
	ObservationUI       ObservationSource = "ui"
)

type GateKind string

const (
	GateTimeNotBefore       GateKind = "time_not_before"
	GateExternalStatus      GateKind = "external_status"
	GateWorkflowTerminal    GateKind = "workflow_terminal"
	GateEvidenceRequired    GateKind = "evidence_required"
	GateNoOverlapWindow     GateKind = "no_overlap_window"
	GateHumanApproval       GateKind = "human_approval"
	GateSecurityContainment GateKind = "security_containment"
)

type GateState string

const (
	GatePending  GateState = "pending"
	GateResolved GateState = "resolved"
)

type TaskState struct {
	TaskID    string    `json:"task_id"`
	AttemptID string    `json:"attempt_id,omitempty"`
	Phase     TaskPhase `json:"phase"`
}

type InboxItem struct {
	JournalPosition        journal.JournalPosition `json:"journal_position"`
	RunSequence            uint64                  `json:"run_sequence"`
	EventID                string                  `json:"event_id"`
	CorrelationID          string                  `json:"correlation_id"`
	TaskID                 string                  `json:"task_id"`
	AttemptID              string                  `json:"attempt_id"`
	ExternalActionID       string                  `json:"external_action_id"`
	ActionGeneration       uint64                  `json:"action_generation"`
	Producer               string                  `json:"producer"`
	Consumer               string                  `json:"consumer"`
	PayloadDigest          string                  `json:"payload_digest"`
	AcknowledgementReceipt string                  `json:"acknowledgement_receipt,omitempty"`
}

type RunInbox []InboxItem

type PendingWorkGate struct {
	ID                string    `json:"id"`
	TaskID            string    `json:"task_id"`
	Kind              GateKind  `json:"kind"`
	ResolverAuthority string    `json:"resolver_authority"`
	WakeEventID       string    `json:"wake_event_id,omitempty"`
	WakeAt            time.Time `json:"wake_at,omitempty"`
	State             GateState `json:"state"`
	WakeReceipt       string    `json:"wake_receipt,omitempty"`
}

type Projection struct {
	Scope               RunScope                `json:"scope"`
	Revision            uint64                  `json:"revision"`
	RunPhase            RunPhase                `json:"run_phase"`
	Tasks               []TaskState             `json:"tasks"`
	Inbox               RunInbox                `json:"inbox"`
	Gates               []PendingWorkGate       `json:"gates"`
	LastRunSequence     uint64                  `json:"last_run_sequence"`
	LastJournalPosition journal.JournalPosition `json:"last_journal_position"`
}

func (p Projection) Task(taskID string) (TaskState, bool) {
	for _, task := range p.Tasks {
		if task.TaskID == taskID {
			return task, true
		}
	}
	return TaskState{}, false
}

func (p Projection) InboxItem(eventID string) (InboxItem, bool) {
	for _, item := range p.Inbox {
		if item.EventID == eventID {
			return item, true
		}
	}
	return InboxItem{}, false
}

func (p Projection) Gate(gateID string) (PendingWorkGate, bool) {
	for _, gate := range p.Gates {
		if gate.ID == gateID {
			return gate, true
		}
	}
	return PendingWorkGate{}, false
}

type CommitResult struct {
	Projection Projection            `json:"projection"`
	Receipt    journal.CommitReceipt `json:"receipt"`
}

type TaskPhaseRequest struct {
	Scope            RunScope
	OperationID      string
	GraphRevision    uint64
	ExpectedRevision uint64
	TaskID           string
	AttemptID        string
	From             TaskPhase
	To               TaskPhase
}

type RunPhaseRequest struct {
	Scope            RunScope
	OperationID      string
	GraphRevision    uint64
	ExpectedRevision uint64
	From             RunPhase
	To               RunPhase
	Authority        string
}

type ObservationRequest struct {
	Scope  RunScope
	Source ObservationSource
	Status string
}

type CallbackRequest struct {
	Scope            RunScope
	OperationID      string
	GraphRevision    uint64
	ExpectedRevision uint64
	EventID          string
	CorrelationID    string
	TaskID           string
	AttemptID        string
	ExternalActionID string
	ActionGeneration uint64
	Producer         string
	Consumer         string
	PayloadDigest    string
}

type AcknowledgeRequest struct {
	Scope            RunScope
	OperationID      string
	GraphRevision    uint64
	ExpectedRevision uint64
	EventID          string
	Consumer         string
	ReceiptID        string
}

type RegisterGateRequest struct {
	Scope            RunScope
	OperationID      string
	GraphRevision    uint64
	ExpectedRevision uint64
	Gate             PendingWorkGate
}

type WakeGateRequest struct {
	Scope             RunScope
	OperationID       string
	GraphRevision     uint64
	ExpectedRevision  uint64
	GateID            string
	ResolverAuthority string
	WakeEventID       string
	WakeTime          time.Time
}

func validateRunScope(scope RunScope) error {
	if !validIdentifier(scope.InstallationID, 128) ||
		!validIdentifier(scope.ControlRunID, 128) {
		return ErrInvalidScope
	}
	return nil
}

func validateProjectScope(scope ProjectScope) error {
	if err := validateRunScope(scope.Run); err != nil ||
		!validIdentifier(scope.ProjectID, 128) ||
		strings.TrimSpace(scope.CanonicalRepository) != scope.CanonicalRepository ||
		scope.CanonicalRepository == "" || len(scope.CanonicalRepository) > 1024 ||
		!validGitSHA(scope.BaseSHA) {
		return ErrInvalidScope
	}
	return nil
}

func validIdentifier(value string, maximum int) bool {
	return strings.TrimSpace(value) == value && value != "" && len(value) <= maximum &&
		!strings.ContainsAny(value, "\x00\r\n\t ")
}

func validOpaqueHandle(value string) bool {
	if !validIdentifier(value, 256) {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("._:/-", character) {
			continue
		}
		return false
	}
	return true
}

func validGitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func scopedID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(append([]string{prefix}, values...), "\x00")))
	return prefix + "_" + hex.EncodeToString(digest[:16])
}
