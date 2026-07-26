package agentharness

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sync"
)

var harnessIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type Registry struct {
	mu        sync.RWMutex
	harnesses map[string]AgentHarness
}

func NewRegistry(harnesses map[string]AgentHarness) (*Registry, error) {
	registry := &Registry{harnesses: make(map[string]AgentHarness, len(harnesses))}
	for id, harness := range harnesses {
		if !harnessIDPattern.MatchString(id) || nilHarness(harness) {
			return nil, fmt.Errorf("%w: invalid harness registration", ErrInvalidRequest)
		}
		registry.harnesses[id] = harness
	}
	return registry, nil
}

func (r *Registry) Resolve(id string) (AgentHarness, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil registry", ErrInvalidRequest)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	harness, ok := r.harnesses[id]
	if !ok {
		return nil, fmt.Errorf("%w: exact harness %q is not registered", ErrProviderUnavailable, id)
	}
	return harness, nil
}

func (r *Registry) Capabilities(
	ctx context.Context,
	principal Principal,
	harnessID string,
	primitive string,
) (HarnessCapabilities, error) {
	harness, err := r.Resolve(harnessID)
	if err != nil {
		return HarnessCapabilities{}, err
	}
	capabilities, err := harness.Capabilities(ctx, principal, primitive)
	if err != nil {
		return HarnessCapabilities{}, err
	}
	if capabilities.HarnessID != harnessID {
		return HarnessCapabilities{}, fmt.Errorf("%w: discovered harness identity mismatch", ErrInvalidCapabilities)
	}
	if err := capabilities.Validate(); err != nil {
		return HarnessCapabilities{}, err
	}
	return capabilities, nil
}

func nilHarness(harness AgentHarness) bool {
	if harness == nil {
		return true
	}
	value := reflect.ValueOf(harness)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
