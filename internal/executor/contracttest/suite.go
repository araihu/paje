// Package contracttest defines the reusable executor lifecycle and security
// conformance suite. Adapter packages call Run from their own tests with real
// provider fixtures and explicitly name unsupported provider-specific cases.
package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
)

type Scenario string

const (
	ScenarioComplete        Scenario = "create_start_complete"
	ScenarioStartFailure    Scenario = "start_failure"
	ScenarioNonzero         Scenario = "nonzero_exit"
	ScenarioTimeout         Scenario = "timeout"
	ScenarioCancellation    Scenario = "cancellation"
	ScenarioDescendantDeath Scenario = "descendant_death"
	ScenarioBoundedOutput   Scenario = "bounded_output"
	ScenarioWorkspace       Scenario = "workspace_isolation"
	ScenarioSecretIsolation Scenario = "secret_stage_isolation"
	ScenarioCanonicalPath   Scenario = "canonical_path"
)

type Fixture struct {
	Executor executor.Executor
	Request  executor.Request
	// ReceiptEnvironment and ReceiptEnvironmentFiles describe the exact child
	// declaration when an adapter applies a documented canonical transform
	// before exec (for example, the OCI sandbox PATH).
	ReceiptEnvironment      map[string]string
	ReceiptEnvironmentFiles map[string]string

	// Started is closed by a fixture when a cancellable process and its
	// descendants are running. It is required by cancellation scenarios.
	Started <-chan struct{}
	// AssertNoDescendants proves cancellation left no attempt descendant alive.
	AssertNoDescendants func(*testing.T)
	// Unsupported must explain a provider capability this adapter cannot offer.
	Unsupported string
}

type Factory func(*testing.T, Scenario) Fixture

func Run(t *testing.T, factory Factory) {
	t.Helper()
	if factory == nil {
		t.Fatal("executor contract factory is required")
	}
	scenarios := []Scenario{
		ScenarioComplete,
		ScenarioStartFailure,
		ScenarioNonzero,
		ScenarioTimeout,
		ScenarioCancellation,
		ScenarioDescendantDeath,
		ScenarioBoundedOutput,
		ScenarioWorkspace,
		ScenarioSecretIsolation,
		ScenarioCanonicalPath,
	}
	for _, scenario := range scenarios {
		t.Run(string(scenario), func(t *testing.T) {
			fixture := factory(t, scenario)
			request := fixture.Request
			t.Cleanup(request.Destroy)
			if fixture.Unsupported != "" {
				t.Skip(fixture.Unsupported)
			}
			if fixture.Executor == nil {
				t.Fatal("executor contract fixture has nil executor")
			}
			if err := request.Validate(); err != nil && scenario != ScenarioWorkspace &&
				scenario != ScenarioSecretIsolation && scenario != ScenarioCanonicalPath {
				t.Fatalf("executor contract fixture request is invalid: %v", err)
			}
			switch scenario {
			case ScenarioComplete:
				runComplete(t, fixture, request)
			case ScenarioStartFailure:
				runStartFailure(t, fixture.Executor, request)
			case ScenarioNonzero:
				runNonzero(t, fixture, request)
			case ScenarioTimeout:
				runTimeout(t, fixture, request)
			case ScenarioCancellation, ScenarioDescendantDeath:
				runCancellation(t, fixture, scenario == ScenarioDescendantDeath)
			case ScenarioBoundedOutput:
				runBoundedOutput(t, fixture, request)
			case ScenarioWorkspace:
				runWorkspaceIsolation(t, fixture.Executor, request)
			case ScenarioSecretIsolation:
				runSecretIsolation(t, fixture.Executor, request)
			case ScenarioCanonicalPath:
				runCanonicalPath(t, fixture.Executor, request)
			}
		})
	}
}

func runCanonicalPath(t *testing.T, target executor.Executor, request executor.Request) {
	t.Helper()
	request.Environment = cloneEnvironment(request.Environment)
	request.Environment["PATH"] = "/tmp/paje-untrusted-bin"
	if err := request.ValidateDeclaration(); err == nil {
		t.Fatal("ValidateDeclaration accepted noncanonical PATH")
	}
	result, err := target.Execute(context.Background(), request)
	if err == nil || result.Created || result.BootstrapStarted || result.Started {
		t.Fatalf("noncanonical PATH reached executor side effects: %#v, %v", result, err)
	}
}

func cloneEnvironment(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values)+1)
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func runComplete(t *testing.T, fixture Fixture, request executor.Request) {
	t.Helper()
	target := fixture.Executor
	result, err := target.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || !result.Started || !result.Completed || result.ExitCode != 0 {
		t.Fatalf("successful lifecycle evidence = %#v", result)
	}
	assertBoundChildStart(t, fixture, request, result)
	for range 2 {
		state, inspectErr := target.Inspect(context.Background(), request.Attempt)
		if inspectErr != nil || state != executor.StateCompleted {
			t.Fatalf("Inspect() = %q, %v", state, inspectErr)
		}
	}
	if _, err := target.Execute(context.Background(), request); !errors.Is(err, executor.ErrAttemptExists) {
		t.Fatalf("identity collision error = %v", err)
	}
	for range 2 {
		if err := target.Cancel(context.Background(), request.Attempt); err != nil {
			t.Fatalf("idempotent Cancel() error = %v", err)
		}
	}
	for range 2 {
		if err := target.Destroy(context.Background(), request.Attempt); err != nil {
			t.Fatalf("idempotent Destroy() error = %v", err)
		}
	}
	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateDestroyed {
		t.Fatalf("Inspect() after Destroy = %q, %v", state, err)
	}
}

