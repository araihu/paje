package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane"
	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/submission/auth"
)

const (
	maxBodyBytes         = 1 << 20
	minIdempotencyBytes  = 16
	maxIdempotencyBytes  = 128
	maxCursorBytes       = 20
	maxPathIDBytes       = 128
	maxEventsPerRead     = 100
	idempotencyRunPrefix = "paje-idem-"
)

type idempotencyKeyContextKey struct{}

type Dependencies struct {
	Service       *controlplane.Service
	Authenticator *auth.Authenticator
}

type server struct {
	service       *controlplane.Service
	authenticator *auth.Authenticator
}

func New(dependencies Dependencies) (http.Handler, error) {
	if dependencies.Service == nil {
		return nil, errors.New("create control HTTP API: service is required")
	}
	if dependencies.Authenticator == nil {
		return nil, errors.New("create control HTTP API: authenticator is required")
	}
	return &server{service: dependencies.Service, authenticator: dependencies.Authenticator}, nil
}

func (s *server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		writeError(writer, auth.ErrUnauthenticated)
		return
	}
	if request.URL.Path == "/v1/capabilities" {
		if request.URL.RawQuery != "" {
			writeError(writer, errInvalidRequest)
			return
		}
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		s.handleCapabilities(writer, principal)
		return
	}
	if request.URL.Path == "/v1/control-runs" {
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		if !s.prepareMutation(writer, request) {
			return
		}
		s.handleCreate(writer, request, principal)
		return
	}
	const prefix = "/v1/control-runs/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		writeError(writer, controlplane.ErrNotFound)
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, prefix), "/")
	if len(parts) == 0 || !safePathID(parts[0]) {
		writeError(writer, controlplane.ErrNotFound)
		return
	}
	if request.Method == http.MethodPost && !s.prepareMutation(writer, request) {
		return
	}
	s.routeRun(writer, request, principal, parts)
}

func (s *server) routeRun(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	parts []string,
) {
	runID := parts[0]
	if strings.HasPrefix(runID, idempotencyRunPrefix) {
		writeError(writer, controlplane.ErrNotFound)
		return
	}
	switch {
	case len(parts) == 1:
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		s.handleStatus(writer, request, principal, runID)
	case len(parts) == 2 && parts[1] == "tasks":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleTaskCreate(writer, request, principal, runID)
	case len(parts) == 4 && parts[1] == "tasks" && safePathID(parts[2]) && parts[3] == "attempts":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleAttemptAction(writer, request, principal, runID, parts[2])
	case len(parts) == 2 && parts[1] == "attempts:wait":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleWaitAction(writer, request, principal, runID)
	case len(parts) == 3 && parts[1] == "attempts" && safePathID(parts[2]):
		switch request.Method {
		case http.MethodGet:
			s.handleAttemptStatus(writer, request, principal, runID, parts[2])
		case http.MethodPost:
			s.handleObserveAction(writer, request, principal, runID, parts[2])
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		}
	case len(parts) == 4 && parts[1] == "attempts" && safePathID(parts[2]) && parts[3] == "messages":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleMessageAction(writer, request, principal, runID, parts[2])
	case len(parts) == 4 && parts[1] == "attempts" && safePathID(parts[2]) && parts[3] == "interrupt":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleSimpleAction(writer, request, principal, runID, parts[2], agentharness.ActionInterrupt, auth.ActionWorkInterrupt)
	case len(parts) == 4 && parts[1] == "attempts" && safePathID(parts[2]) && parts[3] == "close":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleCloseAction(writer, request, principal, runID, parts[2])
	case len(parts) == 2 && parts[1] == "evidence":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleEvidence(writer, request, principal, runID)
	case len(parts) == 2 && parts[1] == "close":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleControlClose(writer, request, principal, runID)
	default:
		writeError(writer, controlplane.ErrNotFound)
	}
}

func (s *server) handleCapabilities(writer http.ResponseWriter, principal submission.Principal) {
	harnesses := make([]string, 0, len(principal.Harnesses))
	for harness, allowed := range principal.Harnesses {
		if allowed {
			harnesses = append(harnesses, harness)
		}
	}
	sort.Strings(harnesses)
	writeJSON(writer, http.StatusOK, map[string]any{
		"api_version": "v1", "credential_id": principal.CredentialID,
		"actions": s.authenticator.Actions(principal), "harnesses": harnesses,
		"projects": s.authenticator.Projects(principal), "max_depth": principal.MaxDepth,
	})
}

