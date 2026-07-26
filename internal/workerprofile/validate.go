package workerprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const secretRoot = "/run/paje/secrets"

var (
	profileNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	capabilityPart     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	identifierPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	versionPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}$`)
	executablePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}$`)
	platformPattern    = regexp.MustCompile(`^linux/[a-z0-9][a-z0-9._\-]{0,31}$`)
	environmentPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
)

var reservedCapabilityNamespaces = map[string]struct{}{
	"paje": {}, "hatchet": {}, "mem0": {}, "submission": {},
	"publisher": {}, "git": {}, "ssh": {}, "registry": {}, "executor": {},
}

var reservedEnvironmentKeys = map[string]struct{}{
	"HOME": {}, "PATH": {}, "TMP": {}, "TEMP": {}, "TMPDIR": {},
	"PWD": {}, "OLDPWD": {}, "SHELL": {}, "USER": {}, "LOGNAME": {},
	"LANG": {}, "CODEX_HOME": {}, "GIT_ASKPASS": {}, "SSH_AUTH_SOCK": {},
}

type Limits struct {
	MaxCPUMillis   int64
	MaxMemoryBytes int64
	MaxPIDs        int64
}

func DefaultLimits() Limits {
	return Limits{
		MaxCPUMillis:   64_000,
		MaxMemoryBytes: 1 << 40,
		MaxPIDs:        32_768,
	}
}

// LimitsForTests returns generous deterministic maxima suitable for fixtures.
func LimitsForTests() Limits { return DefaultLimits() }

func ParseProfileID(value string) (ProfileID, error) {
	if strings.Count(value, "@") != 1 {
		return ProfileID{}, errors.New("worker profile reference must be exact")
	}
	name, revisionText, _ := strings.Cut(value, "@")
	if !profileNamePattern.MatchString(name) || revisionText == "" ||
		(len(revisionText) > 1 && revisionText[0] == '0') {
		return ProfileID{}, errors.New("worker profile reference is invalid")
	}
	revision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil || revision == 0 {
		return ProfileID{}, errors.New("worker profile revision is invalid")
	}
	return ProfileID{Name: name, Revision: revision}, nil
}

func uint64String(value uint64) string { return strconv.FormatUint(value, 10) }

// Canonicalize validates and normalizes a profile using safe global maxima.
func Canonicalize(snapshot Snapshot) (Snapshot, error) {
	return CanonicalizeWithLimits(snapshot, DefaultLimits())
}

