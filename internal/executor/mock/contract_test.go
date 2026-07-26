package mock

import (
	"context"
	"errors"
	"testing"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/executor/contracttest"
)

func TestExecutorContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, scenario contracttest.Scenario) contracttest.Fixture {
		t.Helper()
		target := New()
		request := validRequest(t)
		fixture := contracttest.Fixture{Executor: target, Request: request}
		switch scenario {
		case contracttest.ScenarioComplete:
			target.SetResult(request.Attempt, executor.Result{
				Created: true, Started: true, Completed: true, Stdout: []byte("ok"),
			}, nil)
		case contracttest.ScenarioStartFailure:
			target.SetResult(request.Attempt, executor.Result{}, executor.WrapError("environment", "start", errors.New("provider-detail")))
		case contracttest.ScenarioNonzero:
			target.SetResult(request.Attempt, executor.Result{
				Created: true, Started: true, Completed: true, ExitCode: 7,
			}, nil)
		case contracttest.ScenarioTimeout:
			target.SetResult(request.Attempt, executor.Result{
				Created: true, Started: true,
			}, executor.WrapError("environment", "timeout", errors.New("provider-detail")))
		case contracttest.ScenarioCancellation, contracttest.ScenarioDescendantDeath:
			started := make(chan struct{})
			fixture.Started = started
			fixture.AssertNoDescendants = func(*testing.T) {}
			target.SetBeforeExecute(func(ctx context.Context, _ executor.Request) {
				close(started)
				<-ctx.Done()
			})
			target.SetResult(request.Attempt, executor.Result{
				Created: true, Started: true,
			}, executor.WrapError("canceled", "caller_canceled", errors.New("provider-detail")))
		case contracttest.ScenarioBoundedOutput:
			target.SetResult(request.Attempt, executor.Result{
				Created: true, Started: true, Completed: true,
				Stdout: make([]byte, request.OutputLimit), Stderr: make([]byte, request.OutputLimit),
				StdoutTruncated: true, StderrTruncated: true,
			}, nil)
		case contracttest.ScenarioSecretIsolation:
			fixture.Unsupported = "mock fixture has no secret-bearing profile"
		}
		return fixture
	})
}
