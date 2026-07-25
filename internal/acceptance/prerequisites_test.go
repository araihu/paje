package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptedInAcceptancePrerequisiteFailuresAreFatal(t *testing.T) {
	if helper := os.Getenv("PAJE_ACCEPTANCE_PREREQUISITE_HELPER"); helper != "" {
		switch helper {
		case "github":
			TestGitHubPublicationAcceptance(t)
		case "codex-binary":
			TestCodexArtifactAcceptance(t)
		case "codex-auth":
			_ = existingCodexHome(t)
		default:
			t.Fatalf("unknown prerequisite helper %q", helper)
		}
		return
	}

	tests := []struct {
		name      string
		helper    string
		env       map[string]string
		wantError string
	}{
		{
			name: "GitHub variables", helper: "github",
			env: map[string]string{
				"PAJE_GITHUB_ACCEPTANCE":      "1",
				"PAJE_GITHUB_TOKEN":           "",
				"PAJE_GITHUB_TEST_REPOSITORY": "",
				"PAJE_GITHUB_TEST_BASE_REF":   "",
				"PAJE_GITHUB_TEST_RUN_ID":     "",
			},
			wantError: "required acceptance variables",
		},
		{
			name: "Codex executable", helper: "codex-binary",
			env: map[string]string{
				"PAJE_CODEX_INTEGRATION": "1",
				"PATH":                   t.TempDir(),
			},
			wantError: "authenticated Codex acceptance requires codex on PATH",
		},
		{
			name: "Codex auth", helper: "codex-auth",
			env: map[string]string{
				"CODEX_HOME": filepath.Join(t.TempDir(), "missing-auth"),
			},
			wantError: "authenticated Codex acceptance requires CODEX_HOME auth.json",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestOptedInAcceptancePrerequisiteFailuresAreFatal$")
			overrides := map[string]string{"PAJE_ACCEPTANCE_PREREQUISITE_HELPER": test.helper}
			for key, value := range test.env {
				overrides[key] = value
			}
			command.Env = replacedEnvironment(overrides)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("opted-in missing prerequisite exited successfully: %s", output)
			}
			if !strings.Contains(string(output), test.wantError) || strings.Contains(string(output), "SKIP") {
				t.Fatalf("failure output = %q, want fatal prerequisite %q", output, test.wantError)
			}
		})
	}
}

func replacedEnvironment(overrides map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[key]; !replaced {
			result = append(result, item)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}
