package environment_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/araihu/paje/internal/environment"
)

func TestPolicyBuildAgentEnvironmentIsAllowlistedAndRedacted(t *testing.T) {
	source := map[string]string{
		"PATH":                 "/tools",
		"LANG":                 "C.UTF-8",
		"CODEX_HOME":           "/auth/codex",
		"SAFE_CACHE":           "/cache",
		"HATCHET_CLIENT_TOKEN": "hatchet-secret",
		"MEM0_API_KEY":         "mem0-secret",
		"GITHUB_TOKEN":         "github-secret",
	}
	root := t.TempDir()
	policy, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: root,
		Source:      source,
		Allowed:     []string{"SAFE_CACHE"},
		CodexHome:   "/auth/codex",
		CodexAgent:  true,
	})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}

	result, err := policy.Build(context.Background(), environment.Request{
		RunID:         "run-123",
		Stage:         environment.StageAgent,
		RequestedKeys: []string{"SAFE_CACHE"},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	for key, want := range map[string]string{
		"PATH":       "/tools",
		"LANG":       "C.UTF-8",
		"SAFE_CACHE": "/cache",
		"CODEX_HOME": "/auth/codex",
	} {
		if got := result.Values[key]; got != want {
			t.Errorf("Values[%q] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"HOME", "TMPDIR", "TMP", "TEMP"} {
		value := result.Values[key]
		if value == "" {
			t.Errorf("Values[%q] is empty", key)
			continue
		}
		if !filepath.IsAbs(value) || filepath.Dir(value) != filepath.Join(root, "run-123", "agent") {
			t.Errorf("Values[%q] = %q, want a fresh run-stage directory", key, value)
		}
		info, statErr := os.Stat(value)
		if statErr != nil {
			t.Errorf("Stat(%q) error = %v", value, statErr)
		} else if info.Mode().Perm() != 0o700 {
			t.Errorf("mode(%q) = %#o, want 0700", value, info.Mode().Perm())
		}
	}
	for _, key := range []string{"HATCHET_CLIENT_TOKEN", "MEM0_API_KEY", "GITHUB_TOKEN"} {
		if _, ok := result.Values[key]; ok {
			t.Errorf("Values unexpectedly contains denied key %q", key)
		}
	}
	if !sort.StringsAreSorted(result.Keys) {
		t.Errorf("Keys = %q, want sorted", result.Keys)
	}
	for _, key := range result.Keys {
		if !result.Redacted[key] {
			t.Errorf("Redacted[%q] = false, want true", key)
		}
	}
	if reflect.DeepEqual(result.Values, map[string]string{}) {
		t.Fatal("Values is unexpectedly empty")
	}
}

func TestPolicyBuildRejectsDeniedAndUnknownRequestedKeys(t *testing.T) {
	policy, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: t.TempDir(),
		Source: map[string]string{
			"PATH":                 "/tools",
			"HATCHET_CLIENT_TOKEN": "hatchet-secret",
		},
		Allowed: []string{"SAFE_CACHE"},
	})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}

	for _, key := range []string{"HATCHET_CLIENT_TOKEN", "UNKNOWN"} {
		_, err := policy.Build(context.Background(), environment.Request{
			RunID:         "run-123",
			Stage:         environment.StageVerification,
			RequestedKeys: []string{key},
		})
		if err == nil {
			t.Errorf("Build(requested %q) error = nil, want rejection", key)
		}
	}
}

func TestPolicyBuildAgentWithoutCodexAuth(t *testing.T) {
	policy, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: t.TempDir(),
		Source:      map[string]string{"PATH": "/tools"},
	})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}

	result, err := policy.Build(context.Background(), environment.Request{
		RunID: "run-mock-agent",
		Stage: environment.StageAgent,
	})
	if err != nil {
		t.Fatalf("Build() error = %v, want non-Codex agent environment", err)
	}
	if _, found := result.Values["CODEX_HOME"]; found {
		t.Error("Values contains CODEX_HOME for a non-Codex agent")
	}
}

func TestPolicyRejectsGitAndSSHCredentialChannels(t *testing.T) {
	channels := []string{
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
	}
	for _, key := range channels {
		t.Run(key, func(t *testing.T) {
			policy, err := environment.NewPolicy(environment.Config{
				RuntimeRoot: t.TempDir(),
				Source:      map[string]string{"PATH": "/tools", key: "credential-channel"},
				Allowed:     []string{key},
			})
			if err != nil {
				t.Fatalf("NewPolicy() error = %v", err)
			}

			if _, err := policy.Build(context.Background(), environment.Request{
				RunID:         "run-verification",
				Stage:         environment.StageVerification,
				RequestedKeys: []string{key},
			}); err == nil {
				t.Fatalf("Build(requested %q) error = nil, want credential-channel rejection", key)
			}
		})
	}
}