func (s *server) handleCreate(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionControlCreate); err != nil {
		writeError(writer, err)
		return
	}
	if err := s.authenticator.Authorize(principal, auth.ActionTaskCreate); err != nil {
		writeError(writer, err)
		return
	}
	var body struct {
		Run   controlplane.ControlRun `json:"run"`
		Graph controlplane.TaskGraph  `json:"graph"`
	}
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	if body.Run.PrincipalID != "" && body.Run.PrincipalID != principal.CredentialID {
		writeError(writer, auth.ErrForbidden)
		return
	}
	body.Run.PrincipalID = principal.CredentialID
	if strings.HasPrefix(body.Run.ID, idempotencyRunPrefix) {
		writeError(writer, errInvalidRequest)
		return
	}
	if err := s.authorizeGraph(principal, body.Graph); err != nil {
		writeError(writer, err)
		return
	}
	if err := s.bindMutation(request.Context(), principal, request, body, body.Graph); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.service.Create(request.Context(), body.Run, body.Graph)
	if err == nil {
		writeJSON(writer, http.StatusAccepted, map[string]any{"api_version": "v1", "snapshot": snapshot, "reused": false})
		return
	}
	if !errors.Is(err, controlplane.ErrAlreadyExists) {
		writeError(writer, err)
		return
	}
	existing, loadErr := s.service.Load(request.Context(), body.Run.ID)
	if loadErr != nil || !sameCreateBoundary(existing, body.Run, body.Graph) {
		writeError(writer, controlplane.ErrActionConflict)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"api_version": "v1", "snapshot": existing, "reused": true})
}

func (s *server) handleStatus(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionWorkObserve); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	after, err := parseAfterCursor(request, snapshot.Run.EventCursor)
	if err != nil {
		writeError(writer, err)
		return
	}
	events, next, err := s.service.EventsAfter(request.Context(), runID, after, maxEventsPerRead)
	if err != nil {
		writeError(writer, err)
		return
	}
	if snapshot.Run.Status != controlplane.StatusClosed && snapshot.Run.Status != controlplane.StatusCanceled {
		writer.Header().Set("Retry-After", "1")
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"api_version": "v1", "snapshot": snapshot, "events": events,
		"next_cursor": strconv.FormatUint(next, 10),
	})
}

func (s *server) handleTaskCreate(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionTaskCreate); err != nil {
		writeError(writer, err)
		return
	}
	var body struct {
		ExpectedRevision uint64              `json:"expected_revision"`
		Task             controlplane.Task   `json:"task"`
		IntegrationOrder []string            `json:"integration_order"`
		CombinedGates    []controlplane.Gate `json:"combined_gates"`
	}
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	current, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := s.authorizeTask(principal, body.Task, current.Graph); err != nil {
		writeError(writer, err)
		return
	}
	if err := s.bindMutation(request.Context(), principal, request, body, current.Graph); err != nil {
		writeError(writer, err)
		return
	}
	if current.Graph.Revision != body.ExpectedRevision {
		if exactTaskRevision(current.Graph, body) {
			writeJSON(writer, http.StatusOK, map[string]any{"api_version": "v1", "snapshot": current, "reused": true})
			return
		}
		writeError(writer, controlplane.ErrVersionConflict)
		return
	}
	for _, task := range current.Graph.Tasks {
		if task.ID == body.Task.ID {
			writeError(writer, controlplane.ErrActionConflict)
			return
		}
	}
	next := controlplane.CloneGraph(current.Graph)
	next.Revision = body.ExpectedRevision + 1
	next.Tasks = append(next.Tasks, body.Task)
	next.IntegrationOrder = append([]string(nil), body.IntegrationOrder...)
	next.CombinedGates = append([]controlplane.Gate(nil), body.CombinedGates...)
	if err := s.authorizeGraph(principal, next); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.service.UpdateGraph(request.Context(), runID, body.ExpectedRevision, next)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"api_version": "v1", "snapshot": snapshot, "reused": false})
}

type actionRequest struct {
	Operation      string                          `json:"operation"`
	Capabilities   agentharness.CapabilitySnapshot `json:"capabilities,omitempty"`
	AttemptID      string                          `json:"attempt_id,omitempty"`
	Kind           agentharness.ActionKind         `json:"kind,omitempty"`
	RequestDigest  string                          `json:"request_digest,omitempty"`
	AfterCursor    string                          `json:"after_cursor,omitempty"`
	AfterSequence  uint64                          `json:"after_cursor_sequence,omitempty"`
	ActionID       string                          `json:"action_id,omitempty"`
	Result         agentharness.ActionResult       `json:"result,omitempty"`
	RuntimeChildID string                          `json:"runtime_child_id,omitempty"`
	Message        controlplane.Message            `json:"message,omitempty"`
	Callback       controlplane.CompletionCallback `json:"callback,omitempty"`
	CloseEvidence  controlplane.WorkCloseEvidence  `json:"close_evidence,omitempty"`
}

