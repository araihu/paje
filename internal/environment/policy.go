// Package environment builds isolated, stage-specific process environments.
package environment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// StageAgent is the only stage that receives Codex authentication state.
	StageAgent Stage = "agent"
	// StageVerification runs repository checks without service credentials.
	StageVerification Stage = "verification"
	// StagePublisher is the only stage that receives GitHub credentials.
	StagePublisher Stage = "publisher"
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

var certificateKeys = map[string]struct{}{
	"CURL_CA_BUNDLE":      {},
	"AWS_CA_BUNDLE":       {},
	"GIT_SSL_CAINFO":      {},
	"NIX_SSL_CERT_FILE":   {},
	"NODE_EXTRA_CA_CERTS": {},
	"NPM_CONFIG_CAFILE":   {},
	"PIP_CERT":            {},
	"REQUESTS_CA_BUNDLE":  {},
	"SSL_CERT_DIR":        {},
	"SSL_CERT_FILE":       {},
}

var publisherCredentialKeys = []string{
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"GITHUB_APP_ID",
	"GITHUB_APP_INSTALLATION_ID",
	"GITHUB_APP_PRIVATE_KEY",
}

// Stage identifies a bounded workflow execution stage.
type Stage string

// Config configures the process values supplied by the worker and the operator.
type Config struct {
	RuntimeRoot string
	Source      map[string]string
	Allowed     []string
	CodexHome   string
}

// Request identifies the stage and non-secret operator keys needed by a child.
type Request struct {
	RunID         string
	Stage         Stage
	RequestedKeys []string
}

// Result contains child values and safe environment evidence. Values are for
// process execution only; Keys and Redacted are the serializable evidence.
type Result struct {
	Values   map[string]string
	Keys     []string
	Redacted map[string]bool
}

// Builder is the stage-environment port used by workflow services.
type Builder interface {
	Build(context.Context, Request) (Result, error)
	Cleanup(context.Context, string) error
}

// Policy builds deterministic environments from an explicit worker source.
type Policy struct {
	runtimeRoot string
	source      map[string]string
	allowed     map[string]struct{}
	codexHome   string
}

var _ Builder = (*Policy)(nil)

// NewPolicy validates and snapshots the operator configuration.
func NewPolicy(config Config) (*Policy, error) {
	runtimeRoot := strings.TrimSpace(config.RuntimeRoot)
	if runtimeRoot == "" {
		return nil, fmt.Errorf("create environment policy: runtime root is required")
	}
	absRoot, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return nil, fmt.Errorf("create environment policy: resolve runtime root: %w", err)
	}
	allowed := make(map[string]struct{}, len(config.Allowed))
	for _, key := range config.Allowed {
		if err := validateKey(key); err != nil {
			return nil, fmt.Errorf("create environment policy: allowed key: %w", err)
		}
		allowed[key] = struct{}{}
	}
	source := make(map[string]string, len(config.Source))
	for key, value := range config.Source {
		if err := validateKey(key); err != nil {
			return nil, fmt.Errorf("create environment policy: source key: %w", err)
		}
		source[key] = value
	}
	return &Policy{
		runtimeRoot: absRoot,
		source:      source,
		allowed:     allowed,
		codexHome:   config.CodexHome,
	}, nil
}

// Build creates the isolated stage directories and constructs an exact child environment.
func (p *Policy) Build(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("build environment: %w", err)
	}
	if !runIDPattern.MatchString(request.RunID) {
		return Result{}, fmt.Errorf("build environment: invalid run ID")
	}
	if !validStage(request.Stage) {
		return Result{}, fmt.Errorf("build environment: invalid stage %q", request.Stage)
	}
	requested, err := p.validateRequestedKeys(request)
	if err != nil {
		return Result{}, err
	}
	if request.Stage == StageAgent && strings.TrimSpace(p.codexHome) == "" {
		return Result{}, fmt.Errorf("build environment: Codex home is required for agent stage")
	}

	runRoot := filepath.Join(p.runtimeRoot, request.RunID)
	stageRoot := filepath.Join(runRoot, string(request.Stage))
	home := filepath.Join(stageRoot, "home")
	tmp := filepath.Join(stageRoot, "tmp")
	for _, directory := range []string{runRoot, stageRoot, home, tmp} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return Result{}, fmt.Errorf("build environment: create stage directory: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return Result{}, fmt.Errorf("build environment: secure stage directory: %w", err)
		}
	}

	values := make(map[string]string)
	for key, value := range p.source {
		if isBaselineKey(key) {
			values[key] = value
		}
	}
	values["HOME"] = home
	values["TMPDIR"] = tmp
	values["TMP"] = tmp
	values["TEMP"] = tmp
	for _, key := range requested {
		values[key] = p.source[key]
	}
	switch request.Stage {
	case StageAgent:
		values["CODEX_HOME"] = p.codexHome
	case StagePublisher:
		for _, key := range publisherCredentialKeys {
			if value, ok := p.source[key]; ok {
				values[key] = value
			}
		}
	}

	keys := sortedKeys(values)
	redacted := make(map[string]bool, len(keys))
	for _, key := range keys {
		redacted[key] = true
	}
	return Result{Values: values, Keys: keys, Redacted: redacted}, nil
}

// Cleanup removes only the validated per-run environment tree.
func (p *Policy) Cleanup(ctx context.Context, runID string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cleanup environment: %w", err)
	}
	if !runIDPattern.MatchString(runID) {
		return fmt.Errorf("cleanup environment: invalid run ID")
	}
	if err := os.RemoveAll(filepath.Join(p.runtimeRoot, runID)); err != nil {
		return fmt.Errorf("cleanup environment: remove run directory: %w", err)
	}
	return nil
}

func (p *Policy) validateRequestedKeys(request Request) ([]string, error) {
	requested := make(map[string]struct{}, len(request.RequestedKeys))
	for _, key := range request.RequestedKeys {
		if err := validateKey(key); err != nil {
			return nil, fmt.Errorf("build environment: requested key: %w", err)
		}
		if _, ok := p.source[key]; !ok {
			return nil, fmt.Errorf("build environment: requested key %q is unknown", key)
		}
		if _, ok := p.allowed[key]; !ok {
			return nil, fmt.Errorf("build environment: requested key %q is not allowed", key)
		}
		if isStageCredential(key) {
			return nil, fmt.Errorf("build environment: requested key %q is stage-managed", key)
		}
		requested[key] = struct{}{}
	}
	keys := make([]string, 0, len(requested))
	for key := range requested {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func validStage(stage Stage) bool {
	return stage == StageAgent || stage == StageVerification || stage == StagePublisher
}

func isBaselineKey(key string) bool {
	if key == "PATH" || key == "LANG" || key == "LANGUAGE" || key == "LC_ALL" {
		return true
	}
	if strings.HasPrefix(key, "LC_") {
		return true
	}
	_, ok := certificateKeys[key]
	return ok
}

func isStageCredential(key string) bool {
	if key == "CODEX_HOME" || key == "HOME" || key == "TMPDIR" || key == "TMP" || key == "TEMP" {
		return true
	}
	for _, credential := range publisherCredentialKeys {
		if key == credential {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateKey(key string) error {
	if key == "" || strings.ContainsAny(key, "=\x00") {
		return fmt.Errorf("invalid environment key %q", key)
	}
	return nil
}