// CanonicalizeWithLimits validates and normalizes a profile against
// operator-configured resource maxima.
func CanonicalizeWithLimits(snapshot Snapshot, limits Limits) (Snapshot, error) {
	if err := validateLimits(limits); err != nil {
		return Snapshot{}, err
	}

	wantDigest := snapshot.Digest
	normalized := snapshot.Clone()
	normalized.Digest = ""
	if err := validateAndNormalize(&normalized, limits); err != nil {
		return Snapshot{}, err
	}
	canonical, err := json.Marshal(canonicalSnapshot{
		APIVersion: normalized.APIVersion,
		Kind:       normalized.Kind,
		Metadata:   normalized.Metadata,
		Runtime:    normalized.Runtime,
		Resources:  normalized.Resources,
		Harness:    normalized.Harness,
		Tools:      normalized.Tools,
		Secrets:    normalized.Secrets,
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode worker profile: %w", err)
	}
	digestBytes := sha256.Sum256(canonical)
	normalized.Digest = hex.EncodeToString(digestBytes[:])
	if wantDigest != "" && wantDigest != normalized.Digest {
		return Snapshot{}, errors.New("worker profile digest does not match canonical content")
	}
	return normalized, nil
}

type canonicalSnapshot struct {
	APIVersion string              `json:"api_version"`
	Kind       string              `json:"kind"`
	Metadata   ProfileID           `json:"metadata"`
	Runtime    Runtime             `json:"runtime"`
	Resources  Resources           `json:"resources"`
	Harness    Harness             `json:"harness"`
	Tools      []Tool              `json:"tools"`
	Secrets    []SecretRequirement `json:"secrets,omitempty"`
}

func validateLimits(limits Limits) error {
	if limits.MaxCPUMillis <= 0 || limits.MaxMemoryBytes <= 0 || limits.MaxPIDs <= 0 {
		return errors.New("worker profile limits must be positive")
	}
	return nil
}

func validateAndNormalize(snapshot *Snapshot, limits Limits) error {
	if snapshot.APIVersion != APIVersionV1Alpha1 || snapshot.Kind != KindWorkerProfile {
		return errors.New("unsupported worker profile contract")
	}
	if !profileNamePattern.MatchString(snapshot.Metadata.Name) || snapshot.Metadata.Revision == 0 {
		return errors.New("invalid worker profile identity")
	}
	if !identifierPattern.MatchString(snapshot.Harness.ID) || !versionPattern.MatchString(snapshot.Harness.Version) {
		return errors.New("invalid worker profile harness")
	}
	if err := validateTools(snapshot); err != nil {
		return err
	}
	if err := validateSecrets(snapshot); err != nil {
		return err
	}

	switch snapshot.Runtime.Kind {
	case RuntimeOCI:
		if err := validateOCI(snapshot, limits); err != nil {
			return err
		}
	case RuntimeHost:
		if snapshot.Runtime.Image != "" || snapshot.Runtime.Platform != "" ||
			snapshot.Runtime.Network != "" || snapshot.Runtime.ReadOnlyRoot ||
			snapshot.Resources != (Resources{}) || len(snapshot.Secrets) != 0 {
			return errors.New("host worker profile declares unsupported isolation or secrets")
		}
	default:
		return errors.New("unsupported worker profile runtime")
	}

	slices.SortFunc(snapshot.Tools, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(snapshot.Secrets, func(a, b SecretRequirement) int {
		if result := strings.Compare(a.Capability, b.Capability); result != 0 {
			return result
		}
		return strings.Compare(a.Target, b.Target)
	})
	if len(snapshot.Tools) == 0 {
		snapshot.Tools = nil
	}
	if len(snapshot.Secrets) == 0 {
		snapshot.Secrets = nil
	}
	for index := range snapshot.Tools {
		if len(snapshot.Tools[index].Probe.Args) == 0 {
			snapshot.Tools[index].Probe.Args = nil
		}
	}
	return nil
}

func validateOCI(snapshot *Snapshot, limits Limits) error {
	if !validDigestImage(snapshot.Runtime.Image) {
		return errors.New("OCI worker image must use an immutable sha256 digest")
	}
	if !platformPattern.MatchString(snapshot.Runtime.Platform) {
		return errors.New("OCI worker platform must be explicit Linux architecture")
	}
	if snapshot.Runtime.Network != NetworkNone && snapshot.Runtime.Network != NetworkOutbound {
		return errors.New("OCI worker network mode is unsupported")
	}
	if !snapshot.Runtime.ReadOnlyRoot {
		return errors.New("OCI worker root filesystem must be read-only")
	}
	resources := snapshot.Resources
	if resources.CPUMillis <= 0 || resources.CPUMillis > limits.MaxCPUMillis ||
		resources.MemoryBytes <= 0 || resources.MemoryBytes > limits.MaxMemoryBytes ||
		resources.PIDs <= 0 || resources.PIDs > limits.MaxPIDs {
		return errors.New("OCI worker resources are outside operator limits")
	}
	return nil
}

func validDigestImage(image string) bool {
	name, digest, ok := strings.Cut(image, "@sha256:")
	if !ok || name == "" || strings.Contains(name, "@") || len(digest) != 64 || strings.Contains(digest, "@") {
		return false
	}
	for _, character := range digest {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	lastSlash := strings.LastIndexByte(name, '/')
	if lastColon := strings.LastIndexByte(name, ':'); lastColon > lastSlash {
		return false
	}
	return !strings.ContainsAny(name, " \t\r\n")
}

func validateTools(snapshot *Snapshot) error {
	seen := make(map[string]struct{}, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		if !identifierPattern.MatchString(tool.Name) || !versionPattern.MatchString(tool.Version) {
			return errors.New("invalid worker tool declaration")
		}
		if _, duplicate := seen[tool.Name]; duplicate {
			return errors.New("duplicate worker tool declaration")
		}
		seen[tool.Name] = struct{}{}
		if !executablePattern.MatchString(tool.Probe.Executable) || tool.Probe.OutputContains == "" ||
			len(tool.Probe.OutputContains) > 4096 || strings.IndexByte(tool.Probe.OutputContains, 0) >= 0 {
			return errors.New("invalid worker tool probe")
		}
		if len(tool.Probe.Args) > 128 {
			return errors.New("worker tool probe has too many arguments")
		}
		for _, argument := range tool.Probe.Args {
			if len(argument) > 4096 || strings.IndexByte(argument, 0) >= 0 {
				return errors.New("invalid worker tool probe argument")
			}
		}
	}
	return nil
}

func validateSecrets(snapshot *Snapshot) error {
	capabilities := make(map[string]struct{}, len(snapshot.Secrets))
	for index, requirement := range snapshot.Secrets {
		if !validCapability(requirement.Capability) {
			return errors.New("invalid or reserved secret capability")
		}
		if _, duplicate := capabilities[requirement.Capability]; duplicate {
			return errors.New("duplicate secret capability")
		}
		capabilities[requirement.Capability] = struct{}{}
		if requirement.BindingRevision == 0 {
			return errors.New("secret binding revision must be positive")
		}
		if requirement.Stage != StageAgent || !requirement.Required {
			return errors.New("secret requirements must be required and agent-only")
		}
		switch requirement.Delivery {
		case DeliveryFile, DeliveryDirectory:
			if !validSecretPath(requirement.Target) {
				return errors.New("invalid secret delivery path")
			}
		case DeliveryEnvironment:
			if !validEnvironmentTarget(requirement.Target) {
				return errors.New("invalid secret environment target")
			}
		default:
			return errors.New("unsupported secret delivery")
		}
		for prior := range index {
			if targetsOverlap(snapshot.Secrets[prior], requirement) {
				return errors.New("secret delivery targets overlap")
			}
		}
	}
	return nil
}

func validCapability(capability string) bool {
	parts := strings.Split(capability, ".")
	if len(parts) < 2 {
		return false
	}
	if _, reserved := reservedCapabilityNamespaces[parts[0]]; reserved {
		return false
	}
	for _, part := range parts {
		if !capabilityPart.MatchString(part) {
			return false
		}
	}
	return true
}

func validSecretPath(target string) bool {
	if target == "" || !strings.HasPrefix(target, "/") || path.Clean(target) != target {
		return false
	}
	return strings.HasPrefix(target, secretRoot+"/")
}

func validEnvironmentTarget(target string) bool {
	if !environmentPattern.MatchString(target) {
		return false
	}
	if _, reserved := reservedEnvironmentKeys[target]; reserved {
		return false
	}
	for _, prefix := range []string{
		"PAJE_", "HATCHET_", "MEM0_", "SUBMISSION_", "PUBLISHER_",
		"GIT_", "SSH_", "DOCKER_", "REGISTRY_", "EXECUTOR_",
	} {
		if strings.HasPrefix(target, prefix) {
			return false
		}
	}
	return true
}

func targetsOverlap(first, second SecretRequirement) bool {
	if first.Delivery == DeliveryEnvironment || second.Delivery == DeliveryEnvironment {
		return first.Delivery == DeliveryEnvironment && second.Delivery == DeliveryEnvironment && first.Target == second.Target
	}
	return first.Target == second.Target || strings.HasPrefix(first.Target, second.Target+"/") ||
		strings.HasPrefix(second.Target, first.Target+"/")
}