func (s *server) handleObserveAction(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID, attemptID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionWorkObserve); err != nil {
		writeError(writer, err)
		return
	}
	var body actionRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	_, ok := snapshot.Attempts[attemptID]
	if !ok {
		writeError(writer, controlplane.ErrNotFound)
		return
	}
	switch body.Operation {
	case "prepare":
		if !validRequestDigest(body.RequestDigest) || (body.AfterCursor == "") != (body.AfterSequence == 0) {
			writeError(writer, errInvalidRequest)
			return
		}
		if body.RequestDigest != observeRequestDigest(runID, attemptID, body.AfterCursor, body.AfterSequence) {
			writeError(writer, controlplane.ErrActionConflict)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		action, reused, err := s.service.PrepareObserve(
			request.Context(), runID, attemptID, body.RequestDigest, body.AfterCursor, body.AfterSequence,
		)
		if err != nil {
			writeError(writer, err)
			return
		}
		status := http.StatusAccepted
		if reused {
			status = http.StatusOK
		}
		writeJSON(writer, status, map[string]any{"api_version": "v1", "action": action, "reused": reused})
	case "complete":
		if err := validateActionRoute(snapshot, body.ActionID, attemptID, agentharness.ActionObserve); err != nil {
			writeError(writer, err)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		action, updatedAttempt, err := s.completeBoundAction(
			request.Context(), runID, "", attemptID, body.ActionID, body.Result, agentharness.ActionObserve,
		)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"api_version": "v1", "action": action, "attempt": updatedAttempt, "next_cursor": updatedAttempt.LastCursor,
		})
	default:
		writeError(writer, errInvalidRequest)
	}
}

func (s *server) handleAttemptAction(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID, taskID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionWorkDispatch); err != nil {
		writeError(writer, err)
		return
	}
	var body actionRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	switch body.Operation {
	case "prepare":
		if !validRequestDigest(body.RequestDigest) {
			writeError(writer, errInvalidRequest)
			return
		}
		if err := body.Capabilities.Validate(); err != nil {
			writeError(writer, controlplane.ErrCapabilityUnavailable)
			return
		}
		if !principal.Harnesses[body.Capabilities.HarnessID] {
			writeError(writer, auth.ErrForbidden)
			return
		}
		if findTask(snapshot.Graph, taskID) == nil {
			writeError(writer, controlplane.ErrNotFound)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		attempt, err := s.service.CreateAttempt(request.Context(), runID, taskID, body.Capabilities)
		if err != nil {
			writeError(writer, err)
			return
		}
		wasPrepared, err := s.actionExists(request.Context(), runID, attempt.ID, agentharness.ActionDispatch, body.RequestDigest)
		if err != nil {
			writeError(writer, err)
			return
		}
		action, err := s.service.PrepareAction(request.Context(), runID, attempt.ID, agentharness.ActionDispatch, body.RequestDigest)
		if err != nil {
			writeError(writer, err)
			return
		}
		status := http.StatusAccepted
		if wasPrepared {
			status = http.StatusOK
		}
		writeJSON(writer, status, map[string]any{"api_version": "v1", "attempt": attempt, "action": action, "reused": wasPrepared})
	case "complete":
		if err := validateActionRoute(snapshot, body.ActionID, "", agentharness.ActionDispatch); err != nil {
			writeError(writer, err)
			return
		}
		attempt, ok := snapshot.Attempts[snapshot.Actions[body.ActionID].AttemptID]
		if !ok || attempt.TaskID != taskID {
			writeError(writer, controlplane.ErrNotFound)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		action, attempt, err := s.completeBoundAction(request.Context(), runID, taskID, "", body.ActionID, body.Result, agentharness.ActionDispatch)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"api_version": "v1", "attempt": attempt, "action": action, "session": sessionForAttempt(request.Context(), s.service, runID, attempt.ID),
		})
	default:
		writeError(writer, errInvalidRequest)
	}
}

func (s *server) handleAttemptStatus(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID, attemptID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionWorkObserve); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	attempt, ok := snapshot.Attempts[attemptID]
	if !ok {
		writeError(writer, controlplane.ErrNotFound)
		return
	}
	after, err := parseAfterCursor(request, snapshot.Run.EventCursor)
	if err != nil {
		writeError(writer, err)
		return
	}
	events, next, err := s.service.EventsAfter(request.Context(), runID, after, maxEventsPerRead)
	if err != nil {
		writeError(writer, err)
		return
	}
	filtered := make([]controlplane.Event, 0, len(events))
	for _, event := range events {
		if event.AttemptID == "" || event.AttemptID == attemptID {
			filtered = append(filtered, event)
		}
	}
	if attempt.State != controlplane.AttemptCompleted && attempt.State != controlplane.AttemptFailed && attempt.State != controlplane.AttemptCanceled {
		writer.Header().Set("Retry-After", "1")
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"api_version": "v1", "attempt": attempt, "session": findSession(snapshot, attemptID),
		"events": filtered, "next_cursor": strconv.FormatUint(next, 10),
	})
}

