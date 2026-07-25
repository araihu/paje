package config_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
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
	wantRoots := []string{
		filepath.Join(cfg.WorkspaceRoot, "runs"),
		filepath.Join(cfg.WorkspaceRoot, "artifacts"),
		filepath.Join(cfg.WorkspaceRoot, "runtime"),
	}
	if got := []string{cfg.RunRoot, cfg.ArtifactRoot, cfg.RuntimeRoot}; !reflect.DeepEqual(got, wantRoots) {
		t.Errorf("beta roots = %#v, want %#v", got, wantRoots)
	}
	if cfg.ArtifactLimitBytes != 10<<20 {
		t.Errorf("ArtifactLimitBytes = %d, want %d", cfg.ArtifactLimitBytes, 10<<20)
	}
	if cfg.CommandOutputLimitBytes != 1<<20 {
		t.Errorf("CommandOutputLimitBytes = %d, want %d", cfg.CommandOutputLimitBytes, 1<<20)
	}
	if cfg.PublisherAdapter != "mock" {
		t.Errorf("PublisherAdapter = %q, want mock", cfg.PublisherAdapter)
	}
	if cfg.GitHubAPIURL != "https://api.github.com" {
		t.Errorf("GitHubAPIURL = %q, want GitHub public API", cfg.GitHubAPIURL)
	}
	if cfg.EnvironmentAllowlist == nil || len(cfg.EnvironmentAllowlist) != 0 {
		t.Errorf("EnvironmentAllowlist = %#v, want non-nil empty list", cfg.EnvironmentAllowlist)
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

func TestLoadAcceptsCodexRunnerAdapter(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(environment(map[string]string{
		"HATCHET_CLIENT_TOKEN": "hatchet-token",
		"PAJE_RUNNER_ADAPTER":  "CoDeX",
		"CODEX_HOME":           "/codex-home",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.RunnerAdapter != "codex" {
		t.Errorf("RunnerAdapter = %q, want codex", cfg.RunnerAdapter)
	}
	if cfg.RunnerCommand != "codex" {
		t.Errorf("RunnerCommand = %q, want codex", cfg.RunnerCommand)
	}
	if cfg.CodexHome != "/codex-home" {
		t.Errorf("CodexHome = %q, want /codex-home", cfg.CodexHome)
	}
}

func TestLoadParsesBetaLimitsRootsAndEnvironmentAllowlist(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"HATCHET_CLIENT_TOKEN":            "hatchet-token",
		"PAJE_WORKSPACE_ROOT":             "/srv/paje/workspaces",
		"PAJE_RUN_ROOT":                   "/srv/paje/runs",
		"PAJE_ARTIFACT_ROOT":              "/srv/paje/artifacts",
		"PAJE_RUNTIME_ROOT":               "/run/paje",
		"PAJE_ARTIFACT_LIMIT_BYTES":       strconv.FormatInt(20<<20, 10),
		"PAJE_COMMAND_OUTPUT_LIMIT_BYTES": strconv.FormatInt(2<<20, 10),
		"PAJE_ENV_ALLOWLIST":              `["HTTP_PROXY","NO_PROXY"]`,
		"HTTP_PROXY":                      "http://proxy.example.test",
		"NO_PROXY":                        "localhost,127.0.0.1",
	}
	cfg, err := config.Load(environment(values))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := []string{cfg.RunRoot, cfg.ArtifactRoot, cfg.RuntimeRoot}, []string{
		"/srv/paje/runs", "/srv/paje/artifacts", "/run/paje",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("beta roots = %#v, want %#v", got, want)
	}
	if cfg.ArtifactLimitBytes != 20<<20 || cfg.CommandOutputLimitBytes != 2<<20 {
		t.Errorf("limits = %d/%d", cfg.ArtifactLimitBytes, cfg.CommandOutputLimitBytes)
	}
	if got, want := cfg.EnvironmentAllowlist, []string{"HTTP_PROXY", "NO_PROXY"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EnvironmentAllowlist = %#v, want %#v", got, want)
	}
	if got := cfg.Environment["HTTP_PROXY"]; got != values["HTTP_PROXY"] {
		t.Errorf("Environment[HTTP_PROXY] = %q, want configured value", got)
	}
}

func TestLoadRejectsGitAndSSHEnvironmentChannels(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_SYSTEM",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
		"GIT_PROXY_COMMAND",
		"GIT_SSH",
		"GIT_SSH_COMMAND",
		"SSH_AGENT_PID",
		"SSH_AUTH_SOCK",
	} {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			encoded, err := json.Marshal([]string{key})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			_, err = config.Load(environment(map[string]string{
				"HATCHET_CLIENT_TOKEN": "hatchet-token",
				"PAJE_ENV_ALLOWLIST":   string(encoded),
				key:                    "credential-channel",
			}))
			if err == nil {
				t.Fatalf("Load(allowlist %q) error = nil, want credential-channel rejection", key)
			}
			if strings.Contains(err.Error(), "credential-channel") {
				t.Fatalf("Load() error exposes environment value: %v", err)
			}
		})
	}
}

