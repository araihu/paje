package submission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

const (
	minIdempotencyKeyBytes = 16
	maxIdempotencyKeyBytes = 128
)

// Service implements scoped canonical binding over provider-neutral ports.
type Service struct {
	templates      *template.Registry
	store          Store
	trigger        Trigger
	clock          func() time.Time
	systemMaxDepth int

	lockMu sync.Mutex
	locks  map[string]*submissionLock
}

type submissionLock struct {
	mu   sync.Mutex
	refs int
}

// New validates and snapshots the provider-neutral dependency bundle.
func New(dependencies Dependencies) (*Service, error) {
	required := []struct {
		name  string
		value any
	}{
		{name: "template registry", value: dependencies.Templates},
		{name: "submission store", value: dependencies.Store},
		{name: "submission trigger", value: dependencies.Trigger},
	}
	for _, dependency := range required {
		if isNil(dependency.value) {
			return nil, fmt.Errorf("create submission service: %s is required", dependency.name)
		}
	}
	if dependencies.Clock == nil {
		return nil, errors.New("create submission service: clock is required")
	}
	if dependencies.SystemMaxDepth < 0 || dependencies.SystemMaxDepth > 1 {
		return nil, errors.New("create submission service: system maximum depth must be between 0 and 1")
	}
	return &Service{
		templates:      dependencies.Templates,
		store:          dependencies.Store,
		trigger:        dependencies.Trigger,
		clock:          dependencies.Clock,
		systemMaxDepth: dependencies.SystemMaxDepth,
		locks:          make(map[string]*submissionLock),
	}, nil
}

