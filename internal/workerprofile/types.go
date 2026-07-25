package workerprofile

import "slices"

const (
	APIVersionV1Alpha1 = "paje.araihu.com/v1alpha1"
	KindWorkerProfile  = "WorkerProfile"

	RuntimeOCI  = "oci"
	RuntimeHost = "host"

	NetworkNone     = "none"
	NetworkOutbound = "outbound"

	StageAgent = "agent"

	DeliveryFile        = "file"
	DeliveryDirectory   = "directory"
	DeliveryEnvironment = "environment"
)

// ProfileID is the immutable operator-owned identity of a worker profile.
type ProfileID struct {
	Name     string `json:"name" yaml:"name"`
	Revision uint64 `json:"revision" yaml:"revision"`
}

func (id ProfileID) String() string {
	if id.Name == "" || id.Revision == 0 {
		return ""
	}
	return id.Name + "@" + uint64String(id.Revision)
}

type Runtime struct {
	Kind         string `json:"kind" yaml:"kind"`
	Image        string `json:"image,omitempty" yaml:"image,omitempty"`
	Platform     string `json:"platform,omitempty" yaml:"platform,omitempty"`
	Network      string `json:"network,omitempty" yaml:"network,omitempty"`
	ReadOnlyRoot bool   `json:"read_only_root,omitempty" yaml:"read_only_root,omitempty"`
}

type Resources struct {
	CPUMillis   int64 `json:"cpu_millis,omitempty" yaml:"cpu_millis,omitempty"`
	MemoryBytes int64 `json:"memory_bytes,omitempty" yaml:"memory_bytes,omitempty"`
	PIDs        int64 `json:"pids,omitempty" yaml:"pids,omitempty"`
}

type Harness struct {
	ID      string `json:"id" yaml:"id"`
	Version string `json:"version" yaml:"version"`
}

type Probe struct {
	Executable     string   `json:"executable" yaml:"executable"`
	Args           []string `json:"args,omitempty" yaml:"args,omitempty"`
	OutputContains string   `json:"output_contains" yaml:"output_contains"`
}

type Tool struct {
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version" yaml:"version"`
	Probe   Probe  `json:"probe" yaml:"probe"`
}

type SecretRequirement struct {
	Capability string `json:"capability" yaml:"capability"`
	Stage      string `json:"stage" yaml:"stage"`
	Delivery   string `json:"delivery" yaml:"delivery"`
	Target     string `json:"target" yaml:"target"`
	Required   bool   `json:"required" yaml:"required"`
}

// Snapshot is the normalized, safe, durable worker profile contract. Digest
// is the SHA-256 digest of the canonical JSON encoding with Digest omitted.
type Snapshot struct {
	APIVersion string              `json:"api_version" yaml:"api_version"`
	Kind       string              `json:"kind" yaml:"kind"`
	Metadata   ProfileID           `json:"metadata" yaml:"metadata"`
	Runtime    Runtime             `json:"runtime" yaml:"runtime"`
	Resources  Resources           `json:"resources" yaml:"resources"`
	Harness    Harness             `json:"harness" yaml:"harness"`
	Tools      []Tool              `json:"tools" yaml:"tools"`
	Secrets    []SecretRequirement `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Digest     string              `json:"digest" yaml:"-"`
}

func (snapshot Snapshot) Clone() Snapshot {
	clone := snapshot
	clone.Tools = slices.Clone(snapshot.Tools)
	for index := range clone.Tools {
		clone.Tools[index].Probe.Args = slices.Clone(snapshot.Tools[index].Probe.Args)
	}
	clone.Secrets = slices.Clone(snapshot.Secrets)
	return clone
}
