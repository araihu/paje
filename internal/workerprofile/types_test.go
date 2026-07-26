package workerprofile

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParseProfileIDRequiresExactRevision(t *testing.T) {
	got, err := ParseProfileID("codex-go@1")
	if err != nil || got != (ProfileID{Name: "codex-go", Revision: 1}) {
		t.Fatalf("ParseProfileID() = %#v, %v", got, err)
	}
	if got.String() != "codex-go@1" {
		t.Fatalf("ProfileID.String() = %q", got.String())
	}

	for _, value := range []string{
		"", "codex-go", "codex-go@", "codex-go@latest", "Codex@1",
		"codex_go@1", "codex-go@0", "codex-go@01", "codex-go@1@2",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseProfileID(value); err == nil {
				t.Fatalf("ParseProfileID(%q) succeeded", value)
			}
		})
	}
}

func TestCanonicalizeIsStableAndRejectsMutableOCIImage(t *testing.T) {
	first := validOCIProfile()
	second := validOCIProfile()
	slices.Reverse(second.Tools)
	slices.Reverse(second.Secrets)

	gotA, errA := Canonicalize(first)
	gotB, errB := Canonicalize(second)
	if errA != nil || errB != nil || gotA.Digest != gotB.Digest {
		t.Fatalf("canonical snapshots differ: %v %v", errA, errB)
	}
	if !slices.IsSortedFunc(gotA.Tools, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatalf("tools are not normalized: %#v", gotA.Tools)
	}
	if !slices.IsSortedFunc(gotA.Secrets, func(a, b SecretRequirement) int {
		return strings.Compare(a.Capability, b.Capability)
	}) {
		t.Fatalf("secrets are not normalized: %#v", gotA.Secrets)
	}

	first.Runtime.Image = "ghcr.io/araihu/paje-worker-codex-go:latest"
	if _, err := Canonicalize(first); err == nil {
		t.Fatal("mutable image was accepted")
	}
}