// Submit validates, principal-binds, reserves, and starts one durable leaf run.
func (s *Service) Submit(
	ctx context.Context,
	principal Principal,
	request SubmitRequest,
) (View, bool, error) {
	if err := ctx.Err(); err != nil {
		return View{}, false, err
	}
	if err := validatePrincipal(principal); err != nil {
		return View{}, false, err
	}
	normalizedOrigin, err := validateSubmissionEnvelope(principal, request)
	if err != nil {
		return View{}, false, err
	}

	lockKey := principal.CredentialID + "\x00" + request.IdempotencyKey
	unlock := s.lock(lockKey)
	defer unlock()

	input, canonical, err := s.bindInput(principal, request)
	if err != nil {
		return View{}, false, err
	}
	runID := deriveRunID(principal.CredentialID, request.IdempotencyKey)
	rootRunID, depth, err := s.resolveLineage(ctx, principal, normalizedOrigin, runID)
	if err != nil {
		return View{}, false, err
	}
	normalizedOrigin.ParentRunID = strings.TrimSpace(normalizedOrigin.ParentRunID)
	requestDigest, err := digestBoundRequest(
		request.Template,
		normalizedOrigin,
		canonical,
		rootRunID,
		depth,
	)
	if err != nil {
		return View{}, false, err
	}
	now := s.clock()
	if now.IsZero() {
		return View{}, false, newDomainError(
			ErrProviderUnavailable,
			"submission clock is unavailable",
			nil,
		)
	}
	expected := Record{
		RunID:                 runID,
		CredentialID:          principal.CredentialID,
		RequestDigest:         requestDigest,
		IdempotencyKeyDigest:  digestIdempotencyKey(principal.CredentialID, request.IdempotencyKey),
		Template:              request.Template,
		CanonicalInput:        canonical,
		Origin:                normalizedOrigin,
		RootRunID:             rootRunID,
		Depth:                 depth,
		Trigger:               nil,
		CancellationRequested: nil,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	record, created, err := s.store.Reserve(ctx, Reservation{
		Record:         expected,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return View{}, false, newDomainError(
				ErrIdempotencyConflict,
				"the idempotency key is already bound to different input",
				err,
			)
		}
		return View{}, false, newDomainError(
			ErrProviderUnavailable,
			"the submission store is unavailable",
			err,
		)
	}
	if !sameReservationBinding(record, expected) {
		return View{}, false, newDomainError(
			ErrIdempotencyConflict,
			"the idempotency key is already bound to different input",
			nil,
		)
	}
	if err := validateStoredRecord(record); err != nil {
		return View{}, false, newDomainError(
			ErrProviderUnavailable,
			"the durable submission binding is invalid",
			err,
		)
	}

	if record.Trigger == nil {
		reference, startErr := s.trigger.Start(ctx, TriggerRequest{
			RunID: record.RunID,
			Input: append(json.RawMessage(nil), input...),
		})
		if startErr != nil {
			return View{}, false, newDomainError(
				ErrProviderUnavailable,
				"the submission provider is unavailable",
				startErr,
			)
		}
		if err := validateTriggerReference(reference); err != nil {
			return View{}, false, err
		}
		record, err = s.store.BindTrigger(ctx, record.RunID, reference)
		if err != nil {
			return View{}, false, newDomainError(
				ErrProviderUnavailable,
				"the submission trigger binding is unavailable",
				err,
			)
		}
		if !sameReservationBinding(record, expected) ||
			record.Trigger == nil ||
			*record.Trigger != reference {
			return View{}, false, newDomainError(
				ErrProviderUnavailable,
				"the submission trigger binding is invalid",
				nil,
			)
		}
		if err := validateStoredRecord(record); err != nil {
			return View{}, false, newDomainError(
				ErrProviderUnavailable,
				"the durable submission binding is invalid",
				err,
			)
		}
	}

	return View{Record: cloneRecord(record), Status: StatusAccepted}, !created, nil
}

// Inspect returns a safe provider-neutral projection for the owning principal.
func (s *Service) Inspect(
	ctx context.Context,
	principal Principal,
	runID string,
) (View, error) {
	if err := ctx.Err(); err != nil {
		return View{}, err
	}
	if err := validatePrincipal(principal); err != nil {
		return View{}, err
	}
	if !principal.Actions[ActionRead] {
		return View{}, newDomainError(
			ErrForbidden,
			"the submission action is outside the principal scope",
			nil,
		)
	}
	record, err := s.loadOwned(ctx, principal, runID)
	if err != nil {
		return View{}, err
	}
	return s.inspectRecord(ctx, record)
}

// Cancel durably records cancellation intent before making at most one
// provider cancellation request.
func (s *Service) Cancel(
	ctx context.Context,
	principal Principal,
	runID string,
) (View, bool, error) {
	if err := ctx.Err(); err != nil {
		return View{}, false, err
	}
	if err := validatePrincipal(principal); err != nil {
		return View{}, false, err
	}
	if !principal.Actions[ActionCancel] {
		return View{}, false, newDomainError(
			ErrForbidden,
			"the submission action is outside the principal scope",
			nil,
		)
	}
	if !safeRequired(runID) {
		return View{}, false, newDomainError(ErrNotFound, "the submission was not found", nil)
	}
	unlock := s.lock("cancel\x00" + runID)
	defer unlock()

	record, err := s.loadOwned(ctx, principal, runID)
	if err != nil {
		return View{}, false, err
	}
	if record.Trigger == nil {
		return View{}, false, newDomainError(
			ErrRunNotCancelable,
			"the submission is not cancelable",
			nil,
		)
	}
	current, err := s.inspectRecord(ctx, record)
	if err != nil {
		if record.CancellationRequested != nil &&
			errors.Is(err, ErrProviderUnavailable) {
			return View{
				Record: cloneRecord(record),
				Status: StatusCancellationRequested,
			}, false, nil
		}
		return View{}, false, err
	}
	if terminalStatus(current.Status) {
		return current, false, nil
	}
	if record.CancellationRequested != nil {
		current.Status = StatusCancellationRequested
		return current, false, nil
	}
	now := s.clock()
	if now.IsZero() || now.Before(record.UpdatedAt) {
		return View{}, false, newDomainError(
			ErrProviderUnavailable,
			"the submission clock is unavailable",
			nil,
		)
	}
	beforeCancellation := record
	record, newlyRequested, err := s.store.MarkCancellationRequested(ctx, record.RunID, now)
	if err != nil {
		return View{}, false, newDomainError(
			ErrProviderUnavailable,
			"the submission store is unavailable",
			err,
		)
	}
	if !sameReservationBinding(record, beforeCancellation) ||
		record.CredentialID != principal.CredentialID ||
		record.CancellationRequested == nil ||
		beforeCancellation.Trigger == nil ||
		record.Trigger == nil ||
		*record.Trigger != *beforeCancellation.Trigger ||
		validateStoredRecord(record) != nil {
		return View{}, false, newDomainError(
			ErrProviderUnavailable,
			"the cancellation binding is invalid",
			nil,
		)
	}
	view := View{Record: cloneRecord(record), Status: StatusCancellationRequested}
	if !newlyRequested {
		return view, false, nil
	}
	if err := s.trigger.Cancel(ctx, *record.Trigger); err != nil {
		return View{}, true, newDomainError(
			ErrProviderUnavailable,
			"the submission provider is unavailable",
			err,
		)
	}
	return view, true, nil
}

func (s *Service) loadOwned(
	ctx context.Context,
	principal Principal,
	runID string,
) (Record, error) {
	if !safeRequired(runID) {
		return Record{}, newDomainError(ErrNotFound, "the submission was not found", nil)
	}
	record, err := s.store.Load(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Record{}, newDomainError(ErrNotFound, "the submission was not found", err)
		}
		return Record{}, newDomainError(
			ErrProviderUnavailable,
			"the submission store is unavailable",
			err,
		)
	}
	if record.RunID != runID {
		return Record{}, newDomainError(
			ErrProviderUnavailable,
			"the durable submission lookup binding is invalid",
			nil,
		)
	}
	if record.CredentialID != principal.CredentialID {
		return Record{}, newDomainError(ErrNotFound, "the submission was not found", nil)
	}
	if err := validateStoredRecord(record); err != nil {
		return Record{}, newDomainError(
			ErrProviderUnavailable,
			"the durable submission binding is invalid",
			err,
		)
	}
	return record, nil
}

