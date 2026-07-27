// Package harness defines exact, provider-neutral agent harness protocols.
package harness

import (
	"errors"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/workerprofile"
)

// SandboxAuthority names the layer responsible for constraining one agent
// command. Values are issued only by Registry after exact profile validation.
type SandboxAuthority uint8

const (
	SandboxAuthorityUnknown SandboxAuthority = iota
	SandboxAuthorityHost
	SandboxAuthorityExternalOCI
)

// AgentExecutionContext is an opaque, profile-bound authority grant. A zero
// value is invalid; callers cannot construct a bypass-capable context.
type AgentExecutionContext struct {
	runtimeKind   string
	profileDigest string
	harnessID     string
	version       string
}

// AuthorityFor returns the bound authority only to the exact registered
// adapter identity. Unknown, zero, or rebound contexts fail closed.
func (context AgentExecutionContext) AuthorityFor(id, version string) (SandboxAuthority, error) {
	if context.profileDigest == "" || context.harnessID == "" || context.version == "" ||
		context.harnessID != id || context.version != version {
		return SandboxAuthorityUnknown, errors.New("agent execution authority is unknown or rebound")
	}
	switch context.runtimeKind {
	case workerprofile.RuntimeHost:
		return SandboxAuthorityHost, nil
	case workerprofile.RuntimeOCI:
		return SandboxAuthorityExternalOCI, nil
	default:
		return SandboxAuthorityUnknown, errors.New("agent execution runtime is unknown")
	}
}

type Adapter interface {
	ID() string
	Version() string
	Probe() executor.Command
	// AgentCommand is the host-safe compatibility path. It never grants an
	// externally sandboxed bypass.
	AgentCommand(prompt string) (executor.Command, error)
	// AgentCommandFor is used only through a Registry-bound ResolvedAgent.
	AgentCommandFor(AgentExecutionContext, string) (executor.Command, error)
	AgentEnvironment([]workerprofile.SecretRequirement) (map[string]string, error)
	Parse(executor.Result) (string, error)
	AcceptsCapability(string) bool
}