func TestCanonicalizeUsesCanonicalJSONAndDefensiveCopies(t *testing.T) {
	input := validOCIProfile()
	got, err := Canonicalize(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Digest) != 64 {
		t.Fatalf("digest length = %d", len(got.Digest))
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"digest":"`+got.Digest+`"`) {
		t.Fatalf("snapshot JSON does not contain digest: %s", encoded)
	}

	input.Tools[0].Probe.Args[0] = "mutated"
	input.Secrets[0].Target = "/run/paje/secrets/mutated"
	if got.Tools[0].Probe.Args[0] == "mutated" || got.Secrets[0].Target == input.Secrets[0].Target {
		t.Fatal("canonical snapshot aliases caller-owned slices")
	}

	corrupt := got.Clone()
	corrupt.Harness.Version = "different"
	if _, err := Canonicalize(corrupt); err == nil {
		t.Fatal("snapshot with a stale digest was accepted")
	}
}

func TestCanonicalizeRequiresSecretBindingRevision(t *testing.T) {
	profile := validOCIProfile()
	if got, err := Canonicalize(profile); err != nil || got.Secrets[0].BindingRevision != 7 {
		t.Fatalf("Canonicalize() binding revision = %d, %v, want 7", got.Secrets[0].BindingRevision, err)
	}

	profile.Secrets[0].BindingRevision = 0
	if _, err := Canonicalize(profile); err == nil {
		t.Fatal("zero secret binding revision was accepted")
	}
}

func TestSnapshotClonePreservesSliceShapeAndDoesNotAlias(t *testing.T) {
	t.Run("nil slices", func(t *testing.T) {
		clone := (Snapshot{}).Clone()
		if clone.Tools != nil || clone.Secrets != nil {
			t.Fatalf("Clone() changed nil slices: tools=%#v secrets=%#v", clone.Tools, clone.Secrets)
		}
	})

	t.Run("empty slices", func(t *testing.T) {
		clone := (Snapshot{Tools: []Tool{}, Secrets: []SecretRequirement{}}).Clone()
		if clone.Tools == nil || clone.Secrets == nil {
			t.Fatalf("Clone() changed empty slices: tools=%#v secrets=%#v", clone.Tools, clone.Secrets)
		}
	})

	t.Run("nested values", func(t *testing.T) {
		original := Snapshot{
			Tools:   []Tool{{Probe: Probe{Args: []string{"version"}}}},
			Secrets: []SecretRequirement{{Capability: "workload.token"}},
		}
		clone := original.Clone()
		clone.Tools[0].Probe.Args[0] = "mutated"
		clone.Secrets[0].Capability = "workload.other"
		if original.Tools[0].Probe.Args[0] != "version" || original.Secrets[0].Capability != "workload.token" {
			t.Fatalf("Clone() aliases nested slices: original=%#v", original)
		}
	})
}

func TestCanonicalDigestExcludesDigestField(t *testing.T) {
	input := validOCIProfile()
	got, err := Canonicalize(input)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(struct {
		APIVersion string              `json:"api_version"`
		Kind       string              `json:"kind"`
		Metadata   ProfileID           `json:"metadata"`
		Runtime    Runtime             `json:"runtime"`
		Resources  Resources           `json:"resources"`
		Harness    Harness             `json:"harness"`
		Tools      []Tool              `json:"tools"`
		Secrets    []SecretRequirement `json:"secrets,omitempty"`
	}{
		APIVersion: got.APIVersion, Kind: got.Kind, Metadata: got.Metadata,
		Runtime: got.Runtime, Resources: got.Resources, Harness: got.Harness,
		Tools: got.Tools, Secrets: got.Secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(canonical))
	if got.Digest != want {
		t.Fatalf("digest = %s, want digest of JSON without Digest field %s", got.Digest, want)
	}
}

func TestProfileValidationFailsClosed(t *testing.T) {
	tests := map[string]func(*Snapshot){
		"unsupported api":      func(p *Snapshot) { p.APIVersion = "v2" },
		"unsupported kind":     func(p *Snapshot) { p.Kind = "Pod" },
		"mutable tagged image": func(p *Snapshot) { p.Runtime.Image = "registry:5000/team/image:tag" },
		"tag plus digest":      func(p *Snapshot) { p.Runtime.Image = "registry:5000/team/image:tag@sha256:" + strings.Repeat("a", 64) },
		"extra digest marker":  func(p *Snapshot) { p.Runtime.Image = "repo@junk@sha256:" + strings.Repeat("a", 64) },
		"non-linux platform":   func(p *Snapshot) { p.Runtime.Platform = "darwin/arm64" },
		"writable root":        func(p *Snapshot) { p.Runtime.ReadOnlyRoot = false },
		"unsupported network":  func(p *Snapshot) { p.Runtime.Network = "host" },
		"zero cpu":             func(p *Snapshot) { p.Resources.CPUMillis = 0 },
		"excess memory":        func(p *Snapshot) { p.Resources.MemoryBytes = LimitsForTests().MaxMemoryBytes + 1 },
		"duplicate tool":       func(p *Snapshot) { p.Tools = append(p.Tools, p.Tools[0]) },
		"absolute executable":  func(p *Snapshot) { p.Tools[0].Probe.Executable = "/usr/bin/go" },
		"empty probe match":    func(p *Snapshot) { p.Tools[0].Probe.OutputContains = "" },
		"duplicate capability": func(p *Snapshot) { p.Secrets = append(p.Secrets, p.Secrets[0]) },
		"reserved capability":  func(p *Snapshot) { p.Secrets[0].Capability = "publisher.github-token" },
		"preflight secret":     func(p *Snapshot) { p.Secrets[0].Stage = "preflight" },
		"optional secret":      func(p *Snapshot) { p.Secrets[0].Required = false },
		"escaping target":      func(p *Snapshot) { p.Secrets[0].Target = "/run/paje/secrets/../escape" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			profile := validOCIProfile()
			mutate(&profile)
			if _, err := Canonicalize(profile); err == nil {
				t.Fatal("invalid profile was accepted")
			}
		})
	}
}

func TestHostProfileIsSecretFreeAndDeclaresNoUnenforceableOCISettings(t *testing.T) {
	host := Snapshot{
		APIVersion: APIVersionV1Alpha1,
		Kind:       KindWorkerProfile,
		Metadata:   ProfileID{Name: "host-dev", Revision: 1},
		Runtime:    Runtime{Kind: RuntimeHost},
		Harness:    Harness{ID: "codex", Version: "0.144.5"},
		Tools: []Tool{{
			Name: "git", Version: "2.52.0",
			Probe: Probe{Executable: "git", Args: []string{"--version"}, OutputContains: "git version 2.52.0"},
		}},
	}
	if _, err := Canonicalize(host); err != nil {
		t.Fatalf("valid host profile rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Snapshot){
		"image":     func(p *Snapshot) { p.Runtime.Image = "example.invalid/image@sha256:" + strings.Repeat("a", 64) },
		"platform":  func(p *Snapshot) { p.Runtime.Platform = "linux/amd64" },
		"network":   func(p *Snapshot) { p.Runtime.Network = "outbound" },
		"resources": func(p *Snapshot) { p.Resources.CPUMillis = 1 },
		"secrets": func(p *Snapshot) {
			p.Secrets = []SecretRequirement{{
				Capability: "harness.codex-auth", BindingRevision: 1, Stage: StageAgent,
				Delivery: DeliveryDirectory, Target: "/run/paje/secrets/codex", Required: true,
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			profile := host.Clone()
			mutate(&profile)
			if _, err := Canonicalize(profile); err == nil {
				t.Fatal("unsafe host profile was accepted")
			}
		})
	}
}

func validOCIProfile() Snapshot {
	return Snapshot{
		APIVersion: APIVersionV1Alpha1,
		Kind:       KindWorkerProfile,
		Metadata:   ProfileID{Name: "codex-go", Revision: 1},
		Runtime: Runtime{
			Kind: RuntimeOCI, Image: "ghcr.io/araihu/paje-worker-codex-go@sha256:" + strings.Repeat("a", 64),
			Platform: "linux/amd64", Network: NetworkOutbound, ReadOnlyRoot: true,
		},
		Resources: Resources{CPUMillis: 2000, MemoryBytes: 4 << 30, PIDs: 256},
		Harness:   Harness{ID: "codex", Version: "0.144.5"},
		Tools: []Tool{
			{Name: "git", Version: "2.52.0", Probe: Probe{Executable: "git", Args: []string{"--version"}, OutputContains: "git version 2.52.0"}},
			{Name: "go", Version: "1.26.1", Probe: Probe{Executable: "go", Args: []string{"version"}, OutputContains: "go1.26.1"}},
		},
		Secrets: []SecretRequirement{
			{Capability: "workload.api-token", BindingRevision: 11, Stage: StageAgent, Delivery: DeliveryEnvironment, Target: "WORKLOAD_API_TOKEN", Required: true},
			{Capability: "harness.codex-auth", BindingRevision: 7, Stage: StageAgent, Delivery: DeliveryDirectory, Target: "/run/paje/secrets/codex", Required: true},
		},
	}
}
