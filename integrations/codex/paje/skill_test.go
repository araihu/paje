package paje_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsingPajeSkillIsInstallableAndContainsNoServiceCredentialNames(t *testing.T) {
	root := filepath.Join("skills", "using-paje")
	skill, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(skill, []byte("---\nname: using-paje\ndescription: Use when ")) {
		t.Fatalf("invalid skill frontmatter: %s", skill)
	}
	metadata, err := os.ReadFile(filepath.Join(root, "agents", "openai.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(metadata, []byte(`display_name: "Using Pajé"`)) ||
		!bytes.Contains(metadata, []byte(`$using-paje`)) {
		t.Fatalf("invalid skill UI metadata: %s", metadata)
	}
	for path, raw := range map[string][]byte{"SKILL.md": skill, "agents/openai.yaml": metadata} {
		for _, forbidden := range []string{
			"HATCHET_CLIENT_TOKEN", "PAJE_LEAF_GATEWAY_HATCHET_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "MEM0_API_KEY", "CODEX_HOME",
		} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("%s contains forbidden service credential name %s", path, forbidden)
			}
		}
	}
}
