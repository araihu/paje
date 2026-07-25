// Package config loads Pajé daemon configuration from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultArtifactLimitBytes      int64 = 10 << 20
	defaultCommandOutputLimitBytes int64 = 1 << 20
	defaultGitHubAPIURL                  = "https://api.github.com"
)

var baselineEnvironmentKeys = []string{
	"PATH", "LANG", "LANGUAGE", "LC_ALL",
	"CURL_CA_BUNDLE", "AWS_CA_BUNDLE", "GIT_SSL_CAINFO", "NIX_SSL_CERT_FILE",
	"NODE_EXTRA_CA_CERTS", "NPM_CONFIG_CAFILE", "PIP_CERT", "REQUESTS_CA_BUNDLE",
	"SSL_CERT_DIR", "SSL_CERT_FILE",
}

// Config is the validated configuration for one Pajé worker process.
type Config struct {
	HatchetClientToken string

	MemoryAdapter    string
	WorkspaceAdapter string
	RunnerAdapter    string

	Mem0APIKey  string
	Mem0BaseURL string

	WorkspaceRoot string
	RunRoot       string
	ArtifactRoot  string
	RuntimeRoot   string

	ArtifactLimitBytes      int64
	CommandOutputLimitBytes int64
	EnvironmentAllowlist    []string
	// Environment contains only baseline and explicitly allowlisted process
	// values. It deliberately excludes worker service credentials.
	Environment map[string]string

	PublisherAdapter string
	GitHubToken      string
	GitHubAPIURL     string

	CodexHome string

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
		RunRoot:            strings.TrimSpace(getenv("PAJE_RUN_ROOT")),
		ArtifactRoot:       strings.TrimSpace(getenv("PAJE_ARTIFACT_ROOT")),
		RuntimeRoot:        strings.TrimSpace(getenv("PAJE_RUNTIME_ROOT")),
		PublisherAdapter:   normalizedOrDefault(getenv("PAJE_PUBLISHER_ADAPTER"), "mock"),
		GitHubToken:        strings.TrimSpace(getenv("GITHUB_TOKEN")),
		GitHubAPIURL:       strings.TrimSpace(getenv("GITHUB_API_URL")),
		CodexHome:          strings.TrimSpace(getenv("CODEX_HOME")),
		RunnerCommand:      strings.TrimSpace(getenv("PAJE_RUNNER_COMMAND")),
	}
	if config.HatchetClientToken == "" {
		return Config{}, fmt.Errorf("load config: HATCHET_CLIENT_TOKEN is required")
	}
	if config.WorkspaceRoot == "" {
		config.WorkspaceRoot = filepath.Join(os.TempDir(), "paje", "workspaces")
	}
	if config.RunRoot == "" {
		config.RunRoot = filepath.Join(config.WorkspaceRoot, "runs")
	}
	if config.ArtifactRoot == "" {
		config.ArtifactRoot = filepath.Join(config.WorkspaceRoot, "artifacts")
	}
	if config.RuntimeRoot == "" {
		config.RuntimeRoot = filepath.Join(config.WorkspaceRoot, "runtime")
	}
	if config.GitHubAPIURL == "" {
		config.GitHubAPIURL = defaultGitHubAPIURL
	}
	if config.RunnerCommand == "" {
		config.RunnerCommand = "codex"
	}

	artifactLimit, err := positiveInt64(
		"PAJE_ARTIFACT_LIMIT_BYTES",
		getenv("PAJE_ARTIFACT_LIMIT_BYTES"),
		defaultArtifactLimitBytes,
	)
	if err != nil {
		return Config{}, err
	}
	config.ArtifactLimitBytes = artifactLimit
	commandOutputLimit, err := positiveInt64(
		"PAJE_COMMAND_OUTPUT_LIMIT_BYTES",
		getenv("PAJE_COMMAND_OUTPUT_LIMIT_BYTES"),
		defaultCommandOutputLimitBytes,
	)
	if err != nil {
		return Config{}, err
	}
	config.CommandOutputLimitBytes = commandOutputLimit

	runnerArgs, err := parseStringArray("PAJE_RUNNER_ARGS", getenv("PAJE_RUNNER_ARGS"), []string{"exec"})
	if err != nil {
		return Config{}, err
	}
	config.RunnerArgs = runnerArgs
	allowlist, err := parseStringArray("PAJE_ENV_ALLOWLIST", getenv("PAJE_ENV_ALLOWLIST"), []string{})
	if err != nil {
		return Config{}, err
	}
	if err := validateEnvironmentAllowlist(allowlist); err != nil {
		return Config{}, err
	}
	config.EnvironmentAllowlist = allowlist
	config.Environment = selectedEnvironment(getenv, allowlist)

	if err := validateAdapter("memory", config.MemoryAdapter, "mock", "mem0"); err != nil {
		return Config{}, err
	}
	if config.MemoryAdapter == "mem0" && config.Mem0APIKey == "" {
		return Config{}, fmt.Errorf("load config: MEM0_API_KEY is required for the mem0 adapter")
	}
	if err := validateAdapter("workspace", config.WorkspaceAdapter, "mock", "git"); err != nil {
		return Config{}, err
	}
	if err := validateAdapter("runner", config.RunnerAdapter, "mock", "local", "codex"); err != nil {
		return Config{}, err
	}
	if config.RunnerAdapter == "codex" && config.CodexHome == "" {
		return Config{}, fmt.Errorf("load config: CODEX_HOME is required for the codex adapter")
	}
	if err := validateAdapter("publisher", config.PublisherAdapter, "mock", "github"); err != nil {
		return Config{}, err
	}
	if config.PublisherAdapter == "github" && config.GitHubToken == "" {
		return Config{}, fmt.Errorf("load config: GITHUB_TOKEN is required for the github publisher")
	}
	if err := validateRoots(config.RunRoot, config.ArtifactRoot, config.RuntimeRoot); err != nil {
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

func parseStringArray(name, raw string, fallback []string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		values := make([]string, len(fallback))
		copy(values, fallback)
		return values, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("load config: parse %s as JSON string array: %w", name, err)
	}
	if values == nil {
		return nil, fmt.Errorf("load config: %s must be a JSON string array", name)
	}
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("load config: %s[%d] must not be empty", name, index)
		}
		if name == "PAJE_ENV_ALLOWLIST" {
			if strings.ContainsAny(value, "=\x00") {
				return nil, fmt.Errorf("load config: %s[%d] is not a valid environment key", name, index)
			}
			if _, exists := seen[value]; exists {
				return nil, fmt.Errorf("load config: %s contains duplicate key %q", name, value)
			}
			seen[value] = struct{}{}
		}
	}
	return values, nil
}