func runStartFailure(t *testing.T, target executor.Executor, request executor.Request) {
	t.Helper()
	result, err := target.Execute(context.Background(), request)
	if err == nil || result.Started || result.Completed {
		t.Fatalf("start failure lifecycle = %#v, %v", result, err)
	}
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("marshal safe start error: %v", marshalErr)
	}
	if strings.Contains(string(encoded), "provider-detail") {
		t.Fatalf("safe diagnostics leaked provider detail: %s", encoded)
	}
}

func runNonzero(t *testing.T, fixture Fixture, request executor.Request) {
	t.Helper()
	target := fixture.Executor
	result, err := target.Execute(context.Background(), request)
	if err != nil || !result.Created || !result.Started || !result.Completed || result.ExitCode == 0 {
		t.Fatalf("nonzero lifecycle = %#v, %v", result, err)
	}
	assertBoundChildStart(t, fixture, request, result)
}

func runTimeout(t *testing.T, fixture Fixture, request executor.Request) {
	t.Helper()
	target := fixture.Executor
	result, err := target.Execute(context.Background(), request)
	var providerError *executor.ProviderError
	if !result.Started || err == nil || !errors.As(err, &providerError) ||
		providerError.Class != "environment" || providerError.CauseCode != "timeout" {
		t.Fatalf("timeout lifecycle = %#v, %v", result, err)
	}
	assertBoundChildStart(t, fixture, request, result)
}

func runCancellation(t *testing.T, fixture Fixture, descendants bool) {
	t.Helper()
	if fixture.Started == nil {
		t.Fatal("cancellation fixture must signal process start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	type response struct {
		result executor.Result
		err    error
	}
	finished := make(chan response, 1)
	go func() {
		result, err := fixture.Executor.Execute(ctx, fixture.Request)
		finished <- response{result: result, err: err}
	}()
	select {
	case <-fixture.Started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("cancellation fixture did not start")
	}
	cancel()
	for range 2 {
		if err := fixture.Executor.Cancel(context.Background(), fixture.Request.Attempt); err != nil {
			t.Fatalf("Cancel() error = %v", err)
		}
	}
	select {
	case got := <-finished:
		if !got.result.Started || got.err == nil {
			t.Fatalf("canceled lifecycle = %#v, %v", got.result, got.err)
		}
		assertBoundChildStart(t, fixture, fixture.Request, got.result)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Execute did not return")
	}
	if descendants {
		if fixture.AssertNoDescendants == nil {
			t.Fatal("descendant fixture requires a termination assertion")
		}
		fixture.AssertNoDescendants(t)
	}
}

func runBoundedOutput(t *testing.T, fixture Fixture, request executor.Request) {
	t.Helper()
	target := fixture.Executor
	result, err := target.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(result.Stdout)) > request.OutputLimit || int64(len(result.Stderr)) > request.OutputLimit ||
		!result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("bounded output evidence = stdout %d stderr %d %#v", len(result.Stdout), len(result.Stderr), result)
	}
	assertBoundChildStart(t, fixture, request, result)
}

func assertBoundChildStart(t *testing.T, fixture Fixture, request executor.Request, result executor.Result) {
	t.Helper()
	if result.ChildStartReceipt == nil {
		t.Fatal("conclusive Started result has no child-start receipt")
	}
	environment := fixture.ReceiptEnvironment
	if environment == nil {
		environment = request.Environment
	}
	expected, err := executor.NewChildStartReceipt(
		request.Attempt,
		request.Command,
		environment,
		fixture.ReceiptEnvironmentFiles,
		result.ChildStartReceipt.Challenge,
	)
	if err != nil {
		t.Fatalf("construct expected child-start receipt: %v", err)
	}
	if !result.ChildStartReceipt.Matches(expected) {
		t.Fatalf("child-start receipt is rebound: got %#v want %#v", result.ChildStartReceipt, expected)
	}
}

func runWorkspaceIsolation(t *testing.T, target executor.Executor, request executor.Request) {
	t.Helper()
	request.Command.Directory = "/outside"
	if _, err := target.Execute(context.Background(), request); err == nil {
		t.Fatal("workspace escape succeeded")
	}
}

func runSecretIsolation(t *testing.T, target executor.Executor, request executor.Request) {
	t.Helper()
	if len(request.Secrets) == 0 {
		t.Fatal("secret isolation fixture has no transient materialization")
	}
	request.Attempt.Purpose = executor.PurposeVerification
	if _, err := target.Execute(context.Background(), request); err == nil {
		t.Fatal("verification secret materialization succeeded")
	}
	request.Attempt.Purpose = executor.PurposeProbe
	if _, err := target.Execute(context.Background(), request); err == nil {
		t.Fatal("probe secret materialization succeeded")
	}
}
