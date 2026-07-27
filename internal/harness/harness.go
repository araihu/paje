// Package harness defines exact, provider-neutral agent harness protocols.
package harness

import (
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/workerprofile"
)

type Adapter interface {
	ID() string
	Version() string
	Probe() executor.Command
	AgentCommand(prompt string) (executor.Command, error)
	AgentEnvironment([]workerprofile.SecretRequirement) (map[string]string, error)
	Parse(executor.Result) (string, error)
	AcceptsCapability(string) bool
}
