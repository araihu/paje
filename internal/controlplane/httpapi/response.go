package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane"
	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/submission/auth"
)

var (
	errInvalidRequest         = errors.New("invalid request")
	errUnsupportedContentType = errors.New("unsupported content type")
	errBodyTooLarge           = errors.New("request body too large")
	errHeaderTooLarge         = errors.New("request header too large")
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, err error) {
	status, code, message := classifyError(err)
	if code == "provider_unavailable" {
		writer.Header().Set("Retry-After", "1")
	}
	writeJSON(writer, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func classifyError(err error) (int, string, string) {
	switch {
	case errors.Is(err, errUnsupportedContentType):
		return http.StatusUnsupportedMediaType, "invalid_request", "content type must be application/json"
	case errors.Is(err, errBodyTooLarge):
		return http.StatusRequestEntityTooLarge, "invalid_request", "request body exceeds the allowed size"
	case errors.Is(err, errHeaderTooLarge):
		return http.StatusRequestHeaderFieldsTooLarge, "invalid_request", "request header exceeds the allowed size"
	case errors.Is(err, errInvalidRequest), errors.Is(err, controlplane.ErrInvalidRecord),
		errors.Is(err, controlplane.ErrInvalidGraph), errors.Is(err, controlplane.ErrCursorRegression),
		errors.Is(err, agentharness.ErrInvalidRequest), errors.Is(err, agentharness.ErrActionMismatch),
		errors.Is(err, agentharness.ErrCursorRegression):
		return http.StatusBadRequest, "invalid_request", "the request is invalid"
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, submission.ErrUnauthenticated):
		return http.StatusUnauthorized, "unauthenticated", "authentication is required"
	case errors.Is(err, auth.ErrForbidden):
		return http.StatusForbidden, "forbidden", "the action is outside the credential scope"
	case errors.Is(err, controlplane.ErrNotFound):
		return http.StatusNotFound, "not_found", "the requested resource was not found"
	case errors.Is(err, controlplane.ErrAlreadyExists), errors.Is(err, controlplane.ErrVersionConflict),
		errors.Is(err, controlplane.ErrActionConflict), errors.Is(err, controlplane.ErrEvidenceImmutable),
		errors.Is(err, agentharness.ErrActionConflict):
		return http.StatusConflict, "idempotency_conflict", "the action is already bound to different input"
	case errors.Is(err, controlplane.ErrCapabilityUnavailable), errors.Is(err, agentharness.ErrUnsupportedOperation):
		return http.StatusUnprocessableEntity, "capability_unavailable", "the required capability is unavailable"
	case errors.Is(err, controlplane.ErrConcurrencyExhausted):
		return http.StatusTooManyRequests, "concurrency_exhausted", "the concurrency limit is exhausted"
	case errors.Is(err, controlplane.ErrInvalidPlacement), errors.Is(err, controlplane.ErrOwnershipConflict),
		errors.Is(err, controlplane.ErrImmutableBoundary), errors.Is(err, agentharness.ErrUnexpectedRuntimeIdentity),
		errors.Is(err, agentharness.ErrInvalidCapabilities):
		return http.StatusUnprocessableEntity, "placement_invalid", "the requested placement is invalid"
	case errors.Is(err, controlplane.ErrAmbiguousCreate), errors.Is(err, controlplane.ErrAmbiguousDispatch),
		errors.Is(err, agentharness.ErrActionOutcomeUnknown):
		return http.StatusConflict, "ambiguous_create", "the runtime action outcome is ambiguous"
	case errors.Is(err, controlplane.ErrCleanupIncomplete), errors.Is(err, controlplane.ErrClosePrecondition),
		errors.Is(err, controlplane.ErrActionIncomplete):
		return http.StatusConflict, "cleanup_incomplete", "required terminal or cleanup evidence is incomplete"
	case errors.Is(err, agentharness.ErrProviderUnavailable):
		return http.StatusServiceUnavailable, "provider_unavailable", "a required provider is unavailable"
	default:
		return http.StatusInternalServerError, "internal", "the request could not be completed"
	}
}

func methodNotAllowed(writer http.ResponseWriter, methods ...string) {
	writer.Header().Set("Allow", methods[0])
	writeJSON(writer, http.StatusMethodNotAllowed, errorBody{Error: errorDetail{
		Code: "invalid_request", Message: "the method is not allowed for this route",
	}})
}