func (s *Service) inspectRecord(ctx context.Context, record Record) (View, error) {
	if record.Trigger == nil {
		status := StatusAccepted
		if record.CancellationRequested != nil {
			status = StatusCancellationRequested
		}
		return View{Record: cloneRecord(record), Status: status}, nil
	}
	state, err := s.trigger.Inspect(ctx, *record.Trigger)
	if err != nil {
		return View{}, newDomainError(
			ErrProviderUnavailable,
			"the submission provider is unavailable",
			err,
		)
	}
	if err := validateTriggerState(record.RunID, state); err != nil {
		return View{}, err
	}
	status := state.Status
	if record.CancellationRequested != nil && !terminalStatus(status) {
		status = StatusCancellationRequested
	}
	return View{
		Record: cloneRecord(record),
		Status: status,
		Result: cloneResult(state.Result),
	}, nil
}

func validateTriggerState(runID string, state TriggerState) error {
	switch state.Status {
	case StatusAccepted, StatusQueued, StatusRunning, StatusAwaitingApproval,
		StatusCancellationRequested:
		if state.Result != nil {
			return newDomainError(
				ErrProviderUnavailable,
				"the submission provider returned an invalid nonterminal result",
				nil,
			)
		}
		return nil
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusDeclined:
		if state.Result == nil ||
			state.Result.RunID != runID ||
			!resultStatusMatches(state.Status, state.Result.Status) {
			return newDomainError(
				ErrProviderUnavailable,
				"the submission provider returned an invalid terminal result",
				nil,
			)
		}
		return nil
	default:
		return newDomainError(
			ErrProviderUnavailable,
			"the submission provider returned an unknown status",
			nil,
		)
	}
}

func resultStatusMatches(status Status, resultStatus run.Status) bool {
	switch status {
	case StatusSucceeded:
		return resultStatus == run.StatusSucceeded
	case StatusFailed:
		return resultStatus == run.StatusFailed
	case StatusCanceled:
		return resultStatus == run.StatusCanceled
	case StatusDeclined:
		return resultStatus == run.StatusDeclined
	default:
		return false
	}
}

func terminalStatus(status Status) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusDeclined:
		return true
	default:
		return false
	}
}