func positiveInt64(name, raw string, fallback int64) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("load config: %s must be a positive integer", name)
	}
	return value, nil
}

func selectedEnvironment(getenv func(string) string, allowlist []string) map[string]string {
	keys := make([]string, 0, len(baselineEnvironmentKeys)+len(allowlist))
	keys = append(keys, baselineEnvironmentKeys...)
	keys = append(keys, allowlist...)
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value := getenv(key); value != "" {
			result[key] = value
		}
	}
	return result
}

func validateEnvironmentAllowlist(allowlist []string) error {
	for _, key := range allowlist {
		if strings.HasPrefix(key, "HATCHET_") || strings.HasPrefix(key, "MEM0_") ||
			strings.HasPrefix(key, "GITHUB_") || strings.HasPrefix(key, "PAJE_GIT_") ||
			key == "GH_TOKEN" || key == "GIT_ASKPASS" || key == "CODEX_HOME" ||
			key == "HOME" || key == "TMPDIR" || key == "TMP" || key == "TEMP" {
			return fmt.Errorf("load config: PAJE_ENV_ALLOWLIST contains worker or stage-managed key %q", key)
		}
	}
	return nil
}

func validateRoots(roots ...string) error {
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("load config: run, artifact, and runtime roots must be absolute")
		}
		clean := filepath.Clean(root)
		if _, exists := seen[clean]; exists {
			return fmt.Errorf("load config: run, artifact, and runtime roots must be distinct")
		}
		seen[clean] = struct{}{}
	}
	return nil
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