func (s *server) handleMessageAction(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID, attemptID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionWorkSend); err != nil {
		writeError(writer, err)
		return
	}
	var body actionRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil || snapshot.Attempts[attemptID].ID == "" {
		if err == nil {
			err = controlplane.ErrNotFound
		}
		writeError(writer, err)
		return
	}
	switch body.Operation {
	case "prepare":
		if !validRequestDigest(body.RequestDigest) {
			writeError(writer, errInvalidRequest)
			return
		}
		if body.Kind != agentharness.ActionSend && body.Kind != agentharness.ActionAcknowledge && body.Kind != agentharness.ActionCallback {
			writeError(writer, errInvalidRequest)
			return
		}
		if body.Kind == agentharness.ActionSend {
			if err := s.validateMessageForSend(principal, snapshot, body.Message); err != nil {
				writeError(writer, err)
				return
			}
			if body.RequestDigest != sendRequestDigest(body.Message) {
				writeError(writer, errInvalidRequest)
				return
			}
		}
		wasPrepared, err := s.actionExists(request.Context(), runID, attemptID, body.Kind, body.RequestDigest)
		if err != nil {
			writeError(writer, err)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		action, err := s.service.PrepareAction(request.Context(), runID, attemptID, body.Kind, body.RequestDigest)
		if err != nil {
			writeError(writer, err)
			return
		}
		status := http.StatusAccepted
		if wasPrepared {
			status = http.StatusOK
		}
		writeJSON(writer, status, map[string]any{"api_version": "v1", "action": action, "reused": wasPrepared})
	case "complete":
		if err := validateActionRoute(
			snapshot, body.ActionID, attemptID,
			agentharness.ActionSend, agentharness.ActionAcknowledge, agentharness.ActionCallback,
		); err != nil {
			writeError(writer, err)
			return
		}
		pending := snapshot.Actions[body.ActionID]
		if pending.Kind == agentharness.ActionSend {
			if err := s.validateMessageForSend(principal, snapshot, body.Message); err != nil {
				writeError(writer, err)
				return
			}
			if pending.RequestDigest != sendRequestDigest(body.Message) {
				writeError(writer, controlplane.ErrActionConflict)
				return
			}
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		if pending.Kind == agentharness.ActionSend {
			action, attempt, message, err := s.service.CompleteSend(
				request.Context(), runID, attemptID, body.ActionID, body.Result, body.Message,
			)
			if err != nil {
				writeError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{
				"api_version": "v1", "action": action, "attempt": attempt, "message": message,
			})
			return
		}
		action, attempt, err := s.completeBoundAction(
			request.Context(), runID, "", attemptID, body.ActionID, body.Result,
			agentharness.ActionSend, agentharness.ActionAcknowledge, agentharness.ActionCallback,
		)
		if err != nil {
			writeError(writer, err)
			return
		}
		response := map[string]any{"api_version": "v1", "action": action, "attempt": attempt}
		switch action.Kind {
		case agentharness.ActionAcknowledge:
			session, err := s.service.AcknowledgeRuntimeID(request.Context(), runID, attemptID, body.RuntimeChildID)
			if err != nil {
				writeError(writer, err)
				return
			}
			response["session"] = session
		case agentharness.ActionCallback:
			callback, err := s.service.RecordCallback(request.Context(), runID, body.Callback)
			if err != nil {
				writeError(writer, err)
				return
			}
			response["callback"] = callback
		}
		writeJSON(writer, http.StatusOK, response)
	default:
		writeError(writer, errInvalidRequest)
	}
}

func (s *server) handleWaitAction(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionWorkWait); err != nil {
		writeError(writer, err)
		return
	}
	var body actionRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	if body.Operation == "prepare" {
		if !validRequestDigest(body.RequestDigest) {
			writeError(writer, errInvalidRequest)
			return
		}
		wasPrepared, err := s.actionExists(request.Context(), runID, body.AttemptID, agentharness.ActionWait, body.RequestDigest)
		if err != nil {
			writeError(writer, err)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		action, err := s.service.PrepareAction(request.Context(), runID, body.AttemptID, agentharness.ActionWait, body.RequestDigest)
		if err != nil {
			writeError(writer, err)
			return
		}
		status := http.StatusAccepted
		if wasPrepared {
			status = http.StatusOK
		}
		writeJSON(writer, status, map[string]any{"api_version": "v1", "action": action, "reused": wasPrepared})
		return
	}
	if body.Operation == "complete" {
		if err := validateActionRoute(snapshot, body.ActionID, "", agentharness.ActionWait); err != nil {
			writeError(writer, err)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		action, attempt, err := s.completeBoundAction(request.Context(), runID, "", "", body.ActionID, body.Result, agentharness.ActionWait)
		if err != nil {
			writeError(writer, err)
			return
		}
		if action.Kind != agentharness.ActionWait {
			writeError(writer, controlplane.ErrActionConflict)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"api_version": "v1", "action": action, "attempt": attempt, "next_cursor": attempt.LastCursor})
		return
	}
	writeError(writer, errInvalidRequest)
}

func (s *server) handleSimpleAction(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID, attemptID string,
	kind agentharness.ActionKind,
	actionScope string,
) {
	if err := s.authenticator.Authorize(principal, actionScope); err != nil {
		writeError(writer, err)
		return
	}
	var body actionRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	if body.Operation == "prepare" {
		if !validRequestDigest(body.RequestDigest) {
			writeError(writer, errInvalidRequest)
			return
		}
		wasPrepared, err := s.actionExists(request.Context(), runID, attemptID, kind, body.RequestDigest)
		if err != nil {
			writeError(writer, err)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		action, err := s.service.PrepareAction(request.Context(), runID, attemptID, kind, body.RequestDigest)
		if err != nil {
			writeError(writer, err)
			return
		}
		status := http.StatusAccepted
		if wasPrepared {
			status = http.StatusOK
		}
		writeJSON(writer, status, map[string]any{"api_version": "v1", "action": action, "reused": wasPrepared})
		return
	}
	if body.Operation == "complete" {
		if err := validateActionRoute(snapshot, body.ActionID, attemptID, kind); err != nil {
			writeError(writer, err)
			return
		}
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		action, attempt, err := s.completeBoundAction(request.Context(), runID, "", attemptID, body.ActionID, body.Result, kind)
		if err != nil {
			writeError(writer, err)
			return
		}
		if action.Kind != kind {
			writeError(writer, controlplane.ErrActionConflict)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"api_version": "v1", "action": action, "attempt": attempt})
		return
	}
	writeError(writer, errInvalidRequest)
}

func (s *server) handleCloseAction(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID, attemptID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionWorkClose); err != nil {
		writeError(writer, err)
		return
	}
	var body actionRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	attempt, ok := snapshot.Attempts[attemptID]
	if !ok {
		writeError(writer, controlplane.ErrNotFound)
		return
	}
	if body.Operation == "local" {
		if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
			writeError(writer, err)
			return
		}
		closed, err := s.service.CloseAttempt(request.Context(), runID, attemptID, body.CloseEvidence)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"api_version": "v1", "attempt": closed})
		return
	}
	if attempt.Primitive == agentharness.LocalSequential {
		writeError(writer, controlplane.ErrActionConflict)
		return
	}
	s.handleSimpleAction(writer, requestWithBody(request, body), principal, runID, attemptID, agentharness.ActionClose, auth.ActionWorkClose)
}