func validatePrincipal(principal Principal) error {
	if !safeRequired(principal.CredentialID) ||
		!safeRequired(principal.Subject) ||
		!safeRequired(principal.UserID) ||
		!safeRequired(principal.AppID) {
		return newDomainError(
			ErrUnauthenticated,
			"the submission principal is unauthenticated",
			nil,
		)
	}
	if principal.MaxDepth < 0 {
		return newDomainError(ErrForbidden, "the submission principal is forbidden", nil)
	}
	return nil
}

func validateSubmissionEnvelope(principal Principal, request SubmitRequest) (Origin, error) {
	if !validIdempotencyKey(request.IdempotencyKey) {
		return Origin{}, newDomainError(
			ErrInvalidRequest,
			"the idempotency key must contain between 16 and 128 safe bytes",
			nil,
		)
	}
	if request.Template != templatecodechange.ID {
		return Origin{}, newDomainError(ErrInvalidRequest, "the submission template is invalid", nil)
	}
	origin := Origin{
		Harness:     strings.ToLower(strings.TrimSpace(request.Origin.Harness)),
		SessionID:   strings.TrimSpace(request.Origin.SessionID),
		TurnID:      strings.TrimSpace(request.Origin.TurnID),
		ParentRunID: strings.TrimSpace(request.Origin.ParentRunID),
	}
	if !safeRequired(origin.Harness) ||
		!safeRequired(origin.SessionID) ||
		!safeRequired(origin.TurnID) ||
		(origin.ParentRunID != "" && !safeRequired(origin.ParentRunID)) {
		return Origin{}, newDomainError(ErrInvalidRequest, "the submission origin is invalid", nil)
	}
	if !principal.Harnesses[origin.Harness] {
		return Origin{}, newDomainError(ErrForbidden, "the submission harness is forbidden", nil)
	}
	return origin, nil
}

func (s *Service) bindInput(
	principal Principal,
	request SubmitRequest,
) (json.RawMessage, json.RawMessage, error) {
	if err := validateUniqueJSONNames(request.Input); err != nil {
		return nil, nil, newDomainError(
			ErrInvalidRequest,
			"the submission input is invalid",
			err,
		)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(request.Input, &fields); err != nil || fields == nil {
		return nil, nil, newDomainError(
			ErrInvalidRequest,
			"the submission input is invalid",
			err,
		)
	}
	if _, exists := fields["idempotency_key"]; exists {
		return nil, nil, newDomainError(
			ErrInvalidRequest,
			"the nested idempotency key is server controlled",
			nil,
		)
	}
	definition, err := s.templates.Resolve(request.Template)
	if err != nil {
		return nil, nil, newDomainError(
			ErrInvalidRequest,
			"the submission template is invalid",
			err,
		)
	}
	if err := definition.Validate(request.Input); err != nil {
		return nil, nil, newDomainError(
			ErrInvalidRequest,
			"the submission input is invalid",
			err,
		)
	}
	input, err := templatecodechange.Decode(request.Input)
	if err != nil {
		return nil, nil, newDomainError(
			ErrInvalidRequest,
			"the submission input is invalid",
			err,
		)
	}
	if input.Tags["user_id"] != principal.UserID || input.Tags["app_id"] != principal.AppID {
		return nil, nil, newDomainError(
			ErrForbidden,
			"the submission identity is outside the principal scope",
			nil,
		)
	}
	scope, canonicalRepository, err := parseRepository(input.RepositoryURI)
	if err != nil {
		return nil, nil, newDomainError(
			ErrInvalidRequest,
			"the submission repository is invalid",
			err,
		)
	}
	if !repositoryAllowed(principal.Repositories, scope) {
		return nil, nil, newDomainError(
			ErrForbidden,
			"the submission repository is outside the principal scope",
			nil,
		)
	}
	action := ActionSubmitArtifact
	if input.Publication.Mode == "pull_request" {
		action = ActionSubmitPullRequest
	}
	if !principal.Actions[action] {
		return nil, nil, newDomainError(
			ErrForbidden,
			"the submission action is outside the principal scope",
			nil,
		)
	}
	input.Tags = cloneStringMap(input.Tags)
	input.Tags["user_id"] = principal.UserID
	input.Tags["app_id"] = principal.AppID
	input.RepositoryURI = canonicalRepository
	input.IdempotencyKey = deriveNestedIdempotencyKey(
		principal.CredentialID,
		request.IdempotencyKey,
	)
	canonical, err := canonicalCodeChangeInput(input)
	if err != nil {
		return nil, nil, newDomainError(
			ErrInvalidRequest,
			"the submission input cannot be canonicalized",
			err,
		)
	}
	return canonical, canonical, nil
}

func validateUniqueJSONNames(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := visitJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("input contains multiple JSON values")
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
		names := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("object name is invalid")
			}
			if _, exists := names[name]; exists {
				return errors.New("object name is duplicated")
			}
			names[name] = struct{}{}
			if err := visitJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := visitJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("array is not closed")
		}
	default:
		return errors.New("JSON delimiter is invalid")
	}
	return nil
}

