package harness

import (
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/araihu/paje/internal/workerprofile"
)

var (
	harnessIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	harnessVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?$`)
)

type Registry struct {
	adapters map[string]Adapter
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
func (registry *Registry) ResolveAgent(profile workerprofile.Snapshot) (Adapter, map[string]string, error) {
	adapter, err := registry.Resolve(profile)
	if err != nil {
		return nil, nil, err
	}
	environment, err := adapter.AgentEnvironment(slices.Clone(profile.Secrets))
	if err != nil {
		return nil, nil, err
	}
	return adapter, cloneEnvironment(environment), nil
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
