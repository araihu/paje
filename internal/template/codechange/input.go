// Package codechange defines the built-in code-change@v1 template input.
package codechange

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/verification"
)

const defaultMemoryLimit = 10

// ID is the stable identifier for this built-in template.
var ID = template.ID{Name: "code-change", Version: 1}

// Input is the strict wire input for code-change@v1.
type Input struct {
	IdempotencyKey   string                       `json:"idempotency_key,omitempty"`
	TaskDescription  string                       `json:"task_description"`
	RepositoryURI    string                       `json:"repository_uri"`
	BaseRef          string                       `json:"base_ref"`
	MemoryQuery      string                       `json:"memory_query,omitempty"`
	MemoryLimit      int                          `json:"memory_limit,omitempty"`
	Tags             map[string]string            `json:"tags,omitempty"`
	Profile          string                       `json:"profile,omitempty"`
	Checks           []verification.CommandSpec   `json:"checks,omitempty"`
	ModuleExclusions []repository.ModuleExclusion `json:"module_exclusions,omitempty"`
	EnvironmentKeys  []string                     `json:"environment_keys,omitempty"`
	Publication      Publication                  `json:"publication,omitempty"`
}

// Publication describes the requested durable output mode.
type Publication struct {
	Mode         string `json:"mode,omitempty"`
	Provider     string `json:"provider,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	Title        string `json:"title,omitempty"`
	Draft        bool   `json:"draft,omitempty"`
}

// Definition registers code-change@v1 in a template.Registry.
type Definition struct{}

// ID returns the exact built-in template ID.
func (Definition) ID() template.ID { return ID }

// Validate proves that raw is a valid code-change@v1 input.
func (Definition) Validate(raw json.RawMessage) error {
	_, err := Decode(raw)
	return err
}

// Decode strictly decodes and validates code-change@v1 input.
func Decode(raw json.RawMessage) (Input, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Input{}, fmt.Errorf("decode code-change@v1 input: %w", err)
	}
	_, memoryLimitSet := fields["memory_limit"]

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input Input
	if err := decoder.Decode(&input); err != nil {
		return Input{}, fmt.Errorf("decode code-change@v1 input: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Input{}, err
	}
	return normalizeAndValidate(input, memoryLimitSet)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode code-change@v1 input: unexpected additional JSON value")
		}
		return fmt.Errorf("decode code-change@v1 input: %w", err)
	}
	return nil
}

func normalizeAndValidate(input Input, memoryLimitSet bool) (Input, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.TaskDescription = strings.TrimSpace(input.TaskDescription)
	input.RepositoryURI = strings.TrimSpace(input.RepositoryURI)
	input.BaseRef = strings.TrimSpace(input.BaseRef)
	input.MemoryQuery = strings.TrimSpace(input.MemoryQuery)
	input.Profile = strings.ToLower(strings.TrimSpace(input.Profile))
	input.Publication.Mode = strings.ToLower(strings.TrimSpace(input.Publication.Mode))
	input.Publication.Provider = strings.ToLower(strings.TrimSpace(input.Publication.Provider))
	input.Publication.TargetBranch = strings.TrimSpace(input.Publication.TargetBranch)
	input.Publication.Title = strings.TrimSpace(input.Publication.Title)

	if input.TaskDescription == "" || input.RepositoryURI == "" || input.BaseRef == "" {
		return Input{}, fmt.Errorf("decode code-change@v1 input: task description, repository URI, and base ref are required")
	}
	if input.MemoryLimit < 0 || input.MemoryLimit > 1000 {
		return Input{}, fmt.Errorf("decode code-change@v1 input: memory limit must be between 0 and 1000")
	}
	if !memoryLimitSet {
		input.MemoryLimit = defaultMemoryLimit
	}
	if err := normalizeTags(input.Tags); err != nil {
		return Input{}, err
	}
	if input.Profile == "" {
		input.Profile = "generic"
	}
	if input.Profile != "generic" && input.Profile != "go" {
		return Input{}, fmt.Errorf("decode code-change@v1 input: unsupported profile %q", input.Profile)
	}
	if len(input.Checks) > verification.DefaultLimits.MaxCommands {
		return Input{}, fmt.Errorf("decode code-change@v1 input: too many checks")
	}
	if input.Profile == "generic" && len(input.Checks) == 0 {
		return Input{}, fmt.Errorf("decode code-change@v1 input: generic profile requires at least one check")
	}
	for index, check := range input.Checks {
		if _, err := verification.Compile(check, ".", verification.DefaultLimits); err != nil {
			return Input{}, fmt.Errorf("decode code-change@v1 input: check %d: %w", index, err)
		}
		input.Checks[index].Name = strings.TrimSpace(check.Name)
		input.Checks[index].Directory = strings.TrimSpace(check.Directory)
		input.Checks[index].Executable = strings.TrimSpace(check.Executable)
		input.Checks[index].Timeout = strings.TrimSpace(check.Timeout)
	}
	if err := normalizeModuleExclusions(input.ModuleExclusions); err != nil {
		return Input{}, err
	}
	if err := normalizeEnvironmentKeys(&input.EnvironmentKeys); err != nil {
		return Input{}, err
	}
	if input.Publication.Mode == "" {
		input.Publication.Mode = "artifact"
	}
	if input.Publication.Mode != "artifact" && input.Publication.Mode != "pull_request" {
		return Input{}, fmt.Errorf("decode code-change@v1 input: unsupported publication mode %q", input.Publication.Mode)
	}
	if input.Publication.Mode == "pull_request" && (input.Publication.Provider != "github" || input.Publication.TargetBranch == "") {
		return Input{}, fmt.Errorf("decode code-change@v1 input: pull_request publication requires provider github and a target branch")
	}
	return input, nil
}

func normalizeTags(tags map[string]string) error {
	for key, value := range tags {
		if strings.TrimSpace(key) == "" || strings.IndexByte(key, 0) >= 0 {
			return fmt.Errorf("decode code-change@v1 input: invalid tag key")
		}
		tags[key] = strings.TrimSpace(value)
	}
	if strings.TrimSpace(tags["user_id"]) == "" || strings.TrimSpace(tags["app_id"]) == "" {
		return fmt.Errorf("decode code-change@v1 input: tags.user_id and tags.app_id are required")
	}
	return nil
}

func normalizeModuleExclusions(exclusions []repository.ModuleExclusion) error {
	seen := make(map[string]struct{}, len(exclusions))
	for index := range exclusions {
		exclusions[index].Path = strings.TrimSpace(exclusions[index].Path)
		exclusions[index].Reason = strings.TrimSpace(exclusions[index].Reason)
		if exclusions[index].Path == "" || exclusions[index].Reason == "" {
			return fmt.Errorf("decode code-change@v1 input: module exclusion path and reason are required")
		}
		if _, exists := seen[exclusions[index].Path]; exists {
			return fmt.Errorf("decode code-change@v1 input: duplicate module exclusion %q", exclusions[index].Path)
		}
		seen[exclusions[index].Path] = struct{}{}
	}
	return nil
}

func normalizeEnvironmentKeys(keys *[]string) error {
	seen := make(map[string]struct{}, len(*keys))
	for index, key := range *keys {
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsRune(key, '=') || strings.IndexByte(key, 0) >= 0 {
			return fmt.Errorf("decode code-change@v1 input: invalid environment key %q", key)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("decode code-change@v1 input: duplicate environment key %q", key)
		}
		seen[key] = struct{}{}
		(*keys)[index] = key
	}
	sort.Strings(*keys)
	return nil
}