type evidenceRequest struct {
	Operation   string                   `json:"operation"`
	Evidence    controlplane.Evidence    `json:"evidence,omitempty"`
	AttemptID   string                   `json:"attempt_id,omitempty"`
	Reference   controlplane.EvidenceRef `json:"reference,omitempty"`
	Disposition controlplane.Disposition `json:"disposition,omitempty"`
	Handoff     controlplane.Handoff     `json:"handoff,omitempty"`
	HandoffID   string                   `json:"handoff_id,omitempty"`
	MessageID   string                   `json:"message_id,omitempty"`
	TaskID      string                   `json:"task_id,omitempty"`
}

func (s *server) handleEvidence(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionEvidenceWrite); err != nil {
		writeError(writer, err)
		return
	}
	var body evidenceRequest
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	if body.Operation != "record" && body.Operation != "attach_terminal" &&
		body.Operation != "disposition" && body.Operation != "handoff" &&
		body.Operation != "acknowledge_handoff" && body.Operation != "acknowledge_message" {
		writeError(writer, errInvalidRequest)
		return
	}
	if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
		writeError(writer, err)
		return
	}
	var result any
	switch body.Operation {
	case "record":
		result, err = s.service.RecordEvidence(request.Context(), runID, body.Evidence)
	case "attach_terminal":
		result, err = s.service.AttachTerminalEvidence(request.Context(), runID, body.AttemptID, body.Reference)
	case "disposition":
		result, err = s.service.SetDisposition(request.Context(), runID, body.AttemptID, body.Disposition)
	case "handoff":
		result, err = s.service.AddHandoff(request.Context(), runID, body.Handoff)
	case "acknowledge_handoff":
		result, err = s.service.AcknowledgeHandoff(request.Context(), runID, body.HandoffID)
	case "acknowledge_message":
		result, err = s.service.AcknowledgeMessage(request.Context(), runID, body.MessageID, body.TaskID)
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"api_version": "v1", "result": result})
}

func (s *server) handleControlClose(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID string,
) {
	if err := s.authenticator.Authorize(principal, auth.ActionControlClose); err != nil {
		writeError(writer, err)
		return
	}
	var body struct{}
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err := s.loadOwned(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := s.bindMutation(request.Context(), principal, request, body, snapshot.Graph); err != nil {
		writeError(writer, err)
		return
	}
	snapshot, err = s.service.CloseControlRun(request.Context(), runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"api_version": "v1", "snapshot": snapshot})
}

