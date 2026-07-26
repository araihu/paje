package hatchet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/submission"
	hatchettrigger "github.com/araihu/paje/internal/submission/hatchet"
	"github.com/araihu/paje/internal/workflow/codechangehatchet"
)

const (
	externalRunID = "7ffeb1fe-986b-4a1b-aec1-722b8151c138"
	secondRunID   = "c32fb56b-d643-4e4b-b89d-5b43cf72deca"
)

type fakeClient struct {
	startID      string
	startErr     error
	details      hatchettrigger.Details
	detailsErr   error
	cancelErr    error
	workflow     string
	envelope     map[string]any
	detailsIDs   []string
	cancelIDs    []string
	startCalls   int
	detailsCalls int
	cancelCalls  int
}

func (c *fakeClient) Start(_ context.Context, workflow string, envelope map[string]any) (string, error) {
	c.startCalls++
	c.workflow = workflow
	c.envelope = envelope
	return c.startID, c.startErr
}

func (c *fakeClient) Details(_ context.Context, externalID string) (hatchettrigger.Details, error) {
	c.detailsCalls++
	c.detailsIDs = append(c.detailsIDs, externalID)
	return c.details, c.detailsErr
}

func (c *fakeClient) Cancel(_ context.Context, externalID string) error {
	c.cancelCalls++
	c.cancelIDs = append(c.cancelIDs, externalID)
	return c.cancelErr
}

func TestNewRequiresClient(t *testing.T) {
	if _, err := hatchettrigger.New(nil); err == nil {
		t.Fatal("New(nil) error = nil")
	}
}

func TestNewRejectsTypedNilClient(t *testing.T) {
	var client *fakeClient
	if _, err := hatchettrigger.New(client); err == nil {
		t.Fatal("New(typed nil) error = nil")
	}
}

