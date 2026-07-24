// Package config loads Pajé daemon configuration from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the validated configuration for one Pajé worker process.
type Config struct {
	HatchetClientToken string

	MemoryAdapter    string
	WorkspaceAdapter string
	RunnerAdapter    string

	Mem0APIKey  string
	Mem0BaseURL string

	WorkspaceRoot string

	RunnerCommand string
	RunnerArgs    []string
}

// Load reads and validates configuration with getenv.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, fmt.Errorf("load config: environment reader is required")
	}

	config := Config{
		HatchetClientToken: strings.TrimSpace(getenv("HATCHET_CLIENT_TOKEN")),
		MemoryAdapter:      normalizedOrDefault(getenv("PAJE_MEMORY_ADAPTER"), "mock"),
		WorkspaceAdapter:   normalizedOrDefault(getenv("PAJE_WORKSPACE_ADAPTER"), "mock"),
		RunnerAdapter:      normalizedOrDefault(getenv("PAJE_RUNNER_ADAPTER"), "mock"),
		Mem0APIKey:         strings.TrimSpace(getenv("MEM0_API_KEY")),
		Mem0BaseURL:        strings.TrimSpace(getenv("MEM0_BASE_URL")),
		WorkspaceRoot:      strings.TrimSpace(getenv("PAJE_WORKSPACE_ROOT")),
		RunnerCommand:      strings.TrimSpace(getenv("PAJE_RUNNER_COMMAND")),
	}
	if config.HatchetClientToken == "" {
		return Config{}, fmt.Errorf("load config: HATCHET_CLIENT_TOKEN is required")
	}
	if config.WorkspaceRoot == "" {
		config.WorkspaceRoot = filepath.Join(os.TempDir(), "paje", "workspaces")
	}
	if config.RunnerCommand == "" {
		config.RunnerCommand = "codex"
	}

	runnerArgs, err := parseRunnerArgs(getenv("PAJE_RUNNER_ARGS"))
	if err != nil {
		return Config{}, err
	}
	config.RunnerArgs = runnerArgs

	if err := validateAdapter("memory", config.MemoryAdapter, "mock", "mem0"); err != nil {
		return Config{}, err
	}
	if config.MemoryAdapter == "mem0" && config.Mem0APIKey == "" {
		return Config{}, fmt.Errorf("load config: MEM0_API_KEY is required for the mem0 adapter")
	}
	if err := validateAdapter("workspace", config.WorkspaceAdapter, "mock", "git"); err != nil {
		return Config{}, err
	}
	if err := validateAdapter("runner", config.RunnerAdapter, "mock", "local"); err != nil {
		return Config{}, err
	}

	return config, nil
}

func normalizedOrDefault(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	return value
}

func parseRunnerArgs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{"exec"}, nil
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("load config: parse PAJE_RUNNER_ARGS as JSON string array: %w", err)
	}
	if args == nil {
		return nil, fmt.Errorf("load config: PAJE_RUNNER_ARGS must be a JSON string array")
	}
	for index, arg := range args {
		if strings.TrimSpace(arg) == "" {
			return nil, fmt.Errorf("load config: PAJE_RUNNER_ARGS[%d] must not be empty", index)
		}
	}
	return args, nil
}

func validateAdapter(kind, selected string, allowed ...string) error {
	for _, candidate := range allowed {
		if selected == candidate {
			return nil
		}
	}
	return fmt.Errorf(
		"load config: unsupported %s adapter %q; expected one of %s",
		kind,
		selected,
		strings.Join(allowed, ", "),
	)
}
