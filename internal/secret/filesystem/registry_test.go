package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestResolveRequiresExactAuthorizationTuple(t *testing.T) {
	directory := t.TempDir()
	writeBindingFile(t, directory, "bindings.yaml", bindingsYAML(
		bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/codex"),
	))
	registry, err := New(directory)
	if err != nil {
		t.Fatal(err)
	}
	request := resolveRequest("harness.codex-auth", 1, workerprofile.DeliveryDirectory, "/run/paje/secrets/codex")
	binding, err := registry.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Ref() != request.Ref {
		t.Fatalf("binding ref = %#v", binding.Ref())
	}
	provider, reference := binding.Source()
	if provider != "filesystem" || reference != "/etc/paje/secrets/codex" {
		t.Fatalf("binding source = %q %q", provider, reference)
	}

	for name, mutate := range map[string]func(*secret.ResolveRequest){
		"profile": func(r *secret.ResolveRequest) { r.ProfileID.Revision = 2 },
		"capability": func(r *secret.ResolveRequest) {
			r.Requirement.Capability = "harness.other"
		},
		"stage":    func(r *secret.ResolveRequest) { r.Requirement.Stage = "verification" },
		"delivery": func(r *secret.ResolveRequest) { r.Requirement.Delivery = workerprofile.DeliveryFile },
		"target":   func(r *secret.ResolveRequest) { r.Requirement.Target = "/run/paje/secrets/other" },
		"optional": func(r *secret.ResolveRequest) { r.Requirement.Required = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := request
			mutate(&changed)
			if _, err := registry.Resolve(context.Background(), changed); err == nil {
				t.Fatal("authorization mismatch accepted")
			}
		})
	}
}