func (s *server) completeBoundAction(
	ctx context.Context,
	runID, taskID, attemptID, actionID string,
	result agentharness.ActionResult,
	allowedKinds ...agentharness.ActionKind,
) (controlplane.LifecycleAction, controlplane.PlacementAttempt, error) {
	snapshot, err := s.service.Load(ctx, runID)
	if err != nil {
		return controlplane.LifecycleAction{}, controlplane.PlacementAttempt{}, err
	}
	if err := validateActionRoute(snapshot, actionID, attemptID, allowedKinds...); err != nil {
		return controlplane.LifecycleAction{}, controlplane.PlacementAttempt{}, err
	}
	action := snapshot.Actions[actionID]
	attempt, ok := snapshot.Attempts[action.AttemptID]
	if !ok || attemptID != "" && attempt.ID != attemptID || taskID != "" && attempt.TaskID != taskID {
		return controlplane.LifecycleAction{}, controlplane.PlacementAttempt{}, controlplane.ErrNotFound
	}
	completed, err := s.service.CompleteAction(ctx, runID, actionID, result)
	if err != nil {
		return controlplane.LifecycleAction{}, controlplane.PlacementAttempt{}, err
	}
	updated, err := s.service.Load(ctx, runID)
	if err != nil {
		return controlplane.LifecycleAction{}, controlplane.PlacementAttempt{}, err
	}
	return completed, updated.Attempts[attempt.ID], nil
}

func validateActionRoute(
	snapshot controlplane.Snapshot,
	actionID, attemptID string,
	allowedKinds ...agentharness.ActionKind,
) error {
	action, ok := snapshot.Actions[actionID]
	if !ok {
		return controlplane.ErrNotFound
	}
	allowed := false
	for _, kind := range allowedKinds {
		if action.Kind == kind {
			allowed = true
			break
		}
	}
	if !allowed {
		return controlplane.ErrActionConflict
	}
	if attemptID != "" && action.AttemptID != attemptID {
		return controlplane.ErrNotFound
	}
	return nil
}

func (s *server) loadOwned(
	ctx context.Context,
	principal submission.Principal,
	runID string,
) (controlplane.Snapshot, error) {
	snapshot, err := s.service.Load(ctx, runID)
	if err != nil {
		return controlplane.Snapshot{}, err
	}
	if snapshot.Run.PrincipalID != principal.CredentialID {
		return controlplane.Snapshot{}, controlplane.ErrNotFound
	}
	return snapshot, nil
}

func (s *server) actionExists(
	ctx context.Context,
	runID, attemptID string,
	kind agentharness.ActionKind,
	requestDigest string,
) (bool, error) {
	snapshot, err := s.service.Load(ctx, runID)
	if err != nil {
		return false, err
	}
	attempt, ok := snapshot.Attempts[attemptID]
	if !ok {
		return false, controlplane.ErrNotFound
	}
	for _, actionID := range attempt.ActionIDs {
		action := snapshot.Actions[actionID]
		if action.Kind == kind && action.RequestDigest == requestDigest {
			return true, nil
		}
	}
	return false, nil
}

