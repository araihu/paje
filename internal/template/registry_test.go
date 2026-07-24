package template_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/araihu/paje/internal/template"
)

type stubTemplate struct {
	id template.ID
}

func (s stubTemplate) ID() template.ID { return s.id }

func (stubTemplate) Validate(json.RawMessage) error { return nil }

func TestRegistryResolvesExactVersion(t *testing.T) {
	v1 := stubTemplate{id: template.ID{Name: "code-change", Version: 1}}
	registry, err := template.NewRegistry(v1)
	if err != nil {
		t.Fatal(err)
	}

	got, err := registry.Resolve(v1.id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != v1.id {
		t.Fatalf("Resolve() ID = %#v, want %#v", got.ID(), v1.id)
	}
}

func TestNewRegistryRejectsInvalidDefinitions(t *testing.T) {
	valid := stubTemplate{id: template.ID{Name: "code-change", Version: 1}}

	tests := []struct {
		name        string
		definitions []template.Template
	}{
		{name: "nil", definitions: []template.Template{nil}},
		{name: "empty name", definitions: []template.Template{stubTemplate{id: template.ID{Version: 1}}}},
		{name: "zero version", definitions: []template.Template{stubTemplate{id: template.ID{Name: "code-change"}}}},
		{name: "negative version", definitions: []template.Template{stubTemplate{id: template.ID{Name: "code-change", Version: -1}}}},
		{name: "duplicate", definitions: []template.Template{valid, valid}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := template.NewRegistry(tt.definitions...); err == nil {
				t.Fatal("NewRegistry() error = nil")
			}
		})
	}
}

func TestRegistryRejectsUnknownTemplate(t *testing.T) {
	registry, err := template.NewRegistry(stubTemplate{id: template.ID{Name: "code-change", Version: 1}})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []template.ID{
		{Name: "other", Version: 1},
		{Name: "code-change", Version: 2},
	} {
		if _, err := registry.Resolve(id); !errors.Is(err, template.ErrUnknownTemplate) {
			t.Fatalf("Resolve(%v) error = %v, want ErrUnknownTemplate", id, err)
		}
	}
}

func TestIDString(t *testing.T) {
	if got := (template.ID{Name: "code-change", Version: 1}).String(); got != "code-change@v1" {
		t.Fatalf("ID.String() = %q", got)
	}
}
