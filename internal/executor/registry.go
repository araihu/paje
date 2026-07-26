package executor

import (
	"errors"
	"reflect"

	"github.com/araihu/paje/internal/workerprofile"
)

type Registration struct {
	RuntimeKind string
	Executor    Executor
}

// ProfileValidator lets an adapter deny a canonical profile it cannot enforce
// without adding provider-specific capability types to the executor port.
type ProfileValidator interface {
	ValidateProfile(workerprofile.Snapshot) error
}

type Registry struct {
	executors map[string]Executor
}

func NewRegistry(registrations ...Registration) (*Registry, error) {
	registry := &Registry{executors: make(map[string]Executor, len(registrations))}
	for _, registration := range registrations {
		if registration.RuntimeKind != workerprofile.RuntimeHost && registration.RuntimeKind != workerprofile.RuntimeOCI {
			return nil, errors.New("executor runtime registration is unsupported")
		}
		if nilExecutor(registration.Executor) {
			return nil, errors.New("executor registration is nil")
		}
		if _, duplicate := registry.executors[registration.RuntimeKind]; duplicate {
			return nil, errors.New("duplicate executor runtime registration")
		}
		registry.executors[registration.RuntimeKind] = registration.Executor
	}
	return registry, nil
}

func (registry *Registry) Resolve(profile workerprofile.Snapshot) (Executor, error) {
	if registry == nil {
		return nil, errors.New("executor registry is nil")
	}
	canonical, err := workerprofile.Canonicalize(profile)
	if err != nil || profile.Digest == "" || !reflect.DeepEqual(canonical, profile) {
		return nil, errors.New("executor profile snapshot is not exact and canonical")
	}
	target, ok := registry.executors[profile.Runtime.Kind]
	if !ok {
		return nil, errors.New("executor runtime is not registered")
	}
	if validator, ok := target.(ProfileValidator); ok {
		if err := validator.ValidateProfile(profile.Clone()); err != nil {
			return nil, err
		}
	}
	return target, nil
}

func nilExecutor(target Executor) bool {
	if target == nil {
		return true
	}
	value := reflect.ValueOf(target)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
