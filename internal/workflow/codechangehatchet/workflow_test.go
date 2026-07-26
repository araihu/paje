package codechangehatchet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/run"
	codechange "github.com/araihu/paje/internal/template/codechange"
	workflow "github.com/araihu/paje/internal/workflow/codechange"
	"github.com/hatchet-dev/hatchet/pkg/client/create"
	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

func TestNewNamesWorkflow(t *testing.T) {
	factory := &recordingWorkflowFactory{declaration: &recordingDeclaration{}}
	buildWorkflow(factory, &fakeService{})
	if WorkflowName != "paje-code-change-v1" || factory.name != WorkflowName {
		t.Fatalf("workflow name = %q", factory.name)
	}
	if factory.options.idempotency == nil ||
		factory.options.idempotency.Expression != "input.run_id" ||
		factory.options.idempotency.Method != hatchet.IdempotencyMethodStatus ||
		factory.options.idempotency.TTL != workflowIdempotencyTTL {
		t.Fatalf("workflow idempotency = %#v", factory.options.idempotency)
	}
}

func TestNewProducesSDKDeclarationWithoutServer(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{
  "server_url":"http://127.0.0.1:1",
  "grpc_broadcast_address":"127.0.0.1:1",
  "exp":4102444800,
  "sub":"00000000-0000-0000-0000-000000000001"
}`))
	t.Setenv("HATCHET_CLIENT_TOKEN", "e30."+payload+".test-signature")
	t.Setenv("HATCHET_CLIENT_HOST_PORT", "127.0.0.1:1")
	t.Setenv("HATCHET_CLIENT_SERVER_URL", "http://127.0.0.1:1")
	t.Setenv("HATCHET_CLIENT_TLS_STRATEGY", "none")
	t.Setenv("HATCHET_CLIENT_NO_RETRY", "true")
	t.Setenv("HATCHET_CLIENT_LOG_LEVEL", "error")

	client, err := hatchet.NewClient()
	if err != nil {
		t.Fatalf("new test Hatchet client: %v", err)
	}
	got, err := New(client, &workflow.Service{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dump, handlers, durableHandlers, _ := got.Dump()
	if dump.GetName() != WorkflowName {
		t.Fatalf("workflow name = %q", dump.GetName())
	}
	if len(dump.GetTasks()) != 5 || len(handlers) != 4 || len(durableHandlers) != 1 {
		t.Fatalf("declaration sizes = tasks:%d handlers:%d durable:%d", len(dump.GetTasks()), len(handlers), len(durableHandlers))
	}
	idempotency := dump.GetIdempotency()
	if idempotency == nil || idempotency.GetExpression() != "input.run_id" ||
		idempotency.GetMethod().String() != "STATUS" ||
		idempotency.GetTtlMs() != workflowIdempotencyTTL.Milliseconds() {
		t.Fatalf("workflow idempotency = %v", idempotency)
	}
	wantTasks := map[string]struct {
		parents []string
		retries int32
		timeout string
		durable bool
	}{
		"resolve":  {parents: []string{}, retries: 2, timeout: "120s"},
		"execute":  {parents: []string{"resolve"}, retries: 2, timeout: "1800s"},
		"approval": {parents: []string{"execute"}, durable: true},
		"publish":  {parents: []string{"approval"}, retries: 2, timeout: "900s"},
		"finalize": {parents: []string{"publish"}, retries: 2, timeout: "120s"},
	}
	for _, task := range dump.GetTasks() {
		want, found := wantTasks[task.GetReadableId()]
		if !found {
			t.Fatalf("unexpected task %q", task.GetReadableId())
		}
		if !reflect.DeepEqual(task.GetParents(), want.parents) ||
			task.GetRetries() != want.retries || task.GetTimeout() != want.timeout ||
			task.GetIsDurable() != want.durable {
			t.Fatalf("task %q = parents:%v retries:%d timeout:%q durable:%t", task.GetReadableId(), task.GetParents(), task.GetRetries(), task.GetTimeout(), task.GetIsDurable())
		}
		if task.GetReadableId() != "approval" &&
			(task.GetBackoffFactor() != 2 || task.GetBackoffMaxSeconds() != 60) {
			t.Fatalf("task %q backoff = (%v, %d)", task.GetReadableId(), task.GetBackoffFactor(), task.GetBackoffMaxSeconds())
		}
		if task.GetReadableId() == "publish" {
			if len(task.GetConcurrency()) != 1 {
				t.Fatalf("publish concurrency count = %d", len(task.GetConcurrency()))
			}
			concurrency := task.GetConcurrency()[0]
			if concurrency.GetExpression() != "input.input.repository_uri + ':' + input.input.publication.target_branch" ||
				concurrency.GetMaxRuns() != 1 || concurrency.GetLimitStrategy().String() != "GROUP_ROUND_ROBIN" {
				t.Fatalf("publish concurrency = %v", concurrency)
			}
		}
		delete(wantTasks, task.GetReadableId())
	}
	if len(wantTasks) != 0 {
		t.Fatalf("missing tasks: %v", wantTasks)
	}
}

func TestDeclareWorkflowBuildsFivePhaseDAG(t *testing.T) {
	declaration := &recordingDeclaration{}
	declareWorkflow(declaration, &fakeService{})

	want := []recordedTask{
		{name: "resolve", retries: 2, backoffFactor: 2, maxBackoffSeconds: 60, timeout: 2 * time.Minute},
		{name: "execute", parents: []string{"resolve"}, retries: 2, backoffFactor: 2, maxBackoffSeconds: 60, timeout: 30 * time.Minute},
		{name: "approval", parents: []string{"execute"}, durable: true},
		{
			name: "publish", parents: []string{"approval"}, retries: 2,
			backoffFactor: 2, maxBackoffSeconds: 60, timeout: 15 * time.Minute,
			concurrency: &types.Concurrency{
				Expression: "input.input.repository_uri + ':' + input.input.publication.target_branch",
				MaxRuns:    pointer(int32(1)), LimitStrategy: pointer(types.GroupRoundRobin),
			},
		},
		{name: "finalize", parents: []string{"publish"}, retries: 2, backoffFactor: 2, maxBackoffSeconds: 60, timeout: 2 * time.Minute},
	}
	if !reflect.DeepEqual(declaration.tasks, want) {
		t.Fatalf("declaration mismatch\n got: %#v\nwant: %#v", declaration.tasks, want)
	}
}

func TestResolveHandlerUnwrapsExactTemplateInputWithPreallocatedRunID(t *testing.T) {
	strictError := errors.New("strict input rejected unknown field")
	service := &fakeService{
		resolve: func(_ context.Context, runID string, raw json.RawMessage) (workflow.PhaseResult, error) {
			if runID != "01J-HATCHET-OWNER" {
				t.Fatalf("run ID = %q", runID)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("unmarshal captured input: %v", err)
			}
			if string(fields["task_description"]) != `"change it"` {
				t.Fatalf("task_description = %s", fields["task_description"])
			}
			if string(fields["worker_profile"]) != `"codex-go@1"` {
				t.Fatalf("worker_profile = %s", fields["worker_profile"])
			}
			if string(fields["future_field"]) != `{"nested":true}` {
				t.Fatalf("future_field = %s", fields["future_field"])
			}
			return workflow.PhaseResult{}, strictError
		},
	}
	input := map[string]any{
		"run_id": "01J-HATCHET-OWNER",
		"input": map[string]any{
			"task_description": "change it",
			"repository_uri":   "https://example.test/repo.git",
			"worker_profile":   "codex-go@1",
			"future_field":     map[string]any{"nested": true},
		},
	}

	_, err := resolveHandler(service)(testTaskContext{}, input)
	if !errors.Is(err, strictError) {
		t.Fatalf("error = %v, want strict rejection", err)
	}
}

func TestResolveHandlerRejectsInvalidHatchetEnvelopeBeforeService(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
	}{
		{name: "missing run ID", input: map[string]any{"input": map[string]any{"task_description": "change"}}},
		{name: "blank run ID", input: map[string]any{"run_id": "  ", "input": map[string]any{"task_description": "change"}}},
		{name: "missing input", input: map[string]any{"run_id": "01J-HATCHET-OWNER"}},
		{name: "unknown envelope field", input: map[string]any{"run_id": "01J-HATCHET-OWNER", "input": map[string]any{}, "extra": true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeService{}
			if _, err := resolveHandler(service)(testTaskContext{}, test.input); err == nil {
				t.Fatal("resolve handler error = nil")
			}
			if len(service.calls) != 0 {
				t.Fatalf("invalid envelope reached service: %#v", service.calls)
			}
		})
	}
}

func TestDuplicateHatchetObserverCannotExhaustOwnerOnLastRetry(t *testing.T) {
	service := &fakeService{
		resolve: func(_ context.Context, _ string, _ json.RawMessage) (workflow.PhaseResult, error) {
			return workflow.PhaseResult{RunID: "run-owner", Status: run.StatusExecuting}, run.ErrIdempotencyConflict
		},
	}
	input := map[string]any{
		"run_id": "run-observer",
		"input":  map[string]any{"task_description": "same durable request", "worker_profile": "codex-go@1"},
	}

	result, err := resolveHandler(service)(testTaskContext{retryCount: resolveRetries}, input)
	if !errors.Is(err, run.ErrIdempotencyConflict) {
		t.Fatalf("observer error = %v, want %v", err, run.ErrIdempotencyConflict)
	}
	if result != (workflow.PhaseResult{}) {
		t.Fatalf("observer result = %#v, want no owner result exposed downstream", result)
	}
	if got, want := service.calls, []serviceCall{{method: "resolve", runID: "run-observer"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("observer calls = %#v, want %#v", got, want)
	}
}

func TestDuplicateHatchetObserverCannotInheritRetryableOwnerOnLastRetry(t *testing.T) {
	service := &fakeService{
		resolve: func(_ context.Context, _ string, _ json.RawMessage) (workflow.PhaseResult, error) {
			return workflow.PhaseResult{
				RunID: "run-owner", Status: run.StatusExecuting,
				Retryable: true, FailureClass: run.FailureEnvironment,
			}, run.ErrIdempotencyConflict
		},
	}
	input := map[string]any{
		"run_id": "run-observer",
		"input":  map[string]any{"task_description": "same durable request", "worker_profile": "codex-go@1"},
	}

	result, err := resolveHandler(service)(testTaskContext{retryCount: resolveRetries}, input)
	if !errors.Is(err, run.ErrIdempotencyConflict) {
		t.Fatalf("observer error = %v, want %v", err, run.ErrIdempotencyConflict)
	}
	if result != (workflow.PhaseResult{}) {
		t.Fatalf("observer result = %#v, want empty result", result)
	}
	if got, want := service.calls, []serviceCall{{method: "resolve", runID: "run-observer"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("observer calls = %#v, want no owner Exhaust call", got)
	}
}

func TestDuplicateHatchetObserverCannotInheritTerminalOwner(t *testing.T) {
	service := &fakeService{
		resolve: func(_ context.Context, _ string, _ json.RawMessage) (workflow.PhaseResult, error) {
			return workflow.PhaseResult{RunID: "run-owner", Status: run.StatusFailed}, run.ErrIdempotencyConflict
		},
	}
	input := map[string]any{
		"run_id": "run-observer",
		"input":  map[string]any{"task_description": "same durable request", "worker_profile": "codex-go@1"},
	}

	result, err := resolveHandler(service)(testTaskContext{}, input)
	if !errors.Is(err, run.ErrIdempotencyConflict) {
		t.Fatalf("observer error = %v, want %v", err, run.ErrIdempotencyConflict)
	}
	if result != (workflow.PhaseResult{}) {
		t.Fatalf("observer result = %#v, want no successful terminal parent result", result)
	}
	if got, want := service.calls, []serviceCall{{method: "resolve", runID: "run-observer"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("observer calls = %#v, want %#v", got, want)
	}
}

func TestDownstreamHandlersPassOnlyParentRunID(t *testing.T) {
	service := &fakeService{}
	ctx := testTaskContext{parent: workflow.PhaseResult{RunID: "run-123"}}

	if _, err := executeHandler(service)(ctx, map[string]any{"workspace": "/must/not/pass"}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := publishHandler(service)(ctx, map[string]any{"token": "must-not-pass"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := finalizeHandler(service)(ctx, map[string]any{"memory": "must-not-pass"}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	durableCtx := testDurableTaskContext{testTaskContext: ctx, waiter: &fakeEventWaiter{}}
	if _, err := approvalHandler(service)(durableCtx, map[string]any{"provider": "must-not-pass"}); err != nil {
		t.Fatalf("approval: %v", err)
	}

	if got, want := service.calls, []serviceCall{
		{method: "execute", runID: "run-123"},
		{method: "publish", runID: "run-123"},
		{method: "finalize", runID: "run-123"},
		{method: "approval", runID: "run-123"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestNonRetryablePhaseFailureLetsDownstreamTasksRun(t *testing.T) {
	phaseError := errors.New("terminal failure")
	want := workflow.PhaseResult{
		RunID: "run-123", Status: run.StatusFailed,
		FailureClass: run.FailureInput, Retryable: false,
	}
	service := &fakeService{execute: func(context.Context, string) (workflow.PhaseResult, error) {
		return want, phaseError
	}}

	got, err := executeHandler(service)(testTaskContext{
		parent: workflow.PhaseResult{RunID: "run-123"},
	}, nil)
	if err != nil {
		t.Fatalf("handler returned Hatchet error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
}

func TestRetryablePhaseFailureRequestsHatchetRetry(t *testing.T) {
	phaseError := errors.New("temporary failure")
	service := &fakeService{execute: func(context.Context, string) (workflow.PhaseResult, error) {
		return workflow.PhaseResult{RunID: "run-123", Retryable: true}, phaseError
	}}

	got, err := executeHandler(service)(testTaskContext{
		parent: workflow.PhaseResult{RunID: "run-123"}, retryCount: executeRetries - 1,
	}, nil)
	if !errors.Is(err, phaseError) {
		t.Fatalf("error = %v, want retryable phase error", err)
	}
	if got != (workflow.PhaseResult{}) {
		t.Fatalf("result = %#v, want empty retry result", got)
	}
	if len(service.calls) != 1 || service.calls[0].method != "execute" {
		t.Fatalf("calls = %#v, want execute only", service.calls)
	}
}

func TestLastRetryExhaustsPhaseAndReturnsSuccess(t *testing.T) {
	phaseError := errors.New("temporary failure")
	exhausted := workflow.PhaseResult{RunID: "run-123", Status: run.StatusFailed}
	service := &fakeService{
		execute: func(context.Context, string) (workflow.PhaseResult, error) {
			return workflow.PhaseResult{RunID: "run-123", Retryable: true}, phaseError
		},
		exhaust: func(_ context.Context, runID, stage string) (workflow.PhaseResult, error) {
			if runID != "run-123" || stage != "execute" {
				t.Fatalf("Exhaust(%q, %q)", runID, stage)
			}
			return exhausted, nil
		},
	}

	got, err := executeHandler(service)(testTaskContext{
		parent: workflow.PhaseResult{RunID: "run-123"}, retryCount: executeRetries,
	}, nil)
	if err != nil {
		t.Fatalf("handler returned Hatchet error: %v", err)
	}
	if got != exhausted {
		t.Fatalf("result = %#v, want %#v", got, exhausted)
	}
}

func TestFinalizeReturnsDurableCodeChangeResult(t *testing.T) {
	want := codechange.Result{RunID: "run-123", Status: run.StatusSucceeded}
	service := &fakeService{finalize: func(context.Context, string) (codechange.Result, error) {
		return want, nil
	}}

	got, err := finalizeHandler(service)(testTaskContext{
		parent: workflow.PhaseResult{RunID: "run-123"},
	}, nil)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
}

func TestFinalizeLastRetryExhaustsThenReturnsDurableFailure(t *testing.T) {
	finalizeCalls := 0
	want := codechange.Result{
		RunID: "run-123", Status: run.StatusFailed,
		Failure: &run.Failure{Stage: "finalize", Retryable: false},
	}
	service := &fakeService{
		finalize: func(context.Context, string) (codechange.Result, error) {
			finalizeCalls++
			if finalizeCalls == 1 {
				return codechange.Result{
					RunID: "run-123", Status: run.StatusPublishing,
					Failure: &run.Failure{Stage: "finalize", Retryable: true},
				}, errors.New("outcome memory temporarily unavailable")
			}
			return want, nil
		},
		exhaust: func(_ context.Context, runID, stage string) (workflow.PhaseResult, error) {
			if runID != "run-123" || stage != "finalize" {
				t.Fatalf("Exhaust(%q, %q)", runID, stage)
			}
			return workflow.PhaseResult{RunID: runID, Status: run.StatusFailed}, nil
		},
	}

	got, err := finalizeHandler(service)(testTaskContext{
		parent: workflow.PhaseResult{RunID: "run-123"}, retryCount: finalizeRetries,
	}, nil)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
	if gotCalls, wantCalls := service.calls, []serviceCall{
		{method: "finalize", runID: "run-123"},
		{method: "exhaust", runID: "run-123", stage: "finalize"},
		{method: "finalize", runID: "run-123"},
	}; !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", gotCalls, wantCalls)
	}
}

type recordedTask struct {
	name              string
	parents           []string
	durable           bool
	retries           int
	backoffFactor     float32
	maxBackoffSeconds int
	timeout           time.Duration
	concurrency       *types.Concurrency
}

type recordingDeclaration struct{ tasks []recordedTask }

type recordingWorkflowFactory struct {
	name        string
	options     workflowOptions
	declaration workflowDeclaration
}

func (f *recordingWorkflowFactory) newWorkflow(name string, options workflowOptions) workflowDeclaration {
	f.name = name
	f.options = options
	return f.declaration
}

func (d *recordingDeclaration) newTask(name string, _ any, options taskOptions) taskReference {
	d.tasks = append(d.tasks, recordedTask{
		name: name, parents: append([]string(nil), options.parents...),
		retries: options.retries, backoffFactor: options.retryBackoffFactor,
		maxBackoffSeconds: options.retryMaxBackoffSeconds,
		timeout:           options.executionTimeout, concurrency: cloneConcurrency(options.concurrency),
	})
	return namedTask(name)
}

func (d *recordingDeclaration) newDurableTask(name string, _ any, options taskOptions) taskReference {
	task := d.newTask(name, nil, options)
	d.tasks[len(d.tasks)-1].durable = true
	return task
}

func cloneConcurrency(value *types.Concurrency) *types.Concurrency {
	if value == nil {
		return nil
	}
	cloned := *value
	if value.MaxRuns != nil {
		cloned.MaxRuns = pointer(*value.MaxRuns)
	}
	if value.LimitStrategy != nil {
		cloned.LimitStrategy = pointer(*value.LimitStrategy)
	}
	return &cloned
}

func pointer[T any](value T) *T { return &value }

type serviceCall struct {
	method string
	runID  string
	stage  string
}

type fakeService struct {
	calls    []serviceCall
	resolve  func(context.Context, string, json.RawMessage) (workflow.PhaseResult, error)
	execute  func(context.Context, string) (workflow.PhaseResult, error)
	approval func(context.Context, string, approval.Gate) (workflow.PhaseResult, error)
	publish  func(context.Context, string) (workflow.PhaseResult, error)
	finalize func(context.Context, string) (codechange.Result, error)
	exhaust  func(context.Context, string, string) (workflow.PhaseResult, error)
}

func (s *fakeService) ResolveWithRunID(ctx context.Context, runID string, raw json.RawMessage) (workflow.PhaseResult, error) {
	s.calls = append(s.calls, serviceCall{method: "resolve", runID: runID})
	if s.resolve != nil {
		return s.resolve(ctx, runID, raw)
	}
	return workflow.PhaseResult{RunID: runID}, nil
}

func (s *fakeService) Execute(ctx context.Context, runID string) (workflow.PhaseResult, error) {
	s.calls = append(s.calls, serviceCall{method: "execute", runID: runID})
	if s.execute != nil {
		return s.execute(ctx, runID)
	}
	return workflow.PhaseResult{RunID: runID}, nil
}

func (s *fakeService) Approval(ctx context.Context, runID string, gate approval.Gate) (workflow.PhaseResult, error) {
	s.calls = append(s.calls, serviceCall{method: "approval", runID: runID})
	if s.approval != nil {
		return s.approval(ctx, runID, gate)
	}
	return workflow.PhaseResult{RunID: runID}, nil
}

func (s *fakeService) Publish(ctx context.Context, runID string) (workflow.PhaseResult, error) {
	s.calls = append(s.calls, serviceCall{method: "publish", runID: runID})
	if s.publish != nil {
		return s.publish(ctx, runID)
	}
	return workflow.PhaseResult{RunID: runID}, nil
}

func (s *fakeService) Finalize(ctx context.Context, runID string) (codechange.Result, error) {
	s.calls = append(s.calls, serviceCall{method: "finalize", runID: runID})
	if s.finalize != nil {
		return s.finalize(ctx, runID)
	}
	return codechange.Result{RunID: runID}, nil
}

func (s *fakeService) Exhaust(ctx context.Context, runID, stage string) (workflow.PhaseResult, error) {
	s.calls = append(s.calls, serviceCall{method: "exhaust", runID: runID, stage: stage})
	if s.exhaust != nil {
		return s.exhaust(ctx, runID, stage)
	}
	return workflow.PhaseResult{RunID: runID}, nil
}

type testTaskContext struct {
	context.Context
	parent     any
	parentErr  error
	retryCount int
}

func (c testTaskContext) ParentOutput(_ create.NamedTask, output any) error {
	if c.parentErr != nil {
		return c.parentErr
	}
	encoded, err := json.Marshal(c.parent)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, output)
}

func (c testTaskContext) RetryCount() int { return c.retryCount }

func (c testTaskContext) Deadline() (time.Time, bool) {
	if c.Context == nil {
		return time.Time{}, false
	}
	return c.Context.Deadline()
}

func (c testTaskContext) Done() <-chan struct{} {
	if c.Context == nil {
		return nil
	}
	return c.Context.Done()
}

func (c testTaskContext) Err() error {
	if c.Context == nil {
		return nil
	}
	return c.Context.Err()
}

func (c testTaskContext) Value(key any) any {
	if c.Context == nil {
		return nil
	}
	return c.Context.Value(key)
}

type testDurableTaskContext struct {
	testTaskContext
	waiter approvalEventWaiter
}

func (c testDurableTaskContext) approvalEventWaiter() approvalEventWaiter { return c.waiter }
