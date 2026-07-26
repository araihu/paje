// Package config loads Pajé daemon configuration from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/araihu/paje/internal/secret"
)

const (
	defaultArtifactLimitBytes       int64 = 10 << 20
	defaultCommandOutputLimitBytes  int64 = 1 << 20
	defaultSecretProviderMaxBytes   int64 = 1 << 20
	defaultSecretProviderMaxEntries       = 1024
	defaultGitHubAPIURL                   = "https://api.github.com"
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

	WorkerProfileDirectory string
	SecretBindingDirectory string
	CodeChangeExecutor     string

	SecretFilesystemRoots            []string
	SecretEnvironmentSourceAllowlist []string
	SecretEnvironmentTargetAllowlist []string
	SecretProviderMaxBytes           int64
	SecretProviderMaxEntries         int
	// SecretEnvironment contains only operator-allowlisted provider source
	// values. It is transient composition input and must never enter durable
	// workflow evidence.
	SecretEnvironment map[string]string

	DockerEndpoint         string
	DockerRegistryAuthFile string
	HostExecutorEnabled    bool
	ProductionOnly         bool
}

// Load reads and validates configuration with getenv.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, fmt.Errorf("load config: environment reader is required")
	}

	config := Config{
		HatchetClientToken:     strings.TrimSpace(getenv("HATCHET_CLIENT_TOKEN")),
		MemoryAdapter:          normalizedOrDefault(getenv("PAJE_MEMORY_ADAPTER"), "mock"),
		WorkspaceAdapter:       normalizedOrDefault(getenv("PAJE_WORKSPACE_ADAPTER"), "mock"),
		RunnerAdapter:          normalizedOrDefault(getenv("PAJE_RUNNER_ADAPTER"), "mock"),
		Mem0APIKey:             strings.TrimSpace(getenv("MEM0_API_KEY")),
		Mem0BaseURL:            strings.TrimSpace(getenv("MEM0_BASE_URL")),
		WorkspaceRoot:          strings.TrimSpace(getenv("PAJE_WORKSPACE_ROOT")),
		RunRoot:                strings.TrimSpace(getenv("PAJE_RUN_ROOT")),
		ArtifactRoot:           strings.TrimSpace(getenv("PAJE_ARTIFACT_ROOT")),
		RuntimeRoot:            strings.TrimSpace(getenv("PAJE_RUNTIME_ROOT")),
		PublisherAdapter:       normalizedOrDefault(getenv("PAJE_PUBLISHER_ADAPTER"), "mock"),
		GitHubToken:            strings.TrimSpace(getenv("GITHUB_TOKEN")),
		GitHubAPIURL:           strings.TrimSpace(getenv("GITHUB_API_URL")),
		CodexHome:              strings.TrimSpace(getenv("CODEX_HOME")),
		RunnerCommand:          strings.TrimSpace(getenv("PAJE_RUNNER_COMMAND")),
		WorkerProfileDirectory: strings.TrimSpace(getenv("PAJE_WORKER_PROFILE_DIR")),
		SecretBindingDirectory: strings.TrimSpace(getenv("PAJE_SECRET_BINDING_DIR")),
		CodeChangeExecutor:     normalizedOrDefault(getenv("PAJE_CODECHANGE_EXECUTOR"), "mock"),
		DockerEndpoint:         strings.TrimSpace(getenv("PAJE_DOCKER_ENDPOINT")),
		DockerRegistryAuthFile: strings.TrimSpace(getenv("PAJE_DOCKER_REGISTRY_AUTH_FILE")),
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
	secretProviderMaxBytes, err := positiveInt64(
		"PAJE_SECRET_PROVIDER_MAX_BYTES",
		getenv("PAJE_SECRET_PROVIDER_MAX_BYTES"),
		defaultSecretProviderMaxBytes,
	)
	if err != nil {
		return Config{}, err
	}
	config.SecretProviderMaxBytes = secretProviderMaxBytes
	secretProviderMaxEntries, err := positiveInt(
		"PAJE_SECRET_PROVIDER_MAX_ENTRIES",
		getenv("PAJE_SECRET_PROVIDER_MAX_ENTRIES"),
		defaultSecretProviderMaxEntries,
	)
	if err != nil {
		return Config{}, err
	}
	config.SecretProviderMaxEntries = secretProviderMaxEntries
	config.HostExecutorEnabled, err = parseBool(
		"PAJE_HOST_EXECUTOR_ENABLED", getenv("PAJE_HOST_EXECUTOR_ENABLED"), false,
	)
	if err != nil {
		return Config{}, err
	}
	config.ProductionOnly, err = parseBool(
		"PAJE_PRODUCTION_ONLY", getenv("PAJE_PRODUCTION_ONLY"), false,
	)
	if err != nil {
		return Config{}, err
	}

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
	filesystemRoots, err := parseStringArray(
		"PAJE_SECRET_FILESYSTEM_ROOTS", getenv("PAJE_SECRET_FILESYSTEM_ROOTS"), []string{},
	)
	if err != nil {
		return Config{}, err
	}
	if err := validateAbsolutePaths("PAJE_SECRET_FILESYSTEM_ROOTS", filesystemRoots); err != nil {
		return Config{}, err
	}
	config.SecretFilesystemRoots = filesystemRoots
	secretEnvironmentSources, err := parseStringArray(
		"PAJE_SECRET_ENV_SOURCE_ALLOWLIST", getenv("PAJE_SECRET_ENV_SOURCE_ALLOWLIST"), []string{},
	)
	if err != nil {
		return Config{}, err
	}
	if err := validateSecretEnvironmentSourceAllowlist(secretEnvironmentSources); err != nil {
		return Config{}, err
	}
	config.SecretEnvironmentSourceAllowlist = secretEnvironmentSources
	config.SecretEnvironment = selectedNamedEnvironment(getenv, secretEnvironmentSources)
	secretEnvironmentTargets, err := parseStringArray(
		"PAJE_SECRET_ENV_TARGET_ALLOWLIST", getenv("PAJE_SECRET_ENV_TARGET_ALLOWLIST"), []string{},
	)
	if err != nil {
		return Config{}, err
	}
	for _, target := range secretEnvironmentTargets {
		if err := secret.ValidateEnvironmentTarget(target); err != nil {
			return Config{}, fmt.Errorf("load config: PAJE_SECRET_ENV_TARGET_ALLOWLIST contains invalid target %q", target)
		}
	}
	config.SecretEnvironmentTargetAllowlist = secretEnvironmentTargets

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
	if err := validatePortableExecution(config); err != nil {
		return Config{}, err
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
		if name != "PAJE_RUNNER_ARGS" {
			if strings.ContainsAny(value, "=\x00") {
				return nil, fmt.Errorf("load config: %s[%d] is invalid", name, index)
			}
			if _, exists := seen[value]; exists {
				return nil, fmt.Errorf("load config: %s contains a duplicate value", name)
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

func positiveInt(name, raw string, fallback int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("load config: %s must be a positive integer", name)
	}
	return value, nil
}

func parseBool(name, raw string, fallback bool) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("load config: %s must be true or false", name)
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

func selectedNamedEnvironment(getenv func(string) string, keys []string) map[string]string {
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
			strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "SSH_") ||
			strings.HasPrefix(key, "GITHUB_") || strings.HasPrefix(key, "PAJE_GIT_") ||
			key == "GH_TOKEN" || key == "GIT_ASKPASS" || key == "CODEX_HOME" ||
			key == "HOME" || key == "TMPDIR" || key == "TMP" || key == "TEMP" {
			return fmt.Errorf("load config: PAJE_ENV_ALLOWLIST contains worker or stage-managed key %q", key)
		}
	}
	return nil
}

func validateEnvironmentKeys(name string, keys []string) error {
	for index, key := range keys {
		if key == "" || len(key) > 128 || (key[0] != '_' && (key[0] < 'A' || key[0] > 'Z')) {
			return fmt.Errorf("load config: %s[%d] is not a valid environment key", name, index)
		}
		for _, character := range key[1:] {
			if character != '_' && (character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') {
				return fmt.Errorf("load config: %s[%d] is not a valid environment key", name, index)
			}
		}
	}
	return nil
}

func validateSecretEnvironmentSourceAllowlist(keys []string) error {
	if err := validateEnvironmentKeys("PAJE_SECRET_ENV_SOURCE_ALLOWLIST", keys); err != nil {
		return err
	}
	for _, key := range keys {
		for _, prefix := range []string{
			"HATCHET_", "MEM0_", "GITHUB_", "PAJE_", "GIT_", "SSH_", "DOCKER_", "REGISTRY_", "EXECUTOR_",
		} {
			if strings.HasPrefix(key, prefix) {
				return fmt.Errorf("load config: PAJE_SECRET_ENV_SOURCE_ALLOWLIST contains a platform credential key")
			}
		}
		if key == "GH_TOKEN" || key == "CODEX_HOME" || key == "GIT_ASKPASS" || key == "SSH_AUTH_SOCK" {
			return fmt.Errorf("load config: PAJE_SECRET_ENV_SOURCE_ALLOWLIST contains a platform credential key")
		}
	}
	return nil
}

func validateAbsolutePaths(name string, paths []string) error {
	for index, value := range paths {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
			return fmt.Errorf("load config: %s[%d] must be an absolute normalized path", name, index)
		}
	}
	return nil
}

