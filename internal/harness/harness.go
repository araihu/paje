// Package harness defines exact, provider-neutral agent harness protocols.
package harness

import "github.com/araihu/paje/internal/executor"

type Adapter interface {
	ID() string
	Version() string
	Probe() executor.Command
	AgentCommand(prompt string) (executor.Command, error)
	Parse(executor.Result) (string, error)
	AcceptsCapability(string) bool
}