func (s *Service) resolveLineage(
	ctx context.Context,
	principal Principal,
	origin Origin,
	runID string,
) (string, int, error) {
	if origin.ParentRunID == "" {
		return runID, 0, nil
	}
	if origin.ParentRunID == runID {
		return "", 0, newDomainError(
			ErrInvalidRequest,
			"a submission cannot be its own parent",
			nil,
		)
	}
	parent, err := s.store.Load(ctx, origin.ParentRunID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", 0, newDomainError(ErrNotFound, "the parent submission was not found", err)
		}
		return "", 0, newDomainError(
			ErrProviderUnavailable,
			"the submission store is unavailable",
			err,
		)
	}
	if parent.CredentialID != principal.CredentialID ||
		parent.Origin.Harness != origin.Harness {
		return "", 0, newDomainError(
			ErrForbidden,
			"the parent submission is outside the principal scope",
			nil,
		)
	}
	if err := validateStoredRecord(parent); err != nil {
		return "", 0, newDomainError(
			ErrInvalidRequest,
			"the parent submission lineage is invalid",
			err,
		)
	}
	if parent.Depth > 0 {
		root, loadErr := s.store.Load(ctx, parent.RootRunID)
		if loadErr != nil ||
			root.CredentialID != principal.CredentialID ||
			root.Origin.Harness != origin.Harness ||
			root.Depth != 0 ||
			root.RootRunID != root.RunID ||
			validateStoredRecord(root) != nil {
			return "", 0, newDomainError(
				ErrInvalidRequest,
				"the parent submission lineage is invalid",
				loadErr,
			)
		}
	}
	depth := parent.Depth + 1
	maxDepth := min(principal.MaxDepth, s.systemMaxDepth)
	if depth > maxDepth {
		return "", 0, newDomainError(
			ErrDepthExceeded,
			"the submission depth exceeds the allowed maximum",
			nil,
		)
	}
	return parent.RootRunID, depth, nil
}

func canonicalCodeChangeInput(input templatecodechange.Input) (json.RawMessage, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	memoryLimit, err := json.Marshal(input.MemoryLimit)
	if err != nil {
		return nil, err
	}
	fields["memory_limit"] = memoryLimit
	encoded, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return run.CanonicalInput(encoded)
}

func digestBoundRequest(
	templateID template.ID,
	origin Origin,
	input json.RawMessage,
	rootRunID string,
	depth int,
) (string, error) {
	binding := struct {
		Template  template.ID     `json:"template"`
		Origin    Origin          `json:"origin"`
		Input     json.RawMessage `json:"input"`
		RootRunID string          `json:"root_run_id"`
		Depth     int             `json:"depth"`
	}{
		Template: templateID, Origin: origin, Input: input,
		RootRunID: rootRunID, Depth: depth,
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return "", newDomainError(
			ErrInvalidRequest,
			"the submission binding cannot be canonicalized",
			err,
		)
	}
	canonical, err := run.CanonicalInput(encoded)
	if err != nil {
		return "", newDomainError(
			ErrInvalidRequest,
			"the submission binding cannot be canonicalized",
			err,
		)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func deriveRunID(credentialID, key string) string {
	sum := sha256.Sum256([]byte(
		"paje-run-v1\x00" + credentialID + "\x00" + key,
	))
	return "paje_" + strings.ToLower(
		base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16]),
	)
}

