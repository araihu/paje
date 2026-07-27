// Package admission owns journal-authoritative admission, quota, lease,
// evidence-handoff, and fenced-recovery transitions for the Pajé control
// plane. All returned records are projections rebuilt from ACP-J06 journal
// commits; process memory is never transition authority.
package admission

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
)

const SchemaVersion uint32 = 1

const MaxConsecutiveAdmissions uint64 = 2

const component = "admission"

var (
	ErrInvalidRecord = errors.New("invalid admission record")
	ErrInvalidPolicy = errors.New("invalid admission policy")
	ErrConflict      = errors.New("admission conflict")
	ErrNotFound      = errors.New("admission record not found")
	ErrQuota         = errors.New("admission quota reached")
	ErrLeaseBusy     = errors.New("admission lease unavailable")
	ErrTerminal      = errors.New("admission record is terminal")
	ErrNotExpired    = errors.New("admission lease has not expired")
	ErrAmbiguous     = errors.New("admission recovery is ambiguous")
	ErrFenced        = errors.New("admission recovery generation is fenced")
	ErrUnauthorized  = errors.New("admission authority denied")
)

type ErrorCode string

const (
	CodeInvalidRequest ErrorCode = "invalid_request"
	CodeInvalidRecord  ErrorCode = "invalid_record"
	CodeConflict       ErrorCode = "conflict"
	CodeNotFound       ErrorCode = "not_found"
	CodeQuota          ErrorCode = "quota"
	CodeLeaseBusy      ErrorCode = "lease_busy"
	CodeTerminal       ErrorCode = "terminal"
	CodeNotExpired     ErrorCode = "not_expired"
	CodeAmbiguous      ErrorCode = "ambiguous"
	CodeFenced         ErrorCode = "fenced"
	CodeUnauthorized   ErrorCode = "unauthorized"
	CodeStore          ErrorCode = "store_failure"
)

// Error deliberately contains only a stable code and bounded safe operation.
// Raw dependency errors and caller-controlled subjects never enter diagnostics.
type Error struct {
	Code      ErrorCode
	Operation SemanticOperation
	cause     error
}

func (e *Error) Error() string {
	if e == nil {
		return "admission error"
	}
	if e.Operation == "" {
		return "admission: " + string(e.Code)
	}
	return fmt.Sprintf("admission %s: %s", e.Operation, e.Code)
}

func (e *Error) Unwrap() error { return e.cause }

type SemanticOperation string

const (
	OperationAdmissionReserve   SemanticOperation = "admission_reserve"
	OperationQueueEnqueue       SemanticOperation = "queue_enqueue"
	OperationQueueAdmit         SemanticOperation = "queue_admit"
	OperationBackpressureDefer  SemanticOperation = "backpressure_defer"
	OperationAdmissionRelease   SemanticOperation = "admission_release"
	OperationQueueRelease       SemanticOperation = "queue_release"
	OperationLeaseAcquire       SemanticOperation = "lease_acquire"
	OperationLeaseRenew         SemanticOperation = "lease_renew"
	OperationLeaseRelease       SemanticOperation = "lease_release"
	OperationLeaseExpire        SemanticOperation = "lease_expire"
	OperationStartObservation   SemanticOperation = "start_observation"
	OperationObserveEffect      SemanticOperation = "observe_effect"
	OperationCancelOrFence      SemanticOperation = "cancel_or_fence"
	OperationScannerApply       SemanticOperation = "scanner_apply"
	OperationHandoffIssue       SemanticOperation = "evidence_handoff_issue"
	OperationHandoffGrant       SemanticOperation = "evidence_handoff_grant"
	OperationHandoffAcknowledge SemanticOperation = "evidence_handoff_acknowledge"
)

type Policy struct {
	Version           uint64 `json:"version"`
	InstallationLimit uint64 `json:"installation_limit"`
	PrincipalLimit    uint64 `json:"principal_limit"`
	RunLimit          uint64 `json:"run_limit"`
	ProjectLimit      uint64 `json:"project_limit"`
	PrimitiveLimit    uint64 `json:"primitive_limit"`
}

func (p Policy) validate() error {
	if p.Version == 0 || p.InstallationLimit == 0 || p.PrincipalLimit == 0 || p.RunLimit == 0 ||
		p.ProjectLimit == 0 || p.PrimitiveLimit == 0 {
		return typedError(CodeInvalidRequest, "", ErrInvalidPolicy)
	}
	return nil
}

type Dependencies struct {
	Store            journal.AuthoritativeStore
	Policy           Policy
	Clock            func() time.Time
	Observer         Observer
	ScannerAuthority string
}