func TestStartUsesCanonicalWorkflowEnvelope(t *testing.T) {
	client := &fakeClient{startID: externalRunID}
	trigger := newTrigger(t, client)
	input := json.RawMessage(`{"task_description":"change"}`)

	ref, err := trigger.Start(context.Background(), submission.TriggerRequest{
		RunID: "paje_abc", Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	if codechangehatchet.WorkflowName != "paje-code-change-v1" {
		t.Fatalf("shared workflow name = %q", codechangehatchet.WorkflowName)
	}
	if client.workflow != codechangehatchet.WorkflowName ||
		client.envelope["run_id"] != "paje_abc" {
		t.Fatalf("start = %q %#v", client.workflow, client.envelope)
	}
	gotInput, ok := client.envelope["input"].(json.RawMessage)
	if !ok || !bytes.Equal(gotInput, input) {
		t.Fatalf("input = %#v", client.envelope["input"])
	}
	if ref.Provider != "hatchet" || ref.ExternalRunID != client.startID {
		t.Fatalf("reference = %#v", ref)
	}
}

func TestStartRejectsInvalidRequestBeforeProvider(t *testing.T) {
	tests := []submission.TriggerRequest{
		{},
		{RunID: " ", Input: json.RawMessage(`{}`)},
		{RunID: "paje_abc"},
		{RunID: "paje_abc", Input: json.RawMessage(`null`)},
		{RunID: "paje_abc", Input: json.RawMessage(`{"z":1,"a":2}`)},
	}
	for _, request := range tests {
		client := &fakeClient{startID: externalRunID}
		trigger := newTrigger(t, client)
		if _, err := trigger.Start(context.Background(), request); !errors.Is(err, submission.ErrInvalidRequest) {
			t.Fatalf("Start(%#v) error = %v", request, err)
		}
		if client.startCalls != 0 {
			t.Fatalf("invalid request reached provider: %#v", request)
		}
	}
}

func TestStartProviderErrorIsBoundedAndSafe(t *testing.T) {
	client := &fakeClient{startErr: errors.New("Authorization: Bearer super-secret; body=provider-internal")}
	trigger := newTrigger(t, client)
	_, err := trigger.Start(context.Background(), validRequest())
	assertSafeProviderError(t, err)
}

func TestMaximumCanonicalInputReconcilesAfterCollision(t *testing.T) {
	prefix, suffix := `{"payload":"`, `"}`
	input := json.RawMessage(prefix + strings.Repeat("x", (1<<20)-len(prefix)-len(suffix)) + suffix)
	request := submission.TriggerRequest{RunID: "paje_bound", Input: input}
	client := collisionClient(request)
	trigger := newTrigger(t, client)

	ref, err := trigger.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if ref.ExternalRunID != externalRunID {
		t.Fatalf("reference = %#v", ref)
	}
}

func TestStartCollisionReconcilesExactBinding(t *testing.T) {
	client := collisionClient(validRequest())
	trigger := newTrigger(t, client)
	ref, err := trigger.Start(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if ref != (submission.TriggerReference{Provider: "hatchet", ExternalRunID: externalRunID}) {
		t.Fatalf("reference = %#v", ref)
	}
	if client.detailsCalls != 1 || client.detailsIDs[0] != externalRunID {
		t.Fatalf("details calls = %#v", client.detailsIDs)
	}
}

func TestStartCollisionRejectsDifferentRunID(t *testing.T) {
	client := collisionClient(submission.TriggerRequest{
		RunID: "paje_other", Input: validRequest().Input,
	})
	trigger := newTrigger(t, client)
	_, err := trigger.Start(context.Background(), validRequest())
	if !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestStartCollisionRejectsDifferentInput(t *testing.T) {
	client := collisionClient(submission.TriggerRequest{
		RunID: validRequest().RunID,
		Input: json.RawMessage(`{"task_description":"different"}`),
	})
	trigger := newTrigger(t, client)
	_, err := trigger.Start(context.Background(), validRequest())
	if !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestStartCollisionRejectsWrongExternalRunID(t *testing.T) {
	client := collisionClient(validRequest())
	client.details.ExternalRunID = secondRunID
	trigger := newTrigger(t, client)
	_, err := trigger.Start(context.Background(), validRequest())
	if !errors.Is(err, submission.ErrProviderUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestStartCollisionRejectsMalformedEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		request  submission.TriggerRequest
		envelope json.RawMessage
	}{
		{
			name:     "unknown outer field",
			request:  validRequest(),
			envelope: json.RawMessage(`{"run_id":"paje_bound","input":{},"provider_token":"secret"}`),
		},
		{
			name:     "noncanonical nested whitespace",
			request:  validRequest(),
			envelope: providerEnvelopeJSON(validRequest().RunID, json.RawMessage(` { "task_description" : "change" } `)),
		},
		{
			name: "reordered nested keys",
			request: submission.TriggerRequest{
				RunID: validRequest().RunID,
				Input: json.RawMessage(`{"a":1,"z":2}`),
			},
			envelope: providerEnvelopeJSON(validRequest().RunID, json.RawMessage(`{"z":2,"a":1}`)),
		},
		{
			name:     "duplicate nested key",
			request:  validRequest(),
			envelope: providerEnvelopeJSON(validRequest().RunID, json.RawMessage(`{"task_description":"super-secret","task_description":"change"}`)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := collisionClient(test.request)
			client.details.Input = test.envelope
			trigger := newTrigger(t, client)
			ref, err := trigger.Start(context.Background(), test.request)
			assertSafeProviderError(t, err)
			if ref != (submission.TriggerReference{}) {
				t.Fatalf("reference = %#v, want no reconciled binding", ref)
			}
			if client.startCalls != 1 || client.detailsCalls != 1 {
				t.Fatalf("provider calls start=%d details=%d", client.startCalls, client.detailsCalls)
			}
		})
	}
}

func TestStartCollisionDetailsErrorIsSafe(t *testing.T) {
	client := collisionClient(validRequest())
	client.detailsErr = errors.New("token=super-secret body=provider-internal")
	trigger := newTrigger(t, client)
	_, err := trigger.Start(context.Background(), validRequest())
	assertSafeProviderError(t, err)
}

func TestInspectMapsQueued(t *testing.T) {
	state := inspectState(t, baseDetails(hatchettrigger.RunStatusQueued, false))
	if state.Status != submission.StatusQueued || state.Result != nil {
		t.Fatalf("state = %#v", state)
	}
}

func TestInspectMapsRunning(t *testing.T) {
	state := inspectState(t, baseDetails(hatchettrigger.RunStatusRunning, false))
	if state.Status != submission.StatusRunning || state.Result != nil {
		t.Fatalf("state = %#v", state)
	}
}

func TestInspectMapsFailed(t *testing.T) {
	for name, details := range map[string]hatchettrigger.Details{
		"without finalize task": baseDetails(hatchettrigger.RunStatusFailed, true),
		"failed finalize task": withFinalize(
			baseDetails(hatchettrigger.RunStatusFailed, true),
			hatchettrigger.RunStatusFailed,
			nil,
		),
	} {
		t.Run(name, func(t *testing.T) {
			state := inspectState(t, details)
			if state.Status != submission.StatusFailed || state.Result == nil ||
				state.Result.RunID != validRequest().RunID || state.Result.Status != run.StatusFailed {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestInspectMapsCanceled(t *testing.T) {
	for name, details := range map[string]hatchettrigger.Details{
		"without finalize task": baseDetails(hatchettrigger.RunStatusCanceled, true),
		"canceled finalize task": withFinalize(
			baseDetails(hatchettrigger.RunStatusCanceled, true),
			hatchettrigger.RunStatusCanceled,
			nil,
		),
	} {
		t.Run(name, func(t *testing.T) {
			state := inspectState(t, details)
			if state.Status != submission.StatusCanceled || state.Result == nil ||
				state.Result.RunID != validRequest().RunID || state.Result.Status != run.StatusCanceled {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestInspectMapsCompletedSucceeded(t *testing.T) {
	state := inspectState(t, completedDetails(run.StatusSucceeded))
	if state.Status != submission.StatusSucceeded || state.Result == nil ||
		state.Result.Status != run.StatusSucceeded {
		t.Fatalf("state = %#v", state)
	}
}

func TestInspectMapsCompletedFailed(t *testing.T) {
	tests := []struct {
		result run.Status
		want   submission.Status
	}{
		{result: run.StatusFailed, want: submission.StatusFailed},
		{result: run.StatusCanceled, want: submission.StatusCanceled},
	}
	for _, test := range tests {
		t.Run(string(test.result), func(t *testing.T) {
			state := inspectState(t, completedDetails(test.result))
			if state.Status != test.want || state.Result == nil || state.Result.Status != test.result {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestInspectMapsCompletedDeclined(t *testing.T) {
	state := inspectState(t, completedDetails(run.StatusDeclined))
	if state.Status != submission.StatusDeclined || state.Result == nil ||
		state.Result.Status != run.StatusDeclined {
		t.Fatalf("state = %#v", state)
	}
}

func TestInspectRejectsCompletedWithoutFinalize(t *testing.T) {
	client := &fakeClient{details: baseDetails(hatchettrigger.RunStatusCompleted, true)}
	trigger := newTrigger(t, client)
	_, err := trigger.Inspect(context.Background(), validReference())
	if !errors.Is(err, submission.ErrProviderUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestInspectRejectsMalformedOversizedOrContradictoryFinalize(t *testing.T) {
	oversizedInput := json.RawMessage(`{"payload":"` + strings.Repeat("x", 1<<20) + `"}`)
	oversizedDetails := baseDetails(hatchettrigger.RunStatusQueued, false)
	oversizedDetails.Input = envelopeJSON(submission.TriggerRequest{
		RunID: validRequest().RunID,
		Input: oversizedInput,
	})
	noncanonicalWhitespace := baseDetails(hatchettrigger.RunStatusQueued, false)
	noncanonicalWhitespace.Input = providerEnvelopeJSON(
		validRequest().RunID,
		json.RawMessage(` { "task_description" : "change" } `),
	)
	reorderedNested := baseDetails(hatchettrigger.RunStatusQueued, false)
	reorderedNested.Input = providerEnvelopeJSON(
		validRequest().RunID,
		json.RawMessage(`{"z":2,"a":1}`),
	)
	duplicateNested := baseDetails(hatchettrigger.RunStatusQueued, false)
	duplicateNested.Input = providerEnvelopeJSON(
		validRequest().RunID,
		json.RawMessage(`{"task_description":"super-secret","task_description":"change"}`),
	)
	tests := map[string]hatchettrigger.Details{
		"malformed output":               withFinalize(baseDetails(hatchettrigger.RunStatusCompleted, true), hatchettrigger.RunStatusCompleted, json.RawMessage(`{"run_id":`)),
		"oversized output":               withFinalize(baseDetails(hatchettrigger.RunStatusCompleted, true), hatchettrigger.RunStatusCompleted, json.RawMessage(`"`+strings.Repeat("x", (1<<20)+1)+`"`)),
		"unknown output field":           withFinalize(baseDetails(hatchettrigger.RunStatusCompleted, true), hatchettrigger.RunStatusCompleted, json.RawMessage(`{"run_id":"paje_bound","status":"succeeded","token":"secret"}`)),
		"wrong output run":               withFinalize(baseDetails(hatchettrigger.RunStatusCompleted, true), hatchettrigger.RunStatusCompleted, resultJSON("paje_other", run.StatusSucceeded)),
		"nonterminal output":             withFinalize(baseDetails(hatchettrigger.RunStatusCompleted, true), hatchettrigger.RunStatusCompleted, resultJSON(validRequest().RunID, run.StatusExecuting)),
		"incomplete finalize task":       withFinalize(baseDetails(hatchettrigger.RunStatusCompleted, true), hatchettrigger.RunStatusRunning, resultJSON(validRequest().RunID, run.StatusSucceeded)),
		"finalize on running workflow":   withFinalize(baseDetails(hatchettrigger.RunStatusRunning, false), hatchettrigger.RunStatusCompleted, resultJSON(validRequest().RunID, run.StatusSucceeded)),
		"completed workflow not done":    baseDetails(hatchettrigger.RunStatusCompleted, false),
		"running workflow already done":  baseDetails(hatchettrigger.RunStatusRunning, true),
		"unknown workflow status":        baseDetails(hatchettrigger.RunStatus("MAGIC"), false),
		"oversized nested input":         oversizedDetails,
		"noncanonical nested whitespace": noncanonicalWhitespace,
		"reordered nested keys":          reorderedNested,
		"duplicate nested key":           duplicateNested,
	}
	for name, details := range tests {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{details: details}
			trigger := newTrigger(t, client)
			_, err := trigger.Inspect(context.Background(), validReference())
			if !errors.Is(err, submission.ErrProviderUnavailable) ||
				strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "token") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCancelTargetsStoredExternalUUID(t *testing.T) {
	client := &fakeClient{}
	trigger := newTrigger(t, client)
	if err := trigger.Cancel(context.Background(), validReference()); err != nil {
		t.Fatal(err)
	}
	if client.cancelCalls != 1 || len(client.cancelIDs) != 1 || client.cancelIDs[0] != externalRunID || client.detailsCalls != 0 {
		t.Fatalf("cancel IDs = %#v details calls = %d", client.cancelIDs, client.detailsCalls)
	}
}

func TestCancelRejectsForeignProvider(t *testing.T) {
	client := &fakeClient{}
	trigger := newTrigger(t, client)
	err := trigger.Cancel(context.Background(), submission.TriggerReference{Provider: "other", ExternalRunID: externalRunID})
	if !errors.Is(err, submission.ErrRunNotCancelable) || client.cancelCalls != 0 {
		t.Fatalf("error = %v cancel calls = %d", err, client.cancelCalls)
	}
}

func TestCancelRejectsMalformedExternalID(t *testing.T) {
	client := &fakeClient{}
	trigger := newTrigger(t, client)
	err := trigger.Cancel(context.Background(), submission.TriggerReference{Provider: "hatchet", ExternalRunID: "../../arbitrary"})
	if !errors.Is(err, submission.ErrRunNotCancelable) || client.cancelCalls != 0 {
		t.Fatalf("error = %v cancel calls = %d", err, client.cancelCalls)
	}
}

func TestCancelReconcilesAlreadyTerminal(t *testing.T) {
	client := &fakeClient{
		cancelErr: errors.New("body=already terminal token=super-secret"),
		details:   baseDetails(hatchettrigger.RunStatusCanceled, true),
	}
	trigger := newTrigger(t, client)
	if err := trigger.Cancel(context.Background(), validReference()); err != nil {
		t.Fatal(err)
	}
	if client.cancelCalls != 1 || client.detailsCalls != 1 {
		t.Fatalf("calls cancel=%d details=%d", client.cancelCalls, client.detailsCalls)
	}
}

func TestCancelReturnsSafeProviderErrorForActiveRun(t *testing.T) {
	client := &fakeClient{
		cancelErr: errors.New("body=not cancelable token=super-secret"),
		details:   baseDetails(hatchettrigger.RunStatusRunning, false),
	}
	trigger := newTrigger(t, client)
	err := trigger.Cancel(context.Background(), validReference())
	assertSafeProviderError(t, err)
	if client.cancelCalls != 1 || client.detailsCalls != 1 {
		t.Fatalf("calls cancel=%d details=%d", client.cancelCalls, client.detailsCalls)
	}
}

func TestReferenceValidationRejectsWrongProviderOnInspect(t *testing.T) {
	client := &fakeClient{}
	trigger := newTrigger(t, client)
	_, err := trigger.Inspect(context.Background(), submission.TriggerReference{
		Provider: "other", ExternalRunID: externalRunID,
	})
	if !errors.Is(err, submission.ErrProviderUnavailable) || client.detailsCalls != 0 {
		t.Fatalf("error = %v details calls = %d", err, client.detailsCalls)
	}
}

func TestContextCancellationIdentityIsPreserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &fakeClient{startErr: context.Canceled, detailsErr: context.Canceled, cancelErr: context.Canceled}
	trigger := newTrigger(t, client)

	if _, err := trigger.Start(ctx, validRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v", err)
	}
	if _, err := trigger.Inspect(ctx, validReference()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect error = %v", err)
	}
	if err := trigger.Cancel(ctx, validReference()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Cancel error = %v", err)
	}
}

func newTrigger(t *testing.T, client hatchettrigger.Client) *hatchettrigger.Trigger {
	t.Helper()
	trigger, err := hatchettrigger.New(client)
	if err != nil {
		t.Fatal(err)
	}
	return trigger
}

func validRequest() submission.TriggerRequest {
	return submission.TriggerRequest{
		RunID: "paje_bound",
		Input: json.RawMessage(`{"task_description":"change"}`),
	}
}

func validReference() submission.TriggerReference {
	return submission.TriggerReference{Provider: "hatchet", ExternalRunID: externalRunID}
}

func collisionClient(binding submission.TriggerRequest) *fakeClient {
	return &fakeClient{
		startErr: &hatchettrigger.IdempotencyCollisionError{ExternalRunID: externalRunID},
		details: hatchettrigger.Details{
			ExternalRunID: externalRunID,
			Status:        hatchettrigger.RunStatusQueued,
			Input:         envelopeJSON(binding),
		},
	}
}

func inspectState(t *testing.T, details hatchettrigger.Details) submission.TriggerState {
	t.Helper()
	client := &fakeClient{details: details}
	trigger := newTrigger(t, client)
	state, err := trigger.Inspect(context.Background(), validReference())
	if err != nil {
		t.Fatal(err)
	}
	if client.detailsCalls != 1 || client.detailsIDs[0] != externalRunID {
		t.Fatalf("details calls = %#v", client.detailsIDs)
	}
	return state
}

func baseDetails(status hatchettrigger.RunStatus, done bool) hatchettrigger.Details {
	return hatchettrigger.Details{
		ExternalRunID: externalRunID,
		Status:        status,
		Done:          done,
		Input:         envelopeJSON(validRequest()),
	}
}

func completedDetails(status run.Status) hatchettrigger.Details {
	return withFinalize(
		baseDetails(hatchettrigger.RunStatusCompleted, true),
		hatchettrigger.RunStatusCompleted,
		resultJSON(validRequest().RunID, status),
	)
}

func withFinalize(details hatchettrigger.Details, status hatchettrigger.RunStatus, output json.RawMessage) hatchettrigger.Details {
	details.Finalize = &hatchettrigger.TaskDetails{Status: status, Output: output}
	return details
}

func envelopeJSON(request submission.TriggerRequest) json.RawMessage {
	encoded, err := json.Marshal(map[string]any{
		"run_id": request.RunID,
		"input":  json.RawMessage(request.Input),
	})
	if err != nil {
		panic(err)
	}
	return encoded
}

func providerEnvelopeJSON(runID string, input json.RawMessage) json.RawMessage {
	encodedRunID, err := json.Marshal(runID)
	if err != nil {
		panic(err)
	}
	return json.RawMessage(
		`{"run_id":` + string(encodedRunID) + `,"input":` + string(input) + `}`,
	)
}

func resultJSON(runID string, status run.Status) json.RawMessage {
	encoded, err := json.Marshal(map[string]any{"run_id": runID, "status": status})
	if err != nil {
		panic(err)
	}
	return encoded
}

func assertSafeProviderError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, submission.ErrProviderUnavailable) {
		t.Fatalf("error = %v", err)
	}
	for _, secret := range []string{"super-secret", "provider-internal", "Authorization", "Bearer", "body="} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe provider error = %q", err)
		}
	}
}

var _ hatchettrigger.Client = (*fakeClient)(nil)