func deriveNestedIdempotencyKey(credentialID, key string) string {
	sum := sha256.Sum256([]byte(
		"paje-input-v1\x00" + credentialID + "\x00" + key,
	))
	return hex.EncodeToString(sum[:])
}

func digestIdempotencyKey(credentialID, key string) string {
	sum := sha256.Sum256([]byte(
		"paje-key-v1\x00" + credentialID + "\x00" + key,
	))
	return hex.EncodeToString(sum[:])
}

func parseRepository(raw string) (RepositoryScope, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return RepositoryScope{}, "", errors.New("repository is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil ||
		parsed.Scheme != "https" ||
		parsed.Opaque != "" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		parsed.Hostname() == "" ||
		parsed.Port() != "" ||
		parsed.RawPath != "" {
		return RepositoryScope{}, "", errors.New("repository must be a credential-free HTTPS URL")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 2 {
		return RepositoryScope{}, "", errors.New("repository path must contain owner and repository")
	}
	scope := RepositoryScope{
		Host:  strings.ToLower(parsed.Hostname()),
		Owner: parts[0],
		Name:  strings.TrimSuffix(parts[1], ".git"),
	}
	canonical, err := canonicalRepositoryScope(scope)
	if err != nil {
		return RepositoryScope{}, "", err
	}
	if canonical.Host == "github.com" {
		canonical.Owner = strings.ToLower(canonical.Owner)
		canonical.Name = strings.ToLower(canonical.Name)
	}
	uri := "https://" + canonical.Host + "/" + canonical.Owner + "/" + canonical.Name + ".git"
	return canonical, uri, nil
}

func canonicalRepositoryScope(scope RepositoryScope) (RepositoryScope, error) {
	scope.Host = strings.ToLower(strings.TrimSpace(scope.Host))
	scope.Owner = strings.TrimSpace(scope.Owner)
	scope.Name = strings.TrimSuffix(strings.TrimSpace(scope.Name), ".git")
	if scope.Host == "" ||
		strings.ContainsAny(scope.Host, "/:@\x00\r\n") ||
		!safeRepositoryComponent(scope.Owner) ||
		!safeRepositoryComponent(scope.Name) {
		return RepositoryScope{}, errors.New("repository scope is invalid")
	}
	if scope.Host == "github.com" {
		scope.Owner = strings.ToLower(scope.Owner)
		scope.Name = strings.ToLower(scope.Name)
	}
	return scope, nil
}

func repositoryAllowed(scopes []RepositoryScope, candidate RepositoryScope) bool {
	for _, scope := range scopes {
		canonical, err := canonicalRepositoryScope(scope)
		if err == nil && canonical == candidate {
			return true
		}
	}
	return false
}

func safeRepositoryComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if !('a' <= character && character <= 'z') &&
			!('A' <= character && character <= 'Z') &&
			!('0' <= character && character <= '9') &&
			character != '-' &&
			character != '_' &&
			character != '.' {
			return false
		}
	}
	return true
}

