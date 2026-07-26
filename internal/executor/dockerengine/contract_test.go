package dockerengine

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor/contracttest"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestExecutorContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, scenario contracttest.Scenario) contracttest.Fixture {
		t.Helper()
		api := newFakeEngine()
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		fixture := contracttest.Fixture{Executor: target, Request: request}
		switch scenario {
		case contracttest.ScenarioStartFailure:
			api.startErr = errors.New("provider-detail start failure")
		case contracttest.ScenarioNonzero:
			api.waitCode = 17
		case contracttest.ScenarioTimeout:
			api.blockWait = true
			request.Timeout = 20 * time.Millisecond
			fixture.Request = request
		case contracttest.ScenarioCancellation, contracttest.ScenarioDescendantDeath:
			api.blockWait = true
			fixture.Started = api.started
			fixture.AssertNoDescendants = func(t *testing.T) {
				t.Helper()
				api.mu.Lock()
				defer api.mu.Unlock()
				if api.containerState != engineContainerExited ||
					api.stopCalls == 0 && api.killCalls == 0 {
					t.Fatalf("descendant lifecycle = state %q stop %d kill %d",
						api.containerState, api.stopCalls, api.killCalls)
				}
			}
		case contracttest.ScenarioBoundedOutput:
			api.attached = multiplexedOutput(
				bytes.Repeat([]byte("o"), 1024),
				bytes.Repeat([]byte("e"), 1024),
			)
			request.OutputLimit = 64
			fixture.Request = request
		case contracttest.ScenarioSecretIsolation:
			fixture.Request = secretDockerRequest(t)
		}
		return fixture
	})
}
