// Package scheduler selects fair control-plane work and drives bounded
// recovery through the journal-authoritative admission service. It owns no
// admission, lease, or recovery projection of its own.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
	"github.com/araihu/paje/internal/controlplane/journal"
)

const SchemaVersion uint32 = 1

const (
	VirtualQuantum          uint64        = 1024
	MaximumAgeCredit        uint64        = 300
	ConsecutiveAdmissionCap uint64        = admission.MaxConsecutiveAdmissions
	MaximumScanEntries                    = 100
	AgeInterval             time.Duration = 60 * time.Second
	ObservationWorkBudget   time.Duration = 200 * time.Millisecond
	RecoveryPassBudget      time.Duration = 250 * time.Millisecond
	InitialRetryBackoff     time.Duration = 30 * time.Second
	MaximumRetryBackoff     time.Duration = 5 * time.Minute
)

var (
	ErrInvalidRecord = errors.New("invalid scheduler record")
	ErrInvalidPolicy = errors.New("invalid scheduler policy")
	ErrNoEligible    = errors.New("no eligible scheduler item")
	ErrCursor        = errors.New("scheduler cursor conflict")
	ErrBudget        = errors.New("scheduler recovery budget exhausted")
)

type ErrorCode string

const (
	CodeInvalidRequest ErrorCode = "invalid_request"
	CodeInvalidPolicy  ErrorCode = "invalid_policy"
	CodeNoEligible     ErrorCode = "no_eligible_item"
	CodeAuthority      ErrorCode = "authority_failure"
	CodeCursor         ErrorCode = "cursor_failure"
	CodeBudget         ErrorCode = "budget_exhausted"
)

type Error struct {
	Code      ErrorCode
	Operation string
	cause     error
}

func (e *Error) Error() string {
	if e == nil {
		return "scheduler error"
	}
	if e.Operation == "" {
		return "scheduler: " + string(e.Code)
	}
	return fmt.Sprintf("scheduler %s: %s", e.Operation, e.Code)
}

func (e *Error) Unwrap() error { return e.cause }

func typedError(code ErrorCode, operation string, cause error) error {
	return &Error{Code: code, Operation: operation, cause: cause}
}

type Policy struct {
	Version             uint64
	Quantum             uint64
	AgingInterval       time.Duration
	MaxAgeCredit        uint64
	ConsecutiveLimit    uint64
	MaxScanEntries      int
	ObservationBudget   time.Duration
	PassBudget          time.Duration
	InitialRetryBackoff time.Duration
	MaximumRetryBackoff time.Duration
}

func DefaultPolicy() Policy {
	return Policy{
		Version: 1, Quantum: VirtualQuantum, AgingInterval: AgeInterval,
		MaxAgeCredit: MaximumAgeCredit, ConsecutiveLimit: ConsecutiveAdmissionCap,
		MaxScanEntries: MaximumScanEntries, ObservationBudget: ObservationWorkBudget,
		PassBudget: RecoveryPassBudget, InitialRetryBackoff: InitialRetryBackoff,
		MaximumRetryBackoff: MaximumRetryBackoff,
	}
}

func (p Policy) validate() error {
	if p.Version == 0 || p.Quantum != VirtualQuantum || p.AgingInterval != AgeInterval ||
		p.MaxAgeCredit != MaximumAgeCredit || p.ConsecutiveLimit != ConsecutiveAdmissionCap ||
		p.MaxScanEntries != MaximumScanEntries || p.ObservationBudget != ObservationWorkBudget ||
		p.PassBudget != RecoveryPassBudget || p.InitialRetryBackoff != InitialRetryBackoff ||
		p.MaximumRetryBackoff != MaximumRetryBackoff {
		return typedError(CodeInvalidPolicy, "policy", ErrInvalidPolicy)
	}
	return nil
}

type ReadyItem struct {
	ID              string
	ControlRunID    string
	Weight          uint64
	VirtualStart    uint64
	EnqueueSequence uint64
	EnqueuedAt      time.Time
	Admission       admission.AdmissionRequest
	QueueReceipt    admission.AdmissionReceipt
	Attempt         uint64
}

type RankedItem struct {
	Item                   ReadyItem
	VirtualFinish          uint64
	AgeCredit              uint64
	EffectiveVirtualFinish uint64
}

type FairnessState struct {
	LastRunID             string
	ConsecutiveAdmissions uint64
}

type BackpressureDecision struct {
	ItemID       string
	ControlRunID string
	Receipt      admission.BackpressureReceipt
}

type AdmissionDecision struct {
	Admitted     bool
	Item         RankedItem
	Receipt      admission.AdmissionReceipt
	Backpressure []BackpressureDecision
	State        FairnessState
}

type AdmissionAuthority interface {
	Enqueue(context.Context, admission.AdmissionRequest) (admission.AdmissionReceipt, error)
	Admit(context.Context, admission.AdmissionRequest) (admission.AdmissionReceipt, error)
	AcquireLease(context.Context, admission.LeaseRequest) (admission.LeaseReceipt, error)
	ReleaseLease(context.Context, admission.LeaseRequest) (admission.LeaseReceipt, error)
	ExpireLease(context.Context, admission.LeaseRequest) (admission.LeaseReceipt, error)
}

type RecoveryAuthority interface {
	StartObservation(context.Context, admission.RecoveryRequest) (admission.RecoveryReceipt, error)
	Observe(context.Context, admission.RecoveryIdentity) (admission.ObservationReceipt, error)
	CancelOrFence(context.Context, admission.FenceRequest) (admission.RecoveryReceipt, error)
	ScannerApply(context.Context, admission.ScannerApplyRequest) (admission.RecoveryReceipt, error)
}

type Authority interface {
	AdmissionAuthority
	RecoveryAuthority
}

type Dependencies struct {
	Authority        Authority
	Journal          journal.AuthoritativeStore
	Clock            func() time.Time
	Policy           Policy
	ScannerAuthority string
}

type Service struct {
	authority        Authority
	journal          journal.AuthoritativeStore
	clock            func() time.Time
	policy           Policy
	scannerAuthority string
}

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.Authority == nil || dependencies.Journal == nil || dependencies.Clock == nil {
		return nil, typedError(CodeInvalidRequest, "new", ErrInvalidRecord)
	}
	if err := dependencies.Policy.validate(); err != nil {
		return nil, err
	}
	if !bounded(dependencies.ScannerAuthority, 128) {
		return nil, typedError(CodeInvalidRequest, "new", ErrInvalidRecord)
	}
	return &Service{
		authority: dependencies.Authority, journal: dependencies.Journal,
		clock: dependencies.Clock, policy: dependencies.Policy,
		scannerAuthority: dependencies.ScannerAuthority,
	}, nil
}

func bounded(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit
}