func (s *server) authorizeGraph(principal submission.Principal, graph controlplane.TaskGraph) error {
	for _, task := range graph.Tasks {
		if err := s.authorizeTask(principal, task, graph); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) authorizeTask(
	principal submission.Principal,
	task controlplane.Task,
	graph controlplane.TaskGraph,
) error {
	for _, project := range task.Projects {
		if !s.authenticator.AllowsProject(principal, project.ID) || !repositoryAllowed(principal, project.Repository) {
			return auth.ErrForbidden
		}
	}
	for _, edge := range task.Communication {
		allowed := false
		for _, target := range task.Projects {
			if s.authenticator.AllowsCommunication(principal, edge.ProjectID, target.ID) {
				allowed = true
				break
			}
		}
		if !allowed {
			return auth.ErrForbidden
		}
	}
	return nil
}

func (s *server) authorizeMessage(
	principal submission.Principal,
	graph controlplane.TaskGraph,
	message controlplane.Message,
) error {
	if message.FromTaskID == controlplane.ParentAddress || message.ToTaskID == controlplane.ParentAddress {
		return nil
	}
	from := findTask(graph, message.FromTaskID)
	to := findTask(graph, message.ToTaskID)
	if from == nil || to == nil {
		return auth.ErrForbidden
	}
	for _, source := range from.Projects {
		for _, target := range to.Projects {
			if s.authenticator.AllowsCommunication(principal, source.ID, target.ID) {
				return nil
			}
		}
	}
	return auth.ErrForbidden
}

func (s *server) validateMessageForSend(
	principal submission.Principal,
	snapshot controlplane.Snapshot,
	message controlplane.Message,
) error {
	if err := s.authorizeMessage(principal, snapshot.Graph, message); err != nil {
		return err
	}
	if message.ID == "" || !validControlMessageKind(message.Kind) ||
		!validRequestDigest(message.Digest) ||
		!messageGraphScopeAllowed(snapshot.Graph, message.FromTaskID, message.ToTaskID) || message.Acknowledged {
		return controlplane.ErrInvalidGraph
	}
	if existing, ok := snapshot.Messages[message.ID]; ok && !sameSendMessageInput(existing, message) {
		return controlplane.ErrActionConflict
	}
	return nil
}

func validControlMessageKind(kind controlplane.MessageKind) bool {
	switch kind {
	case controlplane.MessageSteering, controlplane.MessageDependencyHandoff, controlplane.MessageNeedsInput:
		return true
	default:
		return false
	}
}

func messageGraphScopeAllowed(graph controlplane.TaskGraph, fromTaskID, toTaskID string) bool {
	from := findTask(graph, fromTaskID)
	to := findTask(graph, toTaskID)
	if fromTaskID == controlplane.ParentAddress {
		return to != nil
	}
	if toTaskID == controlplane.ParentAddress {
		return from != nil
	}
	if from == nil || to == nil || fromTaskID == toTaskID {
		return false
	}
	for _, edge := range from.Communication {
		if edge.TaskID == toTaskID && taskHasProject(*to, edge.ProjectID) {
			return true
		}
	}
	for _, edge := range to.Communication {
		if edge.TaskID == fromTaskID && taskHasProject(*from, edge.ProjectID) {
			return true
		}
	}
	return false
}

func taskHasProject(task controlplane.Task, projectID string) bool {
	for _, project := range task.Projects {
		if project.ID == projectID {
			return true
		}
	}
	return false
}

func parseAfterCursor(request *http.Request, issued uint64) (uint64, error) {
	raw := request.URL.RawQuery
	if raw == "" {
		return 0, nil
	}
	if strings.ContainsAny(raw, "&;%+") {
		return 0, errInvalidRequest
	}
	name, encoded, found := strings.Cut(raw, "=")
	if !found || name != "after_cursor" || encoded == "" || len(encoded) > maxCursorBytes {
		return 0, errInvalidRequest
	}
	value, err := strconv.ParseUint(encoded, 10, 64)
	if err != nil || value > issued {
		return 0, errInvalidRequest
	}
	return value, nil
}

func (s *server) prepareMutation(writer http.ResponseWriter, request *http.Request) bool {
	if request.URL.RawQuery != "" {
		writeError(writer, errInvalidRequest)
		return false
	}
	if err := requireJSON(request); err != nil {
		writeError(writer, err)
		return false
	}
	key, err := requireIdempotencyKey(request)
	if err != nil {
		writeError(writer, err)
		return false
	}
	*request = *request.WithContext(context.WithValue(request.Context(), idempotencyKeyContextKey{}, key))
	return true
}

func (s *server) bindMutation(
	ctx context.Context,
	principal submission.Principal,
	request *http.Request,
	body any,
	graph controlplane.TaskGraph,
) error {
	key, ok := request.Context().Value(idempotencyKeyContextKey{}).(string)
	if !ok || key == "" {
		return errInvalidRequest
	}
	canonical, err := json.Marshal(struct {
		Version      string `json:"version"`
		CredentialID string `json:"credential_id"`
		Method       string `json:"method"`
		Path         string `json:"path"`
		Body         any    `json:"body"`
	}{
		Version: "paje-control-http-idempotency/v1", CredentialID: principal.CredentialID,
		Method: request.Method, Path: request.URL.Path, Body: body,
	})
	if err != nil {
		return errInvalidRequest
	}
	digestSum := sha256.Sum256(canonical)
	requestDigest := "sha256:" + hex.EncodeToString(digestSum[:])
	idSum := sha256.Sum256([]byte("paje-control-http-idempotency-v1\x00" + principal.CredentialID + "\x00" + key))
	bindingID := idempotencyRunPrefix + hex.EncodeToString(idSum[:])
	bindingGraph := controlplane.CloneGraph(graph)
	bindingGraph.ControlRunID = bindingID
	bindingRun := controlplane.ControlRun{
		SchemaVersion: controlplane.SchemaVersion, ID: bindingID, PrincipalID: principal.CredentialID,
		GoalDigest: requestDigest, GraphRevision: bindingGraph.Revision, Status: controlplane.StatusOpen,
	}
	if _, err := s.service.Create(ctx, bindingRun, bindingGraph); err == nil {
		return nil
	} else if !errors.Is(err, controlplane.ErrAlreadyExists) {
		return err
	}
	existing, err := s.service.Load(ctx, bindingID)
	if err != nil {
		return err
	}
	if existing.Run.ID != bindingID || existing.Run.PrincipalID != principal.CredentialID ||
		existing.Run.GoalDigest != requestDigest {
		return controlplane.ErrActionConflict
	}
	return nil
}

func requireJSON(request *http.Request) error {
	values := request.Header.Values("Content-Type")
	if len(values) != 1 || len(values[0]) > 128 {
		return errUnsupportedContentType
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" {
		return errUnsupportedContentType
	}
	if charset, ok := parameters["charset"]; ok && !strings.EqualFold(charset, "utf-8") {
		return errUnsupportedContentType
	}
	return nil
}

func requireIdempotencyKey(request *http.Request) (string, error) {
	values := request.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		return "", errInvalidRequest
	}
	value := values[0]
	if len(value) > maxIdempotencyBytes {
		return "", errHeaderTooLarge
	}
	if len(value) < minIdempotencyBytes || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return "", errInvalidRequest
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", errInvalidRequest
		}
	}
	return value, nil
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxBodyBytes)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return errInvalidRequest
	}
	if err := auth.ValidateExactJSON(raw, destination); err != nil {
		return errInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return errInvalidRequest
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return errInvalidRequest
	}
	return nil
}

