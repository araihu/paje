package submission

import "errors"

var (
	ErrInvalidRequest      = errors.New("invalid submission request")
	ErrUnauthenticated     = errors.New("submission principal is unauthenticated")
	ErrForbidden           = errors.New("submission is forbidden")
	ErrNotFound            = errors.New("submission not found")
	ErrIdempotencyConflict = errors.New("submission idempotency conflict")
	ErrDepthExceeded       = errors.New("submission depth exceeded")
	ErrRunNotCancelable    = errors.New("submission is not cancelable")
	ErrProviderUnavailable = errors.New("submission provider unavailable")
)

// domainError preserves machine-classifiable causes while exposing only a
// bounded message selected by the submission domain.
type domainError struct {
	kind    error
	message string
	cause   error
}

func (e *domainError) Error() string {
	return e.message
}

func (e *domainError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

func newDomainError(kind error, message string, cause error) error {
	return &domainError{kind: kind, message: message, cause: cause}
}