func TestLoadAcceptsGitHubPublisherWithSeparatedCredential(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(environment(map[string]string{
		"HATCHET_CLIENT_TOKEN":   "hatchet-token",
		"PAJE_PUBLISHER_ADAPTER": "GitHub",
		"GITHUB_TOKEN":           "github-token",
		"GITHUB_API_URL":         "https://github.example.test/api/v3",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PublisherAdapter != "github" || cfg.GitHubToken != "github-token" ||
		cfg.GitHubAPIURL != "https://github.example.test/api/v3" {
		t.Errorf("GitHub config = adapter %q token %q URL %q", cfg.PublisherAdapter, cfg.GitHubToken, cfg.GitHubAPIURL)
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
			name: "Codex without home",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_RUNNER_ADAPTER":  "codex",
			},
		},
		{
			name: "unknown publisher adapter",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN":   "token",
				"PAJE_PUBLISHER_ADAPTER": "unknown",
			},
		},
		{
			name: "GitHub publisher without token",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN":   "token",
				"PAJE_PUBLISHER_ADAPTER": "github",
			},
		},
		{
			name: "relative run root",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_RUN_ROOT":        "relative/runs",
			},
		},
		{
			name: "duplicate durable roots",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_RUN_ROOT":        "/srv/paje/data",
				"PAJE_ARTIFACT_ROOT":   "/srv/paje/data",
			},
		},
		{
			name: "zero artifact limit",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN":      "token",
				"PAJE_ARTIFACT_LIMIT_BYTES": "0",
			},
		},
		{
			name: "negative command output limit",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN":            "token",
				"PAJE_COMMAND_OUTPUT_LIMIT_BYTES": "-1",
			},
		},
		{
			name: "malformed environment allowlist",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_ENV_ALLOWLIST":   `["unterminated"`,
			},
		},
		{
			name: "worker credential in environment allowlist",
			env: map[string]string{
				"HATCHET_CLIENT_TOKEN": "token",
				"PAJE_ENV_ALLOWLIST":   `["GITHUB_TOKEN"]`,
				"GITHUB_TOKEN":         "must-not-pass",
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

func TestLoadErrorsNeverExposeSecrets(t *testing.T) {
	t.Parallel()

	secrets := []string{"hatchet-super-secret", "mem0-super-secret", "github-super-secret"}
	_, err := config.Load(environment(map[string]string{
		"HATCHET_CLIENT_TOKEN":      secrets[0],
		"MEM0_API_KEY":              secrets[1],
		"GITHUB_TOKEN":              secrets[2],
		"PAJE_MEMORY_ADAPTER":       "mem0",
		"PAJE_PUBLISHER_ADAPTER":    "github",
		"PAJE_ARTIFACT_LIMIT_BYTES": "not-a-number",
	}))
	if err == nil {
		t.Fatal("Load() error = nil, want invalid limit error")
	}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("Load() error exposed secret %q: %v", secret, err)
		}
	}
}

func environment(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