func validateUniqueJSONNames(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := visitJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func visitJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		names := make(map[string]bool)
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok || names[name] {
				return errors.New("invalid or duplicate object name")
			}
			names[name] = true
			if err := visitJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := visitJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("array is not closed")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func safePathID(value string) bool {
	if value == "" || len(value) > maxPathIDBytes || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validRequestDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
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

func sameCreateBoundary(snapshot controlplane.Snapshot, run controlplane.ControlRun, graph controlplane.TaskGraph) bool {
	return snapshot.Run.SchemaVersion == run.SchemaVersion && snapshot.Run.ID == run.ID &&
		snapshot.Run.PrincipalID == run.PrincipalID && snapshot.Run.GoalDigest == run.GoalDigest &&
		snapshot.Run.GraphRevision == run.GraphRevision && snapshot.Run.Status == run.Status &&
		reflect.DeepEqual(snapshot.Graph, graph)
}

func exactTaskRevision(graph controlplane.TaskGraph, body struct {
	ExpectedRevision uint64              `json:"expected_revision"`
	Task             controlplane.Task   `json:"task"`
	IntegrationOrder []string            `json:"integration_order"`
	CombinedGates    []controlplane.Gate `json:"combined_gates"`
}) bool {
	if graph.Revision != body.ExpectedRevision+1 ||
		!reflect.DeepEqual(graph.IntegrationOrder, body.IntegrationOrder) ||
		!reflect.DeepEqual(graph.CombinedGates, body.CombinedGates) {
		return false
	}
	for _, task := range graph.Tasks {
		if task.ID == body.Task.ID {
			return reflect.DeepEqual(task, body.Task)
		}
	}
	return false
}

func repositoryAllowed(principal submission.Principal, repository string) bool {
	for _, scope := range principal.Repositories {
		canonical := "https://" + scope.Host + "/" + scope.Owner + "/" + scope.Name + ".git"
		if repository == canonical {
			return true
		}
	}
	return false
}

func sendRequestDigest(message controlplane.Message) string {
	return controlplane.SendRequestDigest(message)
}

func observeRequestDigest(
	runID, attemptID, afterCursor string,
	afterSequence uint64,
) string {
	return controlplane.ObserveRequestDigest(runID, attemptID, afterCursor, afterSequence)
}

func sameSendMessageInput(first, second controlplane.Message) bool {
	return first.ID == second.ID && first.FromTaskID == second.FromTaskID &&
		first.ToTaskID == second.ToTaskID && first.Kind == second.Kind && first.Digest == second.Digest
}

func findTask(graph controlplane.TaskGraph, taskID string) *controlplane.Task {
	for index := range graph.Tasks {
		if graph.Tasks[index].ID == taskID {
			return &graph.Tasks[index]
		}
	}
	return nil
}

func findSession(snapshot controlplane.Snapshot, attemptID string) *controlplane.AgentSession {
	for _, session := range snapshot.Sessions {
		if session.AttemptID == attemptID {
			copy := session
			return &copy
		}
	}
	return nil
}

func sessionForAttempt(
	ctx context.Context,
	service *controlplane.Service,
	runID, attemptID string,
) *controlplane.AgentSession {
	snapshot, err := service.Load(ctx, runID)
	if err != nil {
		return nil
	}
	return findSession(snapshot, attemptID)
}

func requestWithBody(request *http.Request, body actionRequest) *http.Request {
	raw, _ := json.Marshal(body)
	copy := request.Clone(request.Context())
	copy.Body = io.NopCloser(bytes.NewReader(raw))
	copy.ContentLength = int64(len(raw))
	return copy
}