func validatePortableExecution(config Config) error {
	if config.WorkerProfileDirectory == "" || config.SecretBindingDirectory == "" {
		return fmt.Errorf("load config: PAJE_WORKER_PROFILE_DIR and PAJE_SECRET_BINDING_DIR are required")
	}
	if err := validateAbsolutePaths("PAJE_WORKER_PROFILE_DIR", []string{config.WorkerProfileDirectory}); err != nil {
		return err
	}
	if err := validateAbsolutePaths("PAJE_SECRET_BINDING_DIR", []string{config.SecretBindingDirectory}); err != nil {
		return err
	}
	if filepath.Clean(config.WorkerProfileDirectory) == filepath.Clean(config.SecretBindingDirectory) {
		return fmt.Errorf("load config: worker profile and secret binding directories must be distinct")
	}
	if config.DockerRegistryAuthFile != "" {
		if err := validateAbsolutePaths("PAJE_DOCKER_REGISTRY_AUTH_FILE", []string{config.DockerRegistryAuthFile}); err != nil {
			return err
		}
	}

	switch config.CodeChangeExecutor {
	case "mock":
		if config.ProductionOnly {
			return fmt.Errorf("load config: mock code-change execution is forbidden in production-only mode")
		}
		if config.DockerEndpoint != "" || config.DockerRegistryAuthFile != "" {
			return fmt.Errorf("load config: Docker configuration requires the docker code-change executor")
		}
	case "host":
		if !config.HostExecutorEnabled {
			return fmt.Errorf("load config: PAJE_HOST_EXECUTOR_ENABLED=true is required for the host code-change executor")
		}
		if config.ProductionOnly {
			return fmt.Errorf("load config: host code-change execution is forbidden in production-only mode")
		}
		if config.DockerEndpoint != "" || config.DockerRegistryAuthFile != "" {
			return fmt.Errorf("load config: Docker configuration requires the docker code-change executor")
		}
	case "docker":
		if err := validateLocalUnixEndpoint(config.DockerEndpoint); err != nil {
			return err
		}
	default:
		return fmt.Errorf("load config: unsupported code-change executor %q; expected one of mock, host, docker", config.CodeChangeExecutor)
	}
	return nil
}

func validateLocalUnixEndpoint(endpoint string) error {
	if endpoint == "" || !strings.HasPrefix(endpoint, "unix://") {
		return fmt.Errorf("load config: PAJE_DOCKER_ENDPOINT must be an explicit local Unix socket")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" ||
		parsed.Path == "" || !filepath.IsAbs(parsed.Path) || filepath.Clean(parsed.Path) != parsed.Path ||
		parsed.Path == string(filepath.Separator) || strings.IndexByte(parsed.Path, 0) >= 0 {
		return fmt.Errorf("load config: PAJE_DOCKER_ENDPOINT must be an absolute local Unix socket")
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
