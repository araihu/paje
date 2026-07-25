// Package template defines typed, versioned workflow templates.
package template

import (
	"encoding/json"
	"fmt"
)

// ID identifies one immutable template version.
type ID struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// String renders the stable external template identifier.
func (id ID) String() string {
	return fmt.Sprintf("%s@v%d", id.Name, id.Version)
}

// Template validates the wire input accepted by a registered template.
type Template interface {
	ID() ID
	Validate(json.RawMessage) error
}
