package harness

import (
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/workerprofile"
)

var (
	harnessIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	harnessVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?$`)
)

type Registry struct {
	adapters map[string]Adapter
}

// ResolvedAgent binds an adapter and execution authority to one exact
// canonical worker profile. Its fields and authority context are not
// constructible outside this package.
type ResolvedAgent struct {
	adapter Adapter
	profile workerprofile.Snapshot
	context AgentExecutionContext
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	registry := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if nilAdapter(adapter) {
			return nil, errors.New("harness registration is nil")
		}
		id, version := adapter.ID(), adapter.Version()
		if !harnessIDPattern.MatchString(id) || !harnessVersionPattern.MatchString(version) {
			return nil, errors.New("harness registration identity is invalid")
		}
		key := id + "@" + version
		if _, duplicate := registry.adapters[key]; duplicate {
			return nil, errors.New("duplicate exact harness registration")
		}
		registry.adapters[key] = adapter
	}
	return registry, nil
}

func (registry *Registry) Resolve(profile workerprofile.Snapshot) (Adapter, error) {
	if registry == nil {
		return nil, errors.New("harness registry is nil")
	}
	canonical, err := workerprofile.Canonicalize(profile)
	if err != nil || profile.Digest == "" || !reflect.DeepEqual(canonical, profile) {
		return nil, errors.New("harness profile snapshot is not exact and canonical")
	}
	adapter, ok := registry.adapters[profile.Harness.ID+"@"+profile.Harness.Version]
	if !ok {
		return nil, errors.New("exact harness is not registered")
	}
	if adapter.ID() != profile.Harness.ID || adapter.Version() != profile.Harness.Version {
		return nil, errors.New("registered harness identity was rebound")
	}
	for _, requirement := range profile.Secrets {
		if strings.HasPrefix(requirement.Capability, "harness.") && !adapter.AcceptsCapability(requirement.Capability) {
			return nil, errors.New("harness capability is not recognized by exact adapter")
		}
	}
	return adapter, nil
}

// ResolveAgent binds one exact adapter to an independent copy of the
// persisted secret requirements and returns an independent non-secret
// environment declaration for the agent child.
func (registry *Registry) ResolveAgent(profile workerprofile.Snapshot) (*ResolvedAgent, map[string]string, error) {
	adapter, err := registry.Resolve(profile)
	if err != nil {
		return nil, nil, err
	}
	environment, err := adapter.AgentEnvironment(slices.Clone(profile.Secrets))
	if err != nil {
		return nil, nil, err
	}
	switch profile.Runtime.Kind {
	case workerprofile.RuntimeHost:
	case workerprofile.RuntimeOCI:
	default:
		return nil, nil, errors.New("harness execution authority is unsupported")
	}
	resolved := &ResolvedAgent{
		adapter: adapter,
		profile: profile.Clone(),
		context: AgentExecutionContext{
			runtimeKind: profile.Runtime.Kind, profileDigest: profile.Digest,
			harnessID: profile.Harness.ID, version: profile.Harness.Version,
		},
	}
	return resolved, cloneEnvironment(environment), nil
}

func (resolved *ResolvedAgent) ValidateProfile(profile workerprofile.Snapshot) error {
	if resolved == nil || nilAdapter(resolved.adapter) {
		return errors.New("resolved harness is nil")
	}
	canonical, err := workerprofile.Canonicalize(profile)
	if err != nil || profile.Digest == "" || !reflect.DeepEqual(canonical, profile) ||
		!reflect.DeepEqual(resolved.profile, profile) ||
		resolved.context.profileDigest != profile.Digest {
		return errors.New("resolved harness profile was rebound")
	}
	if _, err := resolved.context.AuthorityFor(resolved.adapter.ID(), resolved.adapter.Version()); err != nil {
		return err
	}
	return nil
}

func (resolved *ResolvedAgent) ID() string              { return resolved.adapter.ID() }
func (resolved *ResolvedAgent) Version() string         { return resolved.adapter.Version() }
func (resolved *ResolvedAgent) Probe() executor.Command { return resolved.adapter.Probe() }

func (resolved *ResolvedAgent) AgentCommand(profile workerprofile.Snapshot, prompt string) (executor.Command, error) {
	if err := resolved.ValidateProfile(profile); err != nil {
		return executor.Command{}, err
	}
	return resolved.adapter.AgentCommandFor(resolved.context, prompt)
}

func (resolved *ResolvedAgent) Parse(result executor.Result) (string, error) {
	return resolved.adapter.Parse(result)
}

func cloneEnvironment(environment map[string]string) map[string]string {
	if environment == nil {
		return nil
	}
	clone := make(map[string]string, len(environment))
	for key, value := range environment {
		clone[key] = value
	}
	return clone
}

func nilAdapter(adapter Adapter) bool {
	if adapter == nil {
		return true
	}
	value := reflect.ValueOf(adapter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
