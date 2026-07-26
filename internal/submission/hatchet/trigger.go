// Package hatchet implements the provider-neutral submission trigger at the
// Hatchet provider edge.
package hatchet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"

	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/submission"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/workflow/codechangehatchet"
	"github.com/google/uuid"
)

const (
	maxCanonicalInputBytes = 1 << 20
	maxEnvelopeBytes       = maxCanonicalInputBytes + 1024
	maxFinalizeOutputBytes = 1 << 20
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Trigger starts, observes, and cancels the canonical code-change workflow.
type Trigger struct {
	client Client
}

// New constructs a Hatchet-backed provider-neutral trigger.
func New(client Client) (*Trigger, error) {
	if client == nil || nilClient(client) {
		return nil, fmt.Errorf("create Hatchet submission trigger: client is required")
	}
	return &Trigger{client: client}, nil
}

func (t *Trigger) Start(
	ctx context.Context,
	request submission.TriggerRequest,
) (submission.TriggerReference, error) {
	if err := ctx.Err(); err != nil {
		return submission.TriggerReference{}, err
	}
	input, err := validateStartRequest(request)
	if err != nil {
		return submission.TriggerReference{}, err
	}

	externalRunID, err := t.client.Start(ctx, codechangehatchet.WorkflowName, map[string]any{
		"run_id": request.RunID,
		"input":  json.RawMessage(input),
	})
	if err == nil {
		if !canonicalUUID(externalRunID) {
			return submission.TriggerReference{}, unavailable("start Hatchet workflow")
		}
		return reference(externalRunID), nil
	}
	if cancellation := cancellationError(ctx, err); cancellation != nil {
		return submission.TriggerReference{}, cancellation
	}

	var collision *IdempotencyCollisionError
	if !errors.As(err, &collision) {
		return submission.TriggerReference{}, unavailable("start Hatchet workflow")
	}
	if collision == nil || !canonicalUUID(collision.ExternalRunID) {
		return submission.TriggerReference{}, unavailable("reconcile Hatchet workflow")
	}
	details, detailsErr := t.client.Details(ctx, collision.ExternalRunID)
	if detailsErr != nil {
		if cancellation := cancellationError(ctx, detailsErr); cancellation != nil {
			return submission.TriggerReference{}, cancellation
		}
		return submission.TriggerReference{}, unavailable("reconcile Hatchet workflow")
	}
	if err := validateDetailsIdentity(details, collision.ExternalRunID); err != nil {
		return submission.TriggerReference{}, unavailable("reconcile Hatchet workflow")
	}
	binding, err := decodeEnvelope(details.Input)
	if err != nil {
		return submission.TriggerReference{}, unavailable("reconcile Hatchet workflow")
	}
	if binding.RunID != request.RunID || !bytes.Equal(binding.Input, input) {
		return submission.TriggerReference{}, fmt.Errorf(
			"reconcile Hatchet workflow: %w",
			submission.ErrIdempotencyConflict,
		)
	}
	return reference(collision.ExternalRunID), nil
}

func (t *Trigger) Inspect(
	ctx context.Context,
	reference submission.TriggerReference,
) (submission.TriggerState, error) {
	if err := ctx.Err(); err != nil {
		return submission.TriggerState{}, err
	}
	if err := validateReference(reference); err != nil {
		return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
	}
	details, err := t.client.Details(ctx, reference.ExternalRunID)
	if err != nil {
		if cancellation := cancellationError(ctx, err); cancellation != nil {
			return submission.TriggerState{}, cancellation
		}
		return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
	}
	if err := validateDetailsIdentity(details, reference.ExternalRunID); err != nil {
		return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
	}
	binding, err := decodeEnvelope(details.Input)
	if err != nil {
		return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
	}

	switch details.Status {
	case RunStatusQueued:
		if details.Done || details.Finalize != nil {
			return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
		}
		return submission.TriggerState{Status: submission.StatusQueued}, nil
	case RunStatusRunning:
		if details.Done || details.Finalize != nil {
			return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
		}
		return submission.TriggerState{Status: submission.StatusRunning}, nil
	case RunStatusFailed:
		return t.providerTerminal(details, binding.RunID, submission.StatusFailed, run.StatusFailed)
	case RunStatusCanceled:
		return t.providerTerminal(details, binding.RunID, submission.StatusCanceled, run.StatusCanceled)
	case RunStatusCompleted:
		if !details.Done || details.Finalize == nil || details.Finalize.Status != RunStatusCompleted {
			return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
		}
		result, resultErr := decodeFinalize(details.Finalize.Output, binding.RunID)
		if resultErr != nil {
			return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
		}
		status, ok := submissionStatus(result.Status)
		if !ok {
			return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
		}
		return submission.TriggerState{Status: status, Result: result}, nil
	default:
		return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
	}
}

func (t *Trigger) providerTerminal(
	details Details,
	runID string,
	status submission.Status,
	resultStatus run.Status,
) (submission.TriggerState, error) {
	if !details.Done {
		return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
	}
	if details.Finalize == nil {
		return submission.TriggerState{
			Status: status,
			Result: &templatecodechange.Result{RunID: runID, Status: resultStatus},
		}, nil
	}
	if details.Finalize.Status == details.Status && len(details.Finalize.Output) == 0 {
		return submission.TriggerState{
			Status: status,
			Result: &templatecodechange.Result{RunID: runID, Status: resultStatus},
		}, nil
	}
	if details.Finalize.Status != RunStatusCompleted {
		return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
	}
	result, err := decodeFinalize(details.Finalize.Output, runID)
	if err != nil || result.Status != resultStatus {
		return submission.TriggerState{}, unavailable("inspect Hatchet workflow")
	}
	return submission.TriggerState{Status: status, Result: result}, nil
}

func (t *Trigger) Cancel(
	ctx context.Context,
	reference submission.TriggerReference,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateReference(reference); err != nil {
		return fmt.Errorf("cancel Hatchet workflow: %w", submission.ErrRunNotCancelable)
	}
	if err := t.client.Cancel(ctx, reference.ExternalRunID); err == nil {
		return nil
	} else if cancellation := cancellationError(ctx, err); cancellation != nil {
		return cancellation
	}

	state, err := t.Inspect(ctx, reference)
	if err != nil {
		return err
	}
	if terminalSubmissionStatus(state.Status) {
		return nil
	}
	return unavailable("cancel Hatchet workflow")
}

type workflowEnvelope struct {
	RunID string          `json:"run_id"`
	Input json.RawMessage `json:"input"`
}

func validateStartRequest(request submission.TriggerRequest) (json.RawMessage, error) {
	if !runIDPattern.MatchString(request.RunID) {
		return nil, fmt.Errorf("start Hatchet workflow: %w", submission.ErrInvalidRequest)
	}
	if len(request.Input) == 0 || len(request.Input) > maxCanonicalInputBytes {
		return nil, fmt.Errorf("start Hatchet workflow: %w", submission.ErrInvalidRequest)
	}
	canonical, err := run.CanonicalInput(request.Input)
	if err != nil || !bytes.Equal(canonical, request.Input) {
		return nil, fmt.Errorf("start Hatchet workflow: %w", submission.ErrInvalidRequest)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
		return nil, fmt.Errorf("start Hatchet workflow: %w", submission.ErrInvalidRequest)
	}
	return append(json.RawMessage(nil), canonical...), nil
}

func decodeEnvelope(raw json.RawMessage) (workflowEnvelope, error) {
	if len(raw) == 0 || len(raw) > maxEnvelopeBytes {
		return workflowEnvelope{}, fmt.Errorf("workflow input size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var envelope workflowEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return workflowEnvelope{}, err
	}
	if err := requireEOF(decoder); err != nil {
		return workflowEnvelope{}, err
	}
	if !runIDPattern.MatchString(envelope.RunID) {
		return workflowEnvelope{}, fmt.Errorf("workflow run ID is invalid")
	}
	canonical, err := run.CanonicalInput(envelope.Input)
	if err != nil || len(canonical) == 0 || len(canonical) > maxCanonicalInputBytes ||
		!bytes.Equal(envelope.Input, canonical) {
		return workflowEnvelope{}, fmt.Errorf("workflow input is invalid")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
		return workflowEnvelope{}, fmt.Errorf("workflow input is invalid")
	}
	envelope.Input = canonical
	return envelope, nil
}

func decodeFinalize(raw json.RawMessage, runID string) (*templatecodechange.Result, error) {
	if len(raw) == 0 || len(raw) > maxFinalizeOutputBytes {
		return nil, fmt.Errorf("finalize output size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result templatecodechange.Result
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if err := requireEOF(decoder); err != nil {
		return nil, err
	}
	if result.RunID != runID {
		return nil, fmt.Errorf("finalize output run ID is invalid")
	}
	if _, ok := submissionStatus(result.Status); !ok {
		return nil, fmt.Errorf("finalize output status is invalid")
	}
	return &result, nil
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func validateReference(reference submission.TriggerReference) error {
	if reference.Provider != "hatchet" || !canonicalUUID(reference.ExternalRunID) {
		return fmt.Errorf("invalid Hatchet workflow reference")
	}
	return nil
}

func validateDetailsIdentity(details Details, externalRunID string) error {
	if details.ExternalRunID != externalRunID || !canonicalUUID(details.ExternalRunID) {
		return fmt.Errorf("Hatchet workflow details identity is invalid")
	}
	return nil
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func reference(externalRunID string) submission.TriggerReference {
	return submission.TriggerReference{Provider: "hatchet", ExternalRunID: externalRunID}
}

func submissionStatus(status run.Status) (submission.Status, bool) {
	switch status {
	case run.StatusSucceeded:
		return submission.StatusSucceeded, true
	case run.StatusFailed:
		return submission.StatusFailed, true
	case run.StatusCanceled:
		return submission.StatusCanceled, true
	case run.StatusDeclined:
		return submission.StatusDeclined, true
	default:
		return "", false
	}
}

func terminalSubmissionStatus(status submission.Status) bool {
	switch status {
	case submission.StatusSucceeded, submission.StatusFailed,
		submission.StatusCanceled, submission.StatusDeclined:
		return true
	default:
		return false
	}
}

func cancellationError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func nilClient(client Client) bool {
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func unavailable(operation string) error {
	return fmt.Errorf("%s: %w", operation, submission.ErrProviderUnavailable)
}

var _ submission.Trigger = (*Trigger)(nil)