type TransitionIdentity struct {
	ActionID       string            `json:"action_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	OutcomeEventID string            `json:"outcome_event_id"`
	OutcomeKind    journal.EventKind `json:"outcome_kind"`
	GraphRevision  uint64            `json:"graph_revision"`
	Generation     uint64            `json:"generation"`
	TaskID         string            `json:"task_id,omitempty"`
	AttemptID      string            `json:"attempt_id,omitempty"`
}

func (i TransitionIdentity) validate() error {
	if !boundedRequired(i.ActionID, 128) || !boundedRequired(i.IdempotencyKey, 256) ||
		!boundedRequired(i.OutcomeEventID, 128) || i.GraphRevision == 0 || i.Generation == 0 ||
		len(i.TaskID) > 128 || len(i.AttemptID) > 128 || (i.AttemptID != "" && i.TaskID == "") {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	switch i.OutcomeKind {
	case journal.EventActionResult, journal.EventActionNotPerformed, journal.EventActionAmbiguous:
		return nil
	default:
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
}

type AdmissionState string

const (
	AdmissionReserved AdmissionState = "reserved"
	AdmissionQueued   AdmissionState = "queued"
	AdmissionAdmitted AdmissionState = "admitted"
	AdmissionDeferred AdmissionState = "deferred"
	AdmissionReleased AdmissionState = "released"
)

type QuotaKind string

const (
	QuotaNone         QuotaKind = ""
	QuotaInstallation QuotaKind = "installation"
	QuotaPrincipal    QuotaKind = "principal"
	QuotaRun          QuotaKind = "run"
	QuotaProject      QuotaKind = "project"
	QuotaPrimitive    QuotaKind = "primitive"
)

type EligibilityCondition string

const EligibilityQuotaAvailable EligibilityCondition = "quota_available"

type BackpressureReceipt struct {
	LimitingQuota   QuotaKind            `json:"limiting_quota"`
	NextEligibility EligibilityCondition `json:"next_eligibility"`
	PolicyVersion   uint64               `json:"policy_version"`
	PolicyDigest    string               `json:"policy_digest"`
}

type AdmissionSubject struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	ProjectID   string `json:"project_id"`
	Primitive   string `json:"primitive"`
	WorkID      string `json:"work_id"`
}

func (s AdmissionSubject) validate() error {
	if !boundedRequired(s.ID, 128) || !boundedRequired(s.PrincipalID, 128) ||
		!boundedRequired(s.ProjectID, 256) || !boundedRequired(s.Primitive, 64) ||
		!boundedRequired(s.WorkID, 128) {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	return nil
}

type AdmissionRequest struct {
	ControlRunID string             `json:"control_run_id"`
	Identity     TransitionIdentity `json:"identity"`
	Subject      AdmissionSubject   `json:"subject"`
}

type RunAdmission struct {
	ControlRunID          string           `json:"control_run_id"`
	Subject               AdmissionSubject `json:"subject"`
	State                 AdmissionState   `json:"state"`
	LimitingQuota         QuotaKind        `json:"limiting_quota,omitempty"`
	Sequence              uint64           `json:"sequence"`
	GraphRevision         uint64           `json:"graph_revision"`
	Generation            uint64           `json:"generation"`
	PolicyVersion         uint64           `json:"policy_version"`
	PolicyDigest          string           `json:"policy_digest"`
	OriginalRequestDigest string           `json:"original_request_digest"`
	ReceiptID             string           `json:"receipt_id"`
}

type AdmissionTombstone struct {
	ControlRunID          string           `json:"control_run_id"`
	Subject               AdmissionSubject `json:"subject"`
	GraphRevision         uint64           `json:"graph_revision"`
	Generation            uint64           `json:"generation"`
	PolicyVersion         uint64           `json:"policy_version"`
	PolicyDigest          string           `json:"policy_digest"`
	OriginalRequestDigest string           `json:"original_request_digest"`
	TerminalReceiptID     string           `json:"terminal_receipt_id"`
	ReleasedAt            time.Time        `json:"released_at"`
}

type AdmissionReceipt struct {
	Commit       journal.CommitReceipt `json:"commit"`
	Operation    SemanticOperation     `json:"semantic_operation"`
	Admission    RunAdmission          `json:"admission"`
	Backpressure *BackpressureReceipt  `json:"backpressure,omitempty"`
	Tombstone    *AdmissionTombstone   `json:"tombstone,omitempty"`
	Created      bool                  `json:"created"`
}

type ResourceKey struct {
	Namespace string `json:"namespace"`
	ProjectID string `json:"project_id,omitempty"`
	Name      string `json:"name"`
}

func (k ResourceKey) validate() error {
	if !boundedRequired(k.Namespace, 64) || len(k.ProjectID) > 256 || !boundedRequired(k.Name, 256) {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	return nil
}

type LeaseMode string

const (
	LeaseShared    LeaseMode = "shared"
	LeaseExclusive LeaseMode = "exclusive"
)

type LeaseState string

const (
	LeaseActive   LeaseState = "active"
	LeaseReleased LeaseState = "released"
	LeaseExpired  LeaseState = "expired"
)

type LeaseSubject struct {
	ID       string      `json:"id"`
	Resource ResourceKey `json:"resource"`
	Mode     LeaseMode   `json:"mode"`
	HolderID string      `json:"holder_id"`
}

func (s LeaseSubject) validate() error {
	if !boundedRequired(s.ID, 128) || !boundedRequired(s.HolderID, 128) ||
		(s.Mode != LeaseShared && s.Mode != LeaseExclusive) {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	return s.Resource.validate()
}

type LeaseRequest struct {
	ControlRunID string             `json:"control_run_id"`
	Identity     TransitionIdentity `json:"identity"`
	Subject      LeaseSubject       `json:"subject"`
	ExpiresAt    time.Time          `json:"expires_at"`
}

type Lease struct {
	ControlRunID          string       `json:"control_run_id"`
	Subject               LeaseSubject `json:"subject"`
	State                 LeaseState   `json:"state"`
	IssuedAt              time.Time    `json:"issued_at"`
	ExpiresAt             time.Time    `json:"expires_at"`
	Generation            uint64       `json:"generation"`
	GraphRevision         uint64       `json:"graph_revision"`
	OriginalRequestDigest string       `json:"original_request_digest"`
	ReceiptID             string       `json:"receipt_id"`
}

type LeaseTombstone struct {
	ControlRunID          string       `json:"control_run_id"`
	Subject               LeaseSubject `json:"subject"`
	State                 LeaseState   `json:"state"`
	IssuedAt              time.Time    `json:"issued_at"`
	ExpiresAt             time.Time    `json:"expires_at"`
	Generation            uint64       `json:"generation"`
	GraphRevision         uint64       `json:"graph_revision"`
	OriginalRequestDigest string       `json:"original_request_digest"`
	TerminalReceiptID     string       `json:"terminal_receipt_id"`
	TerminalAt            time.Time    `json:"terminal_at"`
}

type LeaseReceipt struct {
	Commit    journal.CommitReceipt `json:"commit"`
	Operation SemanticOperation     `json:"semantic_operation"`
	Lease     Lease                 `json:"lease"`
	Tombstone *LeaseTombstone       `json:"tombstone,omitempty"`
	Created   bool                  `json:"created"`
}

type EvidenceEndpoint struct {
	ProjectID  string `json:"project_id"`
	TaskID     string `json:"task_id"`
	AttemptID  string `json:"attempt_id"`
	ActionID   string `json:"action_id"`
	Generation uint64 `json:"generation"`
}

func (e EvidenceEndpoint) validate() error {
	if !boundedRequired(e.ProjectID, 256) || !boundedRequired(e.TaskID, 128) ||
		!boundedRequired(e.AttemptID, 128) || !boundedRequired(e.ActionID, 128) || e.Generation == 0 {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	return nil
}

type EvidenceHandoffSubject struct {
	GraphRevision  uint64           `json:"graph_revision"`
	EdgeID         string           `json:"edge_id"`
	Producer       EvidenceEndpoint `json:"producer"`
	Consumer       EvidenceEndpoint `json:"consumer"`
	EvidenceDigest string           `json:"evidence_digest"`
}

func (s EvidenceHandoffSubject) validate() error {
	if s.GraphRevision == 0 || !boundedRequired(s.EdgeID, 128) || !journal.ValidDigest(s.EvidenceDigest) {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	if err := s.Producer.validate(); err != nil {
		return err
	}
	return s.Consumer.validate()
}

type HandoffState string

const (
	HandoffIssued       HandoffState = "issued"
	HandoffGranted      HandoffState = "granted"
	HandoffAcknowledged HandoffState = "acknowledged"
)

type HandoffRequest struct {
	ControlRunID         string                 `json:"control_run_id"`
	Identity             TransitionIdentity     `json:"identity"`
	Subject              EvidenceHandoffSubject `json:"subject"`
	PredecessorReceiptID string                 `json:"predecessor_receipt_id,omitempty"`
}

type EvidenceHandoff struct {
	ID                   string                 `json:"id"`
	ControlRunID         string                 `json:"control_run_id"`
	Subject              EvidenceHandoffSubject `json:"subject"`
	State                HandoffState           `json:"state"`
	ReceiptID            string                 `json:"receipt_id"`
	PredecessorReceiptID string                 `json:"predecessor_receipt_id,omitempty"`
	Sequence             uint64                 `json:"sequence"`
}

type HandoffReceipt struct {
	Commit    journal.CommitReceipt `json:"commit"`
	Operation SemanticOperation     `json:"semantic_operation"`
	Handoff   EvidenceHandoff       `json:"handoff"`
	Created   bool                  `json:"created"`
}

type EvidenceDisclosure struct {
	Authoritative     bool         `json:"authoritative"`
	HandoffID         string       `json:"handoff_id"`
	ControlRunID      string       `json:"control_run_id"`
	EdgeID            string       `json:"edge_id"`
	ProducerProjectID string       `json:"producer_project_id"`
	ConsumerProjectID string       `json:"consumer_project_id"`
	EvidenceDigest    string       `json:"evidence_digest"`
	State             HandoffState `json:"state"`
}

type RecoveryIdentity struct {
	InstallationID string `json:"installation_id,omitempty"`
	ControlRunID   string `json:"control_run_id"`
	ActionID       string `json:"action_id"`
	Generation     uint64 `json:"generation"`
	SubjectDigest  string `json:"subject_digest"`
}

func (i RecoveryIdentity) validate() error {
	if len(i.InstallationID) > 128 || !boundedRequired(i.ControlRunID, 128) || !boundedRequired(i.ActionID, 128) ||
		i.Generation == 0 || !journal.ValidDigest(i.SubjectDigest) {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	return nil
}

type RecoveryState string

const (
	RecoveryObservationStarted RecoveryState = "observation_started"
	RecoveryEffectObserved     RecoveryState = "effect_observed"
	RecoveryNotPerformed       RecoveryState = "not_performed"
	RecoveryCanceled           RecoveryState = "canceled"
	RecoveryFenced             RecoveryState = "fenced"
	RecoveryAmbiguous          RecoveryState = "ambiguous"
	RecoveryApplied            RecoveryState = "applied"
)

type ProviderStatus string

const (
	ProviderEffectObserved ProviderStatus = "effect_observed"
	ProviderNotPerformed   ProviderStatus = "not_performed"
	ProviderCanceled       ProviderStatus = "canceled"
	ProviderFenced         ProviderStatus = "fenced"
	ProviderAmbiguous      ProviderStatus = "ambiguous"
)

type ProviderFact struct {
	Status        ProviderStatus `json:"status"`
	ReceiptID     string         `json:"receipt_id,omitempty"`
	SubjectDigest string         `json:"subject_digest"`
}

type Observer interface {
	Observe(context.Context, RecoveryIdentity) (ProviderFact, error)
}

type RecoveryRequest struct {
	Identity TransitionIdentity `json:"identity"`
	Recovery RecoveryIdentity   `json:"recovery"`
}

type ObservationReceipt struct {
	Authoritative bool             `json:"authoritative"`
	Recovery      RecoveryIdentity `json:"recovery"`
	Fact          ProviderFact     `json:"fact"`
}

type FenceProof struct {
	Status    ProviderStatus `json:"status"`
	ReceiptID string         `json:"receipt_id"`
}

type FenceRequest struct {
	Identity TransitionIdentity `json:"identity"`
	Recovery RecoveryIdentity   `json:"recovery"`
	Proof    FenceProof         `json:"proof"`
}

type ScannerApplyRequest struct {
	Identity         TransitionIdentity `json:"identity"`
	Recovery         RecoveryIdentity   `json:"recovery"`
	ScannerAuthority string             `json:"scanner_authority"`
	Fact             ProviderFact       `json:"fact"`
}

type RecoveryRecord struct {
	Identity  RecoveryIdentity `json:"identity"`
	State     RecoveryState    `json:"state"`
	ReceiptID string           `json:"receipt_id"`
	Fact      ProviderFact     `json:"fact"`
}

type RecoveryReceipt struct {
	Commit       journal.CommitReceipt `json:"commit"`
	Operation    SemanticOperation     `json:"semantic_operation"`
	Recovery     RecoveryRecord        `json:"recovery"`
	RetryAllowed bool                  `json:"retry_allowed"`
	Created      bool                  `json:"created"`
}

func boundedRequired(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit
}

func typedError(code ErrorCode, operation SemanticOperation, cause error) error {
	return &Error{Code: code, Operation: operation, cause: cause}
}