func validIdempotencyKey(value string) bool {
	if len(value) < minIdempotencyKeyBytes ||
		len(value) > maxIdempotencyKeyBytes ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func safeRequired(value string) bool {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validLineageRecord(record Record) bool {
	if !safeRequired(record.RunID) ||
		!safeRequired(record.RootRunID) ||
		record.Depth < 0 {
		return false
	}
	if record.Depth == 0 {
		return record.RootRunID == record.RunID
	}
	return record.RootRunID != record.RunID
}

func validateStoredRecord(record Record) error {
	canonical, err := run.CanonicalInput(record.CanonicalInput)
	if err != nil {
		return errors.New("canonical input is invalid")
	}
	switch {
	case !safeRequired(record.RunID):
		return errors.New("run ID is invalid")
	case !safeRequired(record.CredentialID):
		return errors.New("credential ID is invalid")
	case !validDigest(record.RequestDigest):
		return errors.New("request digest is invalid")
	case !validDigest(record.IdempotencyKeyDigest):
		return errors.New("idempotency key digest is invalid")
	case record.Template != templatecodechange.ID:
		return errors.New("template is invalid")
	case !bytes.Equal(record.CanonicalInput, canonical):
		return errors.New("canonical input is not canonical JSON")
	case !safeRequired(record.Origin.Harness) ||
		!safeRequired(record.Origin.SessionID) ||
		!safeRequired(record.Origin.TurnID):
		return errors.New("origin is invalid")
	case !validLineageRecord(record):
		return errors.New("lineage is invalid")
	case record.Depth == 0 && record.Origin.ParentRunID != "":
		return errors.New("root record has a parent")
	case record.Depth > 0 && !safeRequired(record.Origin.ParentRunID):
		return errors.New("child record has no parent")
	case record.CreatedAt.IsZero() || record.UpdatedAt.IsZero():
		return errors.New("timestamps are required")
	case record.UpdatedAt.Before(record.CreatedAt):
		return errors.New("updated time precedes created time")
	case record.CancellationRequested != nil &&
		(record.CancellationRequested.Before(record.CreatedAt) ||
			record.CancellationRequested.After(record.UpdatedAt)):
		return errors.New("cancellation time is invalid")
	}
	if record.Trigger != nil &&
		(!safeRequired(record.Trigger.Provider) ||
			!safeRequired(record.Trigger.ExternalRunID)) {
		return errors.New("trigger reference is invalid")
	}
	expectedDigest, err := digestBoundRequest(
		record.Template,
		record.Origin,
		canonical,
		record.RootRunID,
		record.Depth,
	)
	if err != nil {
		return errors.New("request binding cannot be canonicalized")
	}
	if record.RequestDigest != expectedDigest {
		return errors.New("request digest does not match the durable binding")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !('0' <= character && character <= '9') &&
			!('a' <= character && character <= 'f') {
			return false
		}
	}
	return true
}

func sameReservationBinding(record, expected Record) bool {
	return record.RunID == expected.RunID &&
		record.CredentialID == expected.CredentialID &&
		record.RequestDigest == expected.RequestDigest &&
		record.IdempotencyKeyDigest == expected.IdempotencyKeyDigest &&
		record.Template == expected.Template &&
		bytes.Equal(record.CanonicalInput, expected.CanonicalInput) &&
		record.Origin == expected.Origin &&
		record.RootRunID == expected.RootRunID &&
		record.Depth == expected.Depth
}

func validateTriggerReference(reference TriggerReference) error {
	if !safeRequired(reference.Provider) || !safeRequired(reference.ExternalRunID) {
		return newDomainError(
			ErrProviderUnavailable,
			"the submission provider returned an invalid reference",
			nil,
		)
	}
	return nil
}

func cloneRecord(source Record) Record {
	cloned := source
	cloned.CanonicalInput = append(json.RawMessage(nil), source.CanonicalInput...)
	if source.Trigger != nil {
		value := *source.Trigger
		cloned.Trigger = &value
	}
	if source.CancellationRequested != nil {
		value := *source.CancellationRequested
		cloned.CancellationRequested = &value
	}
	return cloned
}

func cloneResult(source *templatecodechange.Result) *templatecodechange.Result {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Verification = append(cloned.Verification[:0:0], source.Verification...)
	for index := range cloned.Verification {
		cloned.Verification[index].Command.Args = append(
			[]string(nil),
			source.Verification[index].Command.Args...,
		)
		cloned.Verification[index].Command.Environment = cloneStringMap(
			source.Verification[index].Command.Environment,
		)
	}
	if source.Approval != nil {
		value := *source.Approval
		cloned.Approval = &value
	}
	if source.Publication != nil {
		value := *source.Publication
		cloned.Publication = &value
	}
	if source.Failure != nil {
		value := *source.Failure
		cloned.Failure = &value
	}
	return &cloned
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (s *Service) lock(key string) func() {
	s.lockMu.Lock()
	entry := s.locks[key]
	if entry == nil {
		entry = &submissionLock{}
		s.locks[key] = entry
	}
	entry.refs++
	s.lockMu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		s.lockMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.locks, key)
		}
		s.lockMu.Unlock()
	}
}