func TestPolicyHardDeniesWorkerCredentialsEvenWhenAllowed(t *testing.T) {
	credentials := []string{
		"HATCHET_CLIENT_TOKEN",
		"HATCHET_WORKER_TOKEN",
		"MEM0_API_KEY",
		"MEM0_SERVICE_TOKEN",
	}
	source := map[string]string{"PATH": "/tools"}
	for _, key := range credentials {
		source[key] = "secret"
	}
	policy, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: t.TempDir(),
		Source:      source,
		Allowed:     credentials,
		CodexHome:   "/auth/codex",
		CodexAgent:  true,
	})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}

	for _, stage := range []environment.Stage{environment.StageAgent, environment.StageVerification} {
		t.Run(string(stage), func(t *testing.T) {
			if _, err := policy.Build(context.Background(), environment.Request{
				RunID:         "run-123",
				Stage:         stage,
				RequestedKeys: credentials,
			}); err == nil {
				t.Fatal("Build() error = nil, want hard credential denial")
			}

			result, err := policy.Build(context.Background(), environment.Request{RunID: "run-123", Stage: stage})
			if err != nil {
				t.Fatalf("Build(without credentials) error = %v", err)
			}
			for _, key := range credentials {
				if _, found := result.Values[key]; found {
					t.Errorf("Values unexpectedly contains hard-denied key %q", key)
				}
			}
		})
	}
}

func TestPolicyStagesKeepServiceCredentialsIsolated(t *testing.T) {
	policy, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: t.TempDir(),
		Source: map[string]string{
			"PATH":         "/tools",
			"CODEX_HOME":   "/auth/codex",
			"GITHUB_TOKEN": "github-secret",
			"GH_TOKEN":     "gh-secret",
		},
		Allowed:    []string{"CODEX_HOME", "GITHUB_TOKEN", "GH_TOKEN"},
		CodexHome:  "/auth/codex",
		CodexAgent: true,
	})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}

	for _, request := range []struct {
		stage environment.Stage
		key   string
	}{
		{stage: environment.StageAgent, key: "GITHUB_TOKEN"},
		{stage: environment.StageVerification, key: "CODEX_HOME"},
		{stage: environment.StageVerification, key: "GH_TOKEN"},
		{stage: environment.StagePublisher, key: "CODEX_HOME"},
	} {
		if _, err := policy.Build(context.Background(), environment.Request{
			RunID:         "run-123",
			Stage:         request.stage,
			RequestedKeys: []string{request.key},
		}); err == nil {
			t.Errorf("Build(%q, requested %q) error = nil, want stage-managed denial", request.stage, request.key)
		}
	}

	for _, stage := range []environment.Stage{environment.StageAgent, environment.StageVerification, environment.StagePublisher} {
		result, buildErr := policy.Build(context.Background(), environment.Request{RunID: "run-123", Stage: stage})
		if buildErr != nil {
			t.Fatalf("Build(%q) error = %v", stage, buildErr)
		}
		_, hasCodex := result.Values["CODEX_HOME"]
		_, hasGitHub := result.Values["GITHUB_TOKEN"]
		if got, want := hasCodex, stage == environment.StageAgent; got != want {
			t.Errorf("Build(%q) CODEX_HOME present = %t, want %t", stage, got, want)
		}
		if got, want := hasGitHub, stage == environment.StagePublisher; got != want {
			t.Errorf("Build(%q) GITHUB_TOKEN present = %t, want %t", stage, got, want)
		}
	}
}

func TestPolicyRejectsUnsafeRunIDBeforeCreatingPaths(t *testing.T) {
	root := t.TempDir()
	policy, err := environment.NewPolicy(environment.Config{RuntimeRoot: root, Source: map[string]string{"PATH": "/tools"}})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	if _, err := policy.Build(context.Background(), environment.Request{RunID: "../escape", Stage: environment.StageVerification}); err == nil {
		t.Fatal("Build() error = nil, want invalid run ID rejection")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("runtime root entries = %v, want none", entries)
	}
}

func TestPolicyCleanupRemovesOnlyValidatedRunDirectory(t *testing.T) {
	root := t.TempDir()
	policy, err := environment.NewPolicy(environment.Config{RuntimeRoot: root, Source: map[string]string{"PATH": "/tools"}})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	if _, err := policy.Build(context.Background(), environment.Request{RunID: "run-123", Stage: environment.StageVerification}); err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if err := policy.Cleanup(context.Background(), "run-123"); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run-123")); !os.IsNotExist(err) {
		t.Fatalf("run directory exists or stat failed: %v", err)
	}
	if err := policy.Cleanup(context.Background(), "../escape"); err == nil {
		t.Fatal("Cleanup(unsafe) error = nil, want rejection")
	}
}