func TestFileMayContainMultipleCapabilities(t *testing.T) {
	directory := t.TempDir()
	writeBindingFile(t, directory, "bindings.yml", bindingsYAML(
		bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/codex")+
			bindingEntry("workload.api-token", 4, "/run/paje/secrets/api-token", "filesystem", "/etc/paje/secrets/api-token"),
	))
	registry, err := New(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []secret.ResolveRequest{
		resolveRequest("harness.codex-auth", 1, workerprofile.DeliveryDirectory, "/run/paje/secrets/codex"),
		resolveRequest("workload.api-token", 4, workerprofile.DeliveryDirectory, "/run/paje/secrets/api-token"),
	} {
		if _, err := registry.Resolve(context.Background(), request); err != nil {
			t.Fatalf("Resolve(%#v) error = %v", request.Ref, err)
		}
	}
}

func TestReloadAggregatesDirectoryRetainsOmittedRevisionsAndKeepsLastKnownGood(t *testing.T) {
	directory := t.TempDir()
	v1 := writeBindingFile(t, directory, "codex-v1.yaml", bindingsYAML(
		bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/codex"),
	))
	registry, err := New(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(v1); err != nil {
		t.Fatal(err)
	}
	writeBindingFile(t, directory, "codex-v2.yml", bindingsYAML(
		bindingEntry("harness.codex-auth", 2, "/run/paje/secrets/codex-v2", "filesystem", "/etc/paje/secrets/codex-v2"),
	))
	if err := registry.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	for revision, target := range map[uint64]string{1: "/run/paje/secrets/codex", 2: "/run/paje/secrets/codex-v2"} {
		if _, err := registry.Resolve(context.Background(), resolveRequest("harness.codex-auth", revision, workerprofile.DeliveryDirectory, target)); err != nil {
			t.Fatalf("revision %d unavailable: %v", revision, err)
		}
	}

	writeBindingFile(t, directory, "invalid.yaml", "api_version: wrong\n")
	if err := registry.Reload(context.Background()); err == nil {
		t.Fatal("invalid aggregate reload succeeded")
	}
	if _, err := registry.Resolve(context.Background(), resolveRequest("harness.codex-auth", 2, workerprofile.DeliveryDirectory, "/run/paje/secrets/codex-v2")); err != nil {
		t.Fatalf("last-known-good lost: %v", err)
	}
}

func TestRegistryRejectsDuplicatePairAcrossFiles(t *testing.T) {
	directory := t.TempDir()
	contents := bindingsYAML(bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/codex"))
	writeBindingFile(t, directory, "one.yaml", contents)
	writeBindingFile(t, directory, "two.yml", contents)
	if _, err := New(directory); err == nil {
		t.Fatal("duplicate capability and revision was accepted")
	}
}

func TestRegistryRejectsInvalidStrictWrapperDocuments(t *testing.T) {
	valid := bindingsYAML(bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/codex"))
	tests := map[string]string{
		"flat provisional document": strings.TrimPrefix(bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/codex"), "  "),
		"wrong api version":         strings.Replace(valid, workerprofile.APIVersionV1Alpha1, "paje.araihu.com/v2", 1),
		"wrong kind":                strings.Replace(valid, "SecretBindings", "WorkerProfile", 1),
		"unknown wrapper field":     valid + "unknown: true\n",
		"unknown binding field":     strings.Replace(valid, "    authorize:\n", "    unknown: true\n    authorize:\n", 1),
		"unknown authorize field":   strings.Replace(valid, "      stage: agent\n", "      unknown: true\n      stage: agent\n", 1),
		"unknown source field":      strings.Replace(valid, "      provider: filesystem\n", "      unknown: true\n      provider: filesystem\n", 1),
		"extra document":            valid + "---\n" + valid,
		"reserved capability":       strings.ReplaceAll(valid, "harness.codex-auth", "publisher.github-token"),
		"mutable profile":           strings.ReplaceAll(valid, "codex-go@1", "codex-go"),
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			writeBindingFile(t, directory, "bindings.yaml", contents)
			if _, err := New(directory); err == nil {
				t.Fatal("invalid binding registry accepted")
			}
		})
	}
}

func TestReloadRejectsChangedPreviouslyLoadedRevision(t *testing.T) {
	directory := t.TempDir()
	filename := writeBindingFile(t, directory, "bindings.yaml", bindingsYAML(
		bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/codex"),
	))
	registry, err := New(directory)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filename, bindingsYAML(
		bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/changed"),
	))
	if err := registry.Reload(context.Background()); err == nil {
		t.Fatal("immutable binding revision changed")
	}
	binding, err := registry.Resolve(context.Background(), resolveRequest("harness.codex-auth", 1, workerprofile.DeliveryDirectory, "/run/paje/secrets/codex"))
	if err != nil {
		t.Fatal(err)
	}
	_, reference := binding.Source()
	if reference != "/etc/paje/secrets/codex" {
		t.Fatalf("retained reference = %q", reference)
	}
}

func TestRegistryRejectsSymlinkedYAMLFile(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(t.TempDir(), "real.yaml")
	writeFile(t, target, bindingsYAML(
		bindingEntry("harness.codex-auth", 1, "/run/paje/secrets/codex", "filesystem", "/etc/paje/secrets/codex"),
	))
	if err := os.Symlink(target, filepath.Join(directory, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(directory); err == nil {
		t.Fatal("symlinked binding file accepted")
	}
}

func TestEnvironmentDeliveryRequiresSeparateTargetAllowlist(t *testing.T) {
	directory := t.TempDir()
	writeBindingFile(t, directory, "bindings.yaml", bindingsYAML(
		bindingEntry("workload.api-token", 1, "WORKLOAD_TOKEN", "environment", "WORKLOAD_TOKEN_SOURCE"),
	))
	if _, err := New(directory); err == nil {
		t.Fatal("environment delivery without a target allowlist was accepted")
	}
	registry, err := New(directory, Config{AllowedEnvironmentTargets: []string{"WORKLOAD_TOKEN"}})
	if err != nil {
		t.Fatalf("allowlisted environment target rejected: %v", err)
	}
	request := resolveRequest("workload.api-token", 1, workerprofile.DeliveryEnvironment, "WORKLOAD_TOKEN")
	if _, err := registry.Resolve(context.Background(), request); err != nil {
		t.Fatalf("allowlisted environment binding unavailable: %v", err)
	}
}

func resolveRequest(capability string, revision uint64, delivery, target string) secret.ResolveRequest {
	return secret.ResolveRequest{
		ProfileID: workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Ref:       secret.BindingRef{Capability: capability, Revision: revision},
		Requirement: workerprofile.SecretRequirement{
			Capability: capability, Stage: workerprofile.StageAgent,
			Delivery: delivery, Target: target, Required: true,
		},
	}
}

func writeBindingFile(t *testing.T, directory, name, contents string) string {
	t.Helper()
	filename := filepath.Join(directory, name)
	writeFile(t, filename, contents)
	return filename
}

func writeFile(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bindingsYAML(entries string) string {
	return "api_version: " + workerprofile.APIVersionV1Alpha1 + "\nkind: SecretBindings\nbindings:\n" + entries
}

func bindingEntry(capability string, revision uint64, target, provider, reference string) string {
	delivery := workerprofile.DeliveryDirectory
	if provider == "environment" {
		delivery = workerprofile.DeliveryEnvironment
	}
	return "  " + capability + ":\n" +
		"    revision: " + uintString(revision) + "\n" +
		"    authorize:\n" +
		"      profile: codex-go@1\n" +
		"      stage: agent\n" +
		"      delivery: " + delivery + "\n" +
		"      target: " + target + "\n" +
		"    source:\n" +
		"      provider: " + provider + "\n" +
		"      reference: " + reference + "\n"
}

func uintString(value uint64) string {
	switch value {
	case 1:
		return "1"
	case 2:
		return "2"
	case 4:
		return "4"
	default:
		panic("test helper revision is unsupported")
	}
}
