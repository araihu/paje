package mock_test

import (
	"testing"

	"github.com/araihu/paje/internal/agentharness/contracttest"
	"github.com/araihu/paje/internal/agentharness/mock"
)

func TestHarnessConformance(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, scenario contracttest.Scenario) contracttest.Fixture {
		t.Helper()
		return mock.ContractFixture(scenario)
	})
}
