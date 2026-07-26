package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/workerprofile"
)

func TestReloadIsAtomicAndKeepsLastKnownGood(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "codex-go.yaml", validYAML("codex-go", 1))
	registry, err := New(dir, workerprofile.LimitsForTests())
	if err != nil {
		t.Fatal(err)
	}
	before, err := registry.Resolve(context.Background(), workerprofile.ProfileID{Name: "codex-go", Revision: 1})
	if err != nil {
		t.Fatal(err)
	}

	writeProfile(t, dir, "broken.yaml", "kind: Unknown\n")
	if err := registry.Reload(context.Background()); err == nil {
		t.Fatal("reload succeeded")
	}
	after, err := registry.Resolve(context.Background(), before.Metadata)
	if err != nil || after.Digest != before.Digest {
		t.Fatalf("last-known-good lost: %v", err)
	}
}

func TestReloadRejectsChangedExistingRevisionAndKeepsPriorDigest(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "codex-go.yaml", validYAML("codex-go", 1))
	registry, err := New(dir, workerprofile.LimitsForTests())
	if err != nil {
		t.Fatal(err)
	}
	id := workerprofile.ProfileID{Name: "codex-go", Revision: 1}
	before, err := registry.Resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	changed := strings.Replace(
		validYAML("codex-go", 1),
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
		1,
	)
	writeProfile(t, dir, "codex-go.yaml", changed)
	if err := registry.Reload(context.Background()); err == nil {
		t.Fatal("immutable worker profile revision changed")
	}
	after, err := registry.Resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Digest != before.Digest {
		t.Fatalf("retained digest = %q, want %q", after.Digest, before.Digest)
	}
}

func TestRegistryRejectsOversizedProfileFile(t *testing.T) {
	dir := t.TempDir()
	contents := validYAML("codex-go", 1)
	contents += "#" + strings.Repeat("x", (1<<20)-len(contents)+1)
	writeProfile(t, dir, "oversized.yaml", contents)
	if _, err := New(dir, workerprofile.LimitsForTests()); err == nil {
		t.Fatal("oversized worker profile was accepted")
	}
}

func TestRegistryRejectsSecondDocumentStartingBeyondSizeLimit(t *testing.T) {
	dir := t.TempDir()
	first := validYAML("codex-go", 1)
	padding := (1 << 20) - len(first) - 1
	if padding <= 0 {
		t.Fatal("profile fixture unexpectedly exceeds test boundary")
	}
	contents := first + "#" + strings.Repeat("x", padding) + "\n---\n" + validYAML("other", 1)
	writeProfile(t, dir, "trailing.yaml", contents)
	if _, err := New(dir, workerprofile.LimitsForTests()); err == nil {
		t.Fatal("second document beyond the size limit was accepted")
	}
}

func TestRegistryStrictlyLoadsOneDocumentAndRejectsDuplicateIDs(t *testing.T) {
	tests := map[string]map[string]string{
		"unknown field": {
			"profile.yaml": validYAML("codex-go", 1) + "unknown: true\n",
		},
		"trailing document": {
			"profile.yaml": validYAML("codex-go", 1) + "---\n" + validYAML("other", 1),
		},
		"duplicate id": {
			"one.yaml": validYAML("codex-go", 1),
			"two.yml":  validYAML("codex-go", 1),
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for filename, contents := range files {
				writeProfile(t, dir, filename, contents)
			}
			if _, err := New(dir, workerprofile.LimitsForTests()); err == nil {
				t.Fatal("invalid registry was accepted")
			}
		})
	}
}

func TestRegistryRejectsSymlinkedProfileAndReturnsDefensiveCopies(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "outside.yaml")
		if err := os.WriteFile(target, []byte(validYAML("codex-go", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "profile.yaml")); err != nil {
			t.Fatal(err)
		}
		if _, err := New(dir, workerprofile.LimitsForTests()); err == nil {
			t.Fatal("symlinked profile was accepted")
		}
	})

	t.Run("copy", func(t *testing.T) {
		dir := t.TempDir()
		writeProfile(t, dir, "profile.yaml", validYAML("codex-go", 1))
		registry, err := New(dir, workerprofile.LimitsForTests())
		if err != nil {
			t.Fatal(err)
		}
		id := workerprofile.ProfileID{Name: "codex-go", Revision: 1}
		first, err := registry.Resolve(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		first.Tools[0].Probe.Args[0] = "mutated"
		second, err := registry.Resolve(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if second.Tools[0].Probe.Args[0] == "mutated" {
			t.Fatal("resolved snapshot aliases registry state")
		}
	})
}

func TestReloadReplacesCompleteSetOnlyAfterValidation(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "old.yaml", validYAML("old", 1))
	registry, err := New(dir, workerprofile.LimitsForTests())
	if err != nil {
		t.Fatal(err)
	}
	original, err := registry.Resolve(context.Background(), workerprofile.ProfileID{Name: "old", Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "old.yaml")); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, dir, "new.yaml", validYAML("new", 2))
	if err := registry.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(context.Background(), workerprofile.ProfileID{Name: "old", Revision: 1}); err == nil {
		t.Fatal("removed profile remained in replacement set")
	}
	if _, err := registry.Resolve(context.Background(), workerprofile.ProfileID{Name: "new", Revision: 2}); err != nil {
		t.Fatalf("new profile unavailable: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "new.yaml")); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(validYAML("old", 1), "cpu_millis: 2000", "cpu_millis: 3000", 1)
	writeProfile(t, dir, "old.yaml", changed)
	if err := registry.Reload(context.Background()); err == nil {
		t.Fatal("removed immutable revision was re-added with changed content")
	}
	if _, err := registry.Resolve(context.Background(), workerprofile.ProfileID{Name: "new", Revision: 2}); err != nil {
		t.Fatalf("last-known-good set changed after immutable revision failure: %v", err)
	}

	writeProfile(t, dir, "old.yaml", validYAML("old", 1))
	if err := registry.Reload(context.Background()); err != nil {
		t.Fatalf("original immutable revision could not be re-added: %v", err)
	}
	readded, err := registry.Resolve(context.Background(), original.Metadata)
	if err != nil || readded.Digest != original.Digest {
		t.Fatalf("re-added immutable revision = %#v, %v", readded, err)
	}
}

func writeProfile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validYAML(name string, revision uint64) string {
	return strings.TrimSpace(`
api_version: paje.araihu.com/v1alpha1
kind: WorkerProfile
metadata:
  name: `+name+`
  revision: `+uintString(revision)+`
runtime:
  kind: oci
  image: ghcr.io/araihu/paje-worker-codex-go@sha256:`+strings.Repeat("a", 64)+`
  platform: linux/amd64
  network: outbound
  read_only_root: true
resources:
  cpu_millis: 2000
  memory_bytes: 4294967296
  pids: 256
harness:
  id: codex
  version: 0.144.5
tools:
  - name: go
    version: 1.26.1
    probe:
      executable: go
      args: ["version"]
      output_contains: go1.26.1
secrets:
  - capability: harness.codex-auth
    binding_revision: 1
    stage: agent
    delivery: directory
    target: /run/paje/secrets/codex
    required: true
`) + "\n"
}

func uintString(value uint64) string {
	if value == 1 {
		return "1"
	}
	if value == 2 {
		return "2"
	}
	panic("test helper only supports revisions 1 and 2")
}
