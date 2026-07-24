package config_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/config"
)

func TestLoadUsesMockDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(environment(map[string]string{
		"HATCHET_CLIENT_TOKEN": "hatchet-token",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HatchetClientToken != "hatchet-token" {
		t.Errorf("HatchetClientToken = %q", cfg.HatchetClientToken)
	}
	if cfg.MemoryAdapter != "mock" ||
		cfg.WorkspaceAdapter != "mock" ||
		cfg.RunnerAdapter != "mock" {
		t.Errorf("adapter defaults = %q/%q/%q", cfg.MemoryAdapter, cfg.WorkspaceAdapter, cfg.RunnerAdapter)
	}
	if cfg.RunnerCommand != "codex" {
		t.Errorf("RunnerCommand = %q, want codex", cfg.RunnerCommand)
	}
	if !reflect.DeepEqual(cfg.RunnerArgs, []string{"exec"}) {
		t.Errorf("RunnerArgs = %#v, want [exec]", cfg.RunnerArgs)
	}
	if !strings.HasSuffix(cfg.WorkspaceRoot, filepath.Join("paje", "workspaces")) {
		t.Errorf("WorkspaceRoot = %q, want paje/workspaces suffix", cfg.WorkspaceRoot)
	}
}

func TestLoadParsesExplicitRealAdapters(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(environment(map[string]string{
		"HATCHET_CLIENT_TOKEN":   "hatchet-token",
		"PAJE_MEMORY_ADAPTER":    "MEM0",
		"PAJE_WORKSPACE_ADAPTER": "GIT",
		"PAJE_RUNNER_ADAPTER":    "LOCAL",
		"MEM0_API_KEY":           "mem0-key",
		"MEM0_BASE_URL":          "https://mem0.example.test",
		"PAJE_WORKSPACE_ROOT":    "/var/lib/paje",
		"PAJE_RUNNER_COMMAND":    "claude",
		"PAJE_RUNNER_ARGS":       `["-p","--output-format","json"]`,
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MemoryAdapter != "mem0" ||
		cfg.WorkspaceAdapter != "git" ||
		cfg.RunnerAdapter != "local" {
		t.Errorf("adapters = %q/%q/%q", cfg.MemoryAdapter, cfg.WorkspaceAdapter, cfg.RunnerAdapter)
	}
	if cfg.Mem0APIKey != "mem0-key" || cfg.Mem0BaseURL != "https://mem0.example.test" {
		t.Errorf("Mem0 config = key %q, URL %q", cfg.Mem0APIKey, cfg.Mem0BaseURL)
	}
	if cfg.WorkspaceRoot != "/var/lib/paje" || cfg.RunnerCommand != "claude" {
		t.Errorf("runtime config = root %q, command %q", cfg.WorkspaceRoot, cfg.RunnerCommand)
	}
	if !reflect.DeepEqual(cfg.RunnerArgs, []string{"-p", "--output-format", "json"}) {
		t.Errorf("RunnerArgs = %#v", cfg.RunnerArgs)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "missing Hatchet token",
			env:  map[string]string{},
		},
		{
			name: "unknown memory adapter",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_MEMORY_ADAPTER":  "unknown",
			},
		},
		{
			name: "Mem0 without key",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_MEMORY_ADAPTER":  "mem0",
			},
		},
		{
			name: "unknown workspace adapter",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN":   "token",
				"PAJE_WORKSPACE_ADAPTER": "unknown",
			},
		},
		{
			name: "unknown runner adapter",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_RUNNER_ADAPTER":  "unknown",
			},
		},
		{
			name: "malformed runner args",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_RUNNER_ARGS":     `["unterminated"`,
			},
		},
		{
			name: "null runner args",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_RUNNER_ARGS":     "null",
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.Load(environment(testCase.env)); err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
		})
	}
}

func environment(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
