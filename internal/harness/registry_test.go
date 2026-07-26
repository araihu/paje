package harness

import (
	"testing"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestRegistryRequiresExactHarnessAndCapabilities(t *testing.T) {
	codex := stubAdapter{id: "codex", version: "0.144.5", capabilities: map[string]bool{"harness.codex-auth": true}}
	registry, err := NewRegistry(codex, stubAdapter{id: "other", version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	profile := profileWithHarness(t, "codex", "0.144.5", "harness.codex-auth")
	got, err := registry.Resolve(profile)
	if err != nil || got.ID() != "codex" || got.Version() != "0.144.5" {
		t.Fatalf("Resolve() = %#v, %v", got, err)
	}

	profile.Harness.Version = "0.144.6"
	profile.Digest = ""
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(profile); err == nil {
		t.Fatal("unregistered exact harness version accepted")
	}

	profile = profileWithHarness(t, "codex", "0.144.5", "harness.unknown")
	if _, err := registry.Resolve(profile); err == nil {
		t.Fatal("unrecognized harness capability accepted")
	}
}

func TestRegistryRejectsDuplicateAndInvalidAdapters(t *testing.T) {
	valid := stubAdapter{id: "codex", version: "0.144.5"}
	if _, err := NewRegistry(valid, valid); err == nil {
		t.Fatal("duplicate exact harness registration accepted")
	}
	if _, err := NewRegistry(stubAdapter{id: "Codex", version: "0.144.5"}); err == nil {
		t.Fatal("invalid harness ID accepted")
	}
	if _, err := NewRegistry((*stubPointerAdapter)(nil)); err == nil {
		t.Fatal("typed nil harness accepted")
	}
}

type stubAdapter struct {
	id           string
	version      string
	capabilities map[string]bool
}

func (adapter stubAdapter) ID() string              { return adapter.id }
func (adapter stubAdapter) Version() string         { return adapter.version }
func (adapter stubAdapter) Probe() executor.Command { return executor.Command{} }
func (adapter stubAdapter) AgentCommand(string) (executor.Command, error) {
	return executor.Command{}, nil
}
func (adapter stubAdapter) Parse(executor.Result) (string, error) { return "", nil }
func (adapter stubAdapter) AcceptsCapability(capability string) bool {
	return adapter.capabilities[capability]
}

type stubPointerAdapter struct{ stubAdapter }

func profileWithHarness(t *testing.T, id, version, capability string) workerprofile.Snapshot {
	t.Helper()
	profile := workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Runtime: workerprofile.Runtime{
			Kind: workerprofile.RuntimeOCI, Image: "example.invalid/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Platform: "linux/amd64", Network: workerprofile.NetworkNone, ReadOnlyRoot: true,
		},
		Resources: workerprofile.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 64},
		Harness:   workerprofile.Harness{ID: id, Version: version},
	}
	if capability != "" {
		profile.Secrets = []workerprofile.SecretRequirement{{
			Capability: capability, Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/codex", Required: true,
		}}
	}
	canonical, err := workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
