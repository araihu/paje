package controlplane

import "errors"

var (
	ErrNotFound              = errors.New("control plane record not found")
	ErrAlreadyExists         = errors.New("control plane record already exists")
	ErrVersionConflict       = errors.New("control plane version conflict")
	ErrInvalidRecord         = errors.New("invalid control plane record")
	ErrInvalidGraph          = errors.New("invalid control plane graph")
	ErrInvalidPlacement      = errors.New("invalid execution placement")
	ErrOwnershipConflict     = errors.New("control plane ownership conflict")
	ErrImmutableBoundary     = errors.New("active task boundary is immutable")
	ErrCapabilityUnavailable = errors.New("required capability unavailable")
	ErrConcurrencyExhausted  = errors.New("control plane concurrency exhausted")
	ErrActionConflict        = errors.New("control plane action conflict")
	ErrActionIncomplete      = errors.New("control plane action incomplete")
	ErrAmbiguousDispatch     = errors.New("ambiguous dispatch")
	ErrAmbiguousCreate       = errors.New("ambiguous create")
	ErrCursorRegression      = errors.New("control plane cursor regression")
	ErrEvidenceImmutable     = errors.New("integrated evidence is immutable")
	ErrCleanupIncomplete     = errors.New("control plane cleanup incomplete")
	ErrClosePrecondition     = errors.New("control plane close precondition failed")
	ErrCorruptStore          = errors.New("corrupt control plane store")
)
