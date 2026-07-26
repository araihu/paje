package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/submission/auth"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
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

type submissionResponse struct {
	APIVersion string                     `json:"api_version"`
	RunID      string                     `json:"run_id"`
	Status     string                     `json:"status"`
	Reused     *bool                      `json:"reused,omitempty"`
	Depth      int                        `json:"depth"`
	RootRunID  string                     `json:"root_run_id"`
	Result     *templatecodechange.Result `json:"result,omitempty"`
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
	if code == "unauthenticated" {
		writer.Header().Set("WWW-Authenticate", "Bearer")
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
	case errors.Is(err, errInvalidRequest), errors.Is(err, submission.ErrInvalidRequest):
		return http.StatusBadRequest, "invalid_request", "the request is invalid"
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, submission.ErrUnauthenticated):
		return http.StatusUnauthorized, "unauthenticated", "authentication is required"
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, submission.ErrForbidden):
		return http.StatusForbidden, "forbidden", "the action is outside the credential scope"
	case errors.Is(err, submission.ErrNotFound):
		return http.StatusNotFound, "not_found", "the requested resource was not found"
	case errors.Is(err, submission.ErrIdempotencyConflict):
		return http.StatusConflict, "idempotency_conflict", "the idempotency key is already bound to different input"
	case errors.Is(err, submission.ErrDepthExceeded):
		return http.StatusUnprocessableEntity, "depth_exceeded", "the submission depth exceeds the allowed maximum"
	case errors.Is(err, submission.ErrRunNotCancelable):
		return http.StatusConflict, "run_not_cancelable", "the submission is not cancelable"
	case errors.Is(err, submission.ErrProviderUnavailable):
		return http.StatusServiceUnavailable, "provider_unavailable", "a required provider is unavailable"
	default:
		return http.StatusInternalServerError, "internal", "the request could not be completed"
	}
}

func responseForView(view submission.View, reused *bool) submissionResponse {
	return submissionResponse{
		APIVersion: "v1",
		RunID:      view.Record.RunID,
		Status:     string(view.Status),
		Reused:     reused,
		Depth:      view.Record.Depth,
		RootRunID:  view.Record.RootRunID,
		Result:     view.Result,
	}
}
