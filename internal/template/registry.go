package template

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ErrUnknownTemplate is returned when a template ID has not been registered.
var ErrUnknownTemplate = errors.New("unknown template")

// Registry resolves a fixed, immutable set of template definitions.
// It is safe for concurrent reads after construction.
type Registry struct {
	definitions map[ID]Template
}

// NewRegistry validates and registers each definition by its exact ID.
func NewRegistry(definitions ...Template) (*Registry, error) {
	registered := make(map[ID]Template, len(definitions))
	for _, definition := range definitions {
		if isNilTemplate(definition) {
			return nil, fmt.Errorf("create template registry: definition is required")
		}
		id := definition.ID()
		if strings.TrimSpace(id.Name) == "" {
			return nil, fmt.Errorf("create template registry: template name is required")
		}
		if id.Version <= 0 {
			return nil, fmt.Errorf("create template registry: template %q version must be positive", id.Name)
		}
		if _, exists := registered[id]; exists {
			return nil, fmt.Errorf("create template registry: duplicate template %s", id)
		}
		registered[id] = definition
	}
	return &Registry{definitions: registered}, nil
}

// Resolve returns the exact registered definition for id.
func (r *Registry) Resolve(id ID) (Template, error) {
	if r == nil {
		return nil, fmt.Errorf("resolve template %s: %w", id, ErrUnknownTemplate)
	}
	definition, ok := r.definitions[id]
	if !ok {
		return nil, fmt.Errorf("resolve template %s: %w", id, ErrUnknownTemplate)
	}
	return definition, nil
}

func isNilTemplate(definition Template) bool {
	if definition == nil {
		return true
	}
	// Templates are normally value definitions. A typed nil pointer stored in
	// the interface must still be rejected without invoking ID on it.
	v := reflect.ValueOf(definition)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
