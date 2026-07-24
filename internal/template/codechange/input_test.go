package codechange_test

import (
	"encoding/json"
	"testing"

	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/template/codechange"
)

func TestDecodeMinimalInput(t *testing.T) {
	input, err := codechange.Decode(json.RawMessage(`{
  "task_description": "update the parser",
  "repository_uri": "https://github.com/araihu/paje.git",
  "base_ref": "main",
  "tags": {"user_id": "guilhermecastro", "app_id": "araihu-paje"},
  "profile": "go",
  "publication": {"mode": "artifact"}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if input.MemoryLimit != 10 || input.Profile != "go" || input.Publication.Mode != "artifact" {
		t.Fatalf("Decode() = %#v", input)
	}
	if codechange.ID != (template.ID{Name: "code-change", Version: 1}) {
		t.Fatalf("ID = %#v", codechange.ID)
	}
}

func TestDecodeDefaultsAndRejectsExtraJSON(t *testing.T) {
	input, err := codechange.Decode(json.RawMessage(`{
  "task_description": "update the parser",
  "repository_uri": "https://github.com/araihu/paje.git",
  "base_ref": "main",
  "tags": {"user_id": "guilhermecastro", "app_id": "araihu-paje"},
  "checks": [{"name":"go test","executable":"go","timeout":"1m"}]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if input.Profile != "generic" || input.Publication.Mode != "artifact" || input.MemoryLimit != 10 {
		t.Fatalf("Decode() defaults = %#v", input)
	}

	if _, err := codechange.Decode(json.RawMessage(`{"task_description":"task","repository_uri":"repo","base_ref":"main","tags":{"user_id":"user","app_id":"app"},"checks":[{"name":"check","executable":"go","timeout":"1m"}]} {}`)); err == nil {
		t.Fatal("Decode() accepted a second JSON value")
	}
}

func TestDecodePreservesExplicitZeroMemoryLimit(t *testing.T) {
	input, err := codechange.Decode(json.RawMessage(`{
  "task_description": "update the parser",
  "repository_uri": "https://github.com/araihu/paje.git",
  "base_ref": "main",
  "tags": {"user_id": "guilhermecastro", "app_id": "araihu-paje"},
  "profile": "go",
  "memory_limit": 0
}`))
	if err != nil {
		t.Fatal(err)
	}
	if input.MemoryLimit != 0 {
		t.Fatalf("Decode() MemoryLimit = %d, want 0", input.MemoryLimit)
	}
}

func TestDecodeRejectsInvalidInput(t *testing.T) {
	valid := map[string]any{
		"task_description": "update the parser",
		"repository_uri":   "https://github.com/araihu/paje.git",
		"base_ref":         "main",
		"tags":             map[string]any{"user_id": "guilhermecastro", "app_id": "araihu-paje"},
		"checks": []any{map[string]any{
			"name": "go test", "executable": "go", "args": []any{"test", "./..."}, "timeout": "1m",
		}},
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown field", mutate: func(v map[string]any) { v["unknown"] = true }},
		{name: "empty task", mutate: func(v map[string]any) { v["task_description"] = " " }},
		{name: "empty repository", mutate: func(v map[string]any) { v["repository_uri"] = " " }},
		{name: "empty base ref", mutate: func(v map[string]any) { v["base_ref"] = " " }},
		{name: "missing user tag", mutate: func(v map[string]any) { v["tags"] = map[string]any{"app_id": "araihu-paje"} }},
		{name: "blank user tag", mutate: func(v map[string]any) { v["tags"] = map[string]any{"user_id": " ", "app_id": "araihu-paje"} }},
		{name: "missing app tag", mutate: func(v map[string]any) { v["tags"] = map[string]any{"user_id": "guilhermecastro"} }},
		{name: "blank app tag", mutate: func(v map[string]any) { v["tags"] = map[string]any{"user_id": "guilhermecastro", "app_id": " "} }},
		{name: "negative memory limit", mutate: func(v map[string]any) { v["memory_limit"] = -1 }},
		{name: "large memory limit", mutate: func(v map[string]any) { v["memory_limit"] = 1001 }},
		{name: "shell fragment", mutate: func(v map[string]any) {
			v["checks"] = []any{map[string]any{"name": "shell", "executable": "go test ./...", "timeout": "1m"}}
		}},
		{name: "bad timeout", mutate: func(v map[string]any) {
			v["checks"] = []any{map[string]any{"name": "go", "executable": "go", "timeout": "later"}}
		}},
		{name: "large timeout", mutate: func(v map[string]any) {
			v["checks"] = []any{map[string]any{"name": "go", "executable": "go", "timeout": "31m"}}
		}},
		{name: "padded absolute directory", mutate: func(v map[string]any) {
			v["checks"] = []any{map[string]any{"name": "go", "directory": " /tmp", "executable": "go", "timeout": "1m"}}
		}},
		{name: "padded escaping directory", mutate: func(v map[string]any) {
			v["checks"] = []any{map[string]any{"name": "go", "directory": " ../outside ", "executable": "go", "timeout": "1m"}}
		}},
		{name: "generic without check", mutate: func(v map[string]any) { v["profile"] = "generic"; delete(v, "checks") }},
		{name: "duplicate exclusion", mutate: func(v map[string]any) {
			v["module_exclusions"] = []any{map[string]any{"path": "site", "reason": "separate module"}, map[string]any{"path": "site", "reason": "duplicate"}}
		}},
		{name: "exclusion without reason", mutate: func(v map[string]any) { v["module_exclusions"] = []any{map[string]any{"path": "site"}} }},
		{name: "pull request wrong provider", mutate: func(v map[string]any) {
			v["publication"] = map[string]any{"mode": "pull_request", "provider": "forgejo", "target_branch": "main"}
		}},
		{name: "pull request missing branch", mutate: func(v map[string]any) {
			v["publication"] = map[string]any{"mode": "pull_request", "provider": "github"}
		}},
		{name: "unsupported publication", mutate: func(v map[string]any) { v["publication"] = map[string]any{"mode": "merge"} }},
		{name: "environment key contains equals", mutate: func(v map[string]any) { v["environment_keys"] = []any{"SAFE=value"} }},
		{name: "duplicate environment key", mutate: func(v map[string]any) { v["environment_keys"] = []any{"SAFE", "SAFE"} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := clone(valid)
			tt.mutate(payload)
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := codechange.Decode(raw); err == nil {
				t.Fatal("Decode() error = nil")
			}
		})
	}
}

func TestDefinitionValidatesInput(t *testing.T) {
	definition := codechange.Definition{}
	if definition.ID() != codechange.ID {
		t.Fatalf("Definition.ID() = %#v", definition.ID())
	}
	if err := definition.Validate(json.RawMessage(`{"task_description":"task","repository_uri":"repo","base_ref":"main","tags":{"user_id":"user","app_id":"app"},"checks":[{"name":"check","executable":"go","timeout":"1m"}]}`)); err != nil {
		t.Fatal(err)
	}
}

func clone(source map[string]any) map[string]any {
	raw, err := json.Marshal(source)
	if err != nil {
		panic(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		panic(err)
	}
	return result
}
