// Package artifact defines immutable, content-addressed workflow artifacts.
package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unsafe"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/verification"
)

var (
	// ErrTooLarge indicates an artifact exceeded a configured storage bound.
	ErrTooLarge = errors.New("artifact too large")
	// ErrDigestMismatch indicates untrusted artifact bytes do not match their reference.
	ErrDigestMismatch = errors.New("artifact digest mismatch")
	// ErrInvalidReference indicates a reference cannot be used safely.
	ErrInvalidReference = errors.New("invalid artifact reference")
	// ErrInvalidBundle indicates a bundle cannot be canonically encoded or verified.
	ErrInvalidBundle = errors.New("invalid artifact bundle")
)

// Reference identifies an immutable artifact. Digest and Size are over the
// uncompressed canonical tar stream, so compression does not change identity.
type Reference struct {
	RunID  string `json:"run_id"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Manifest is the metadata authenticated alongside a bundle's evidence.
type Manifest struct {
	SchemaVersion int         `json:"schema_version"`
	RunID         string      `json:"run_id"`
	Template      template.ID `json:"template"`
	Repository    string      `json:"repository"`
	BaseSHA       string      `json:"base_sha"`
	TreeSHA       string      `json:"tree_sha"`
	Changes       []Change    `json:"changes"`
	Members       []Member    `json:"members"`
	MemoryIDs     []string    `json:"memory_ids,omitempty"`
	MemoryCount   int         `json:"memory_count"`
}

// Change records one Git path and mode transition.
type Change struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status"`
	OldMode string `json:"old_mode,omitempty"`
	NewMode string `json:"new_mode,omitempty"`
}

// Member authenticates one non-manifest archive member.
type Member struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Bundle is the fixed artifact payload.
type Bundle struct {
	Manifest          Manifest              `json:"manifest"`
	ChangesPatch      []byte                `json:"-"`
	AgentOutput       []byte                `json:"-"`
	ExecutionMetadata json.RawMessage       `json:"execution_metadata"`
	Verification      []verification.Result `json:"verification"`
	Preflight         map[string]string     `json:"preflight"`
	Warnings          []string              `json:"warnings"`
}

// ExecutionEvidence is the only execution metadata permitted in an artifact.
// Transcript and output are deliberately stored in their dedicated members.
type ExecutionEvidence struct {
	// The original generic process fields remain wire-compatible while Tasks
	// 10-11 migrate independent artifact fixtures. code-change@v1 now also
	// binds these fields to the exact portable runtime evidence below.
	ExitCode  int     `json:"exit_code"`
	Duration  float64 `json:"duration"`
	Started   bool    `json:"started"`
	Completed bool    `json:"completed"`
	Truncated bool    `json:"truncated"`

	Profile                     *WorkerProfileEvidence `json:"profile,omitempty"`
	Runtime                     *RuntimeEvidence       `json:"runtime,omitempty"`
	Harness                     *HarnessEvidence       `json:"harness,omitempty"`
	Tools                       *ToolEvidenceList      `json:"tools,omitempty"`
	Attempts                    *AttemptEvidenceList   `json:"attempts,omitempty"`
	AgentEnvironmentKeys        *EnvironmentKeyList    `json:"agent_environment_keys,omitempty"`
	VerificationEnvironmentKeys *EnvironmentKeyList    `json:"verification_environment_keys,omitempty"`
}

// Pointer-backed named lists preserve ExecutionEvidence comparability for
// legacy callers while retaining array-shaped portable JSON evidence.
type ToolEvidenceList []ToolEvidence
type AttemptEvidenceList []AttemptEvidence
type EnvironmentKeyList []string

// WorkerProfileEvidence binds execution to one immutable operator profile.
type WorkerProfileEvidence struct {
	Name     string `json:"name"`
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

// RuntimeEvidence contains only provider-neutral facts. ImageDigest excludes
// the repository name and any registry/provider metadata.
type RuntimeEvidence struct {
	Kind        string `json:"kind"`
	ImageDigest string `json:"image_digest,omitempty"`
	Platform    string `json:"platform,omitempty"`
	Isolated    bool   `json:"isolated"`
	Certified   bool   `json:"certified"`
}

type HarnessEvidence struct {
	ID              string `json:"id"`
	DeclaredVersion string `json:"declared_version"`
	ProbedVersion   string `json:"probed_version"`
	ProbePassed     bool   `json:"probe_passed"`
	Sequence        int    `json:"sequence"`
}

type ToolEvidence struct {
	Name            string `json:"name"`
	DeclaredVersion string `json:"declared_version"`
	ProbedVersion   string `json:"probed_version"`
	ProbePassed     bool   `json:"probe_passed"`
	Sequence        int    `json:"sequence"`
}

// AttemptEvidence records bounded lifecycle metadata without output,
// provider handles, host paths, or raw diagnostics.
type AttemptEvidence struct {
	ID        executor.AttemptID `json:"id"`
	Created   bool               `json:"created"`
	Started   bool               `json:"started"`
	Completed bool               `json:"completed"`
	Canceled  bool               `json:"canceled"`
	Destroyed bool               `json:"destroyed"`
	ExitCode  int                `json:"exit_code"`
	Duration  float64            `json:"duration"`
	Truncated bool               `json:"truncated"`
}

// ExecutionEvidenceFrom converts Task 1's safe process outcome into artifact
// metadata without carrying transcript or terminal output.
func ExecutionEvidenceFrom(result runner.ExecutionResult) ExecutionEvidence {
	return ExecutionEvidence{ExitCode: result.ExitCode, Duration: result.Duration, Started: result.Started, Completed: result.Completed, Truncated: result.Truncated}
}

// Store persists and verifies immutable bundles.
type Store interface {
	Save(context.Context, Bundle) (Reference, error)
	Load(context.Context, Reference) (Bundle, error)
}

var memberNames = []string{
	"manifest.json", "changes.patch", "agent-output.txt", "execution.json",
	"verification.json", "preflight.json", "warnings.json",
}

// ReferenceFor returns the stable logical reference for bundle.
func ReferenceFor(bundle Bundle) (Reference, error) {
	_, _, reference, err := Canonicalize(bundle)
	return reference, err
}

// ReferenceForTar returns the logical reference for an already canonical tar
// stream. Filesystem adapters use it while verifying stored bytes.
func ReferenceForTar(canonicalTar []byte) (Reference, error) {
	bundle, err := DecodeCanonicalTar(canonicalTar)
	if err != nil {
		return Reference{}, err
	}
	return Reference{RunID: bundle.Manifest.RunID, Digest: DigestTar(canonicalTar), Size: int64(len(canonicalTar))}, nil
}

// DigestTar returns the SHA-256 identity of an uncompressed canonical tar stream.
func DigestTar(canonicalTar []byte) string {
	sum := sha256.Sum256(canonicalTar)
	return hex.EncodeToString(sum[:])
}

// Canonicalize returns the normalized bundle, its canonical tar stream, and
// its logical reference. It is shared by all artifact stores.
func Canonicalize(bundle Bundle) (Bundle, []byte, Reference, error) {
	return CanonicalizeLimited(context.Background(), bundle, 0)
}

// CanonicalizeLimited canonicalizes bundle while respecting ctx and maxTarBytes.
// A non-positive limit leaves the canonical stream unbounded.
func CanonicalizeLimited(ctx context.Context, bundle Bundle, maxTarBytes int64) (Bundle, []byte, Reference, error) {
	if err := ctx.Err(); err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	if err := preflightBundleSize(ctx, bundle, maxTarBytes); err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	normalized, err := normalizeBundleContext(ctx, bundle)
	if err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	payloads, err := encodePayloadsContext(ctx, normalized)
	if err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	members, err := membersForContext(ctx, payloads)
	if err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	normalized.Manifest.Members = members
	if err := ctx.Err(); err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	manifest, err := stableJSON(normalized.Manifest)
	if err != nil {
		return Bundle{}, nil, Reference{}, fmt.Errorf("%w: encode manifest: %v", ErrInvalidBundle, err)
	}
	payloads["manifest.json"] = manifest
	if err := ctx.Err(); err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	tarBytes, err := writeTar(ctx, payloads, maxTarBytes)
	if err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	if err := ctx.Err(); err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	return normalized, tarBytes, Reference{RunID: normalized.Manifest.RunID, Digest: DigestTar(tarBytes), Size: int64(len(tarBytes))}, nil
}

// preflightBundleSize rejects hostile payloads before CloneBundle duplicates
// their byte slices. The exact tar framing bound is enforced by writeTar.
func preflightBundleSize(ctx context.Context, bundle Bundle, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if limit <= 0 {
		return nil
	}
	remaining := limit
	consume := func(size int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if size < 0 || size > remaining {
			return ErrTooLarge
		}
		remaining -= size
		return nil
	}
	consumeCount := func(count int, elementSize uintptr) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := int64(elementSize)
		if count < 0 || size <= 0 || int64(count) > remaining/size {
			return ErrTooLarge
		}
		remaining -= int64(count) * size
		return nil
	}
	consumeString := func(value string) error {
		const maxJSONExpansion = 6
		if err := ctx.Err(); err != nil {
			return err
		}
		length := int64(len(value))
		if length > remaining/maxJSONExpansion {
			return ErrTooLarge
		}
		return consume(length * maxJSONExpansion)
	}
	consumeStrings := func(values ...string) error {
		for _, value := range values {
			if err := consumeString(value); err != nil {
				return err
			}
		}
		return nil
	}
	const minimumCanonicalTarBytes = 10 * 1024
	if err := consume(minimumCanonicalTarBytes); err != nil {
		return err
	}
	for _, data := range [][]byte{bundle.ChangesPatch, bundle.AgentOutput, bundle.ExecutionMetadata} {
		if err := consume(int64(len(data))); err != nil {
			return err
		}
	}
	if err := consumeStrings(
		bundle.Manifest.RunID,
		bundle.Manifest.Template.Name,
		bundle.Manifest.Repository,
		bundle.Manifest.BaseSHA,
		bundle.Manifest.TreeSHA,
	); err != nil {
		return err
	}
	if err := consumeCount(len(bundle.Manifest.Changes), unsafe.Sizeof(Change{})); err != nil {
		return err
	}
	for _, change := range bundle.Manifest.Changes {
		if err := consumeStrings(change.Path, change.OldPath, change.Status, change.OldMode, change.NewMode); err != nil {
			return err
		}
	}
	if err := consumeCount(len(bundle.Manifest.Members), unsafe.Sizeof(Member{})); err != nil {
		return err
	}
	for _, member := range bundle.Manifest.Members {
		if err := consumeStrings(member.Name, member.SHA256); err != nil {
			return err
		}
	}
	for _, values := range [][]string{bundle.Manifest.MemoryIDs, bundle.Warnings} {
		if err := consumeCount(len(values), unsafe.Sizeof("")); err != nil {
			return err
		}
		for _, value := range values {
			if err := consumeString(value); err != nil {
				return err
			}
		}
	}
	if err := consumeCount(len(bundle.Verification), unsafe.Sizeof(verification.Result{})); err != nil {
		return err
	}
	for _, result := range bundle.Verification {
		if err := consumeStrings(
			result.Command.Name,
			result.Command.Directory,
			result.Command.Executable,
			result.Output,
			result.FailureClass,
			result.CauseCode,
		); err != nil {
			return err
		}
		if err := consumeCount(len(result.Command.Args), unsafe.Sizeof("")); err != nil {
			return err
		}
		for _, arg := range result.Command.Args {
			if err := consumeString(arg); err != nil {
				return err
			}
		}
		if err := consumeCount(len(result.Command.Environment), 64); err != nil {
			return err
		}
		for key, value := range result.Command.Environment {
			if err := consumeStrings(key, value); err != nil {
				return err
			}
		}
	}
	if err := consumeCount(len(bundle.Preflight), 64); err != nil {
		return err
	}
	for key, value := range bundle.Preflight {
		if err := consumeStrings(key, value); err != nil {
			return err
		}
	}
	return nil
}

// DecodeCanonicalTar verifies the fixed member set and reconstructs a bundle.
// Callers must validate the enclosing digest and size separately.
func DecodeCanonicalTar(canonicalTar []byte) (Bundle, error) {
	reader := tar.NewReader(bytes.NewReader(canonicalTar))
	members := make(map[string][]byte, len(memberNames))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Bundle{}, fmt.Errorf("%w: read tar: %v", ErrDigestMismatch, err)
		}
		if !isMemberName(header.Name) || header.Typeflag != tar.TypeReg || header.Mode != 0o600 || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || !header.ModTime.Equal(time.Unix(0, 0).UTC()) {
			return Bundle{}, fmt.Errorf("%w: non-canonical tar member", ErrDigestMismatch)
		}
		if _, exists := members[header.Name]; exists {
			return Bundle{}, fmt.Errorf("%w: duplicate member %q", ErrDigestMismatch, header.Name)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return Bundle{}, fmt.Errorf("%w: read member %q: %v", ErrDigestMismatch, header.Name, err)
		}
		members[header.Name] = data
	}
	for _, name := range memberNames {
		if _, ok := members[name]; !ok {
			return Bundle{}, fmt.Errorf("%w: missing member %q", ErrDigestMismatch, name)
		}
	}

	var bundle Bundle
	if err := json.Unmarshal(members["manifest.json"], &bundle.Manifest); err != nil {
		return Bundle{}, fmt.Errorf("%w: decode manifest: %v", ErrDigestMismatch, err)
	}
	bundle.ChangesPatch = append([]byte(nil), members["changes.patch"]...)
	bundle.AgentOutput = append([]byte(nil), members["agent-output.txt"]...)
	bundle.ExecutionMetadata = append(json.RawMessage(nil), members["execution.json"]...)
	if err := json.Unmarshal(members["verification.json"], &bundle.Verification); err != nil {
		return Bundle{}, fmt.Errorf("%w: decode verification: %v", ErrDigestMismatch, err)
	}
	var preflight []keyValue
	if err := json.Unmarshal(members["preflight.json"], &preflight); err != nil {
		return Bundle{}, fmt.Errorf("%w: decode preflight: %v", ErrDigestMismatch, err)
	}
	bundle.Preflight = make(map[string]string, len(preflight))
	for _, entry := range preflight {
		if _, exists := bundle.Preflight[entry.Key]; exists {
			return Bundle{}, fmt.Errorf("%w: duplicate preflight key", ErrDigestMismatch)
		}
		bundle.Preflight[entry.Key] = entry.Value
	}
	if err := json.Unmarshal(members["warnings.json"], &bundle.Warnings); err != nil {
		return Bundle{}, fmt.Errorf("%w: decode warnings: %v", ErrDigestMismatch, err)
	}
	if err := validateDecoded(bundle, members); err != nil {
		return Bundle{}, err
	}
	normalized, reencoded, _, err := Canonicalize(bundle)
	if err != nil || !bytes.Equal(reencoded, canonicalTar) {
		return Bundle{}, fmt.Errorf("%w: tar is not canonical", ErrDigestMismatch)
	}
	return CloneBundle(normalized), nil
}

func normalizeBundle(bundle Bundle) (Bundle, error) {
	return normalizeBundleContext(context.Background(), bundle)
}

func normalizeBundleContext(ctx context.Context, bundle Bundle) (Bundle, error) {
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	bundle = CloneBundle(bundle)
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	bundle.Manifest.Changes = nonNil(bundle.Manifest.Changes)
	bundle.Manifest.MemoryIDs = nonNil(bundle.Manifest.MemoryIDs)
	bundle.Verification = nonNil(bundle.Verification)
	bundle.Warnings = nonNil(bundle.Warnings)
	if strings.TrimSpace(bundle.Manifest.RunID) == "" || strings.TrimSpace(bundle.Manifest.Template.Name) == "" || bundle.Manifest.Template.Version <= 0 || strings.TrimSpace(bundle.Manifest.Repository) == "" || strings.TrimSpace(bundle.Manifest.BaseSHA) == "" || strings.TrimSpace(bundle.Manifest.TreeSHA) == "" {
		return Bundle{}, fmt.Errorf("%w: manifest identity fields are required", ErrInvalidBundle)
	}
	if bundle.Manifest.SchemaVersion != 0 && bundle.Manifest.SchemaVersion != 1 {
		return Bundle{}, fmt.Errorf("%w: unsupported schema version", ErrInvalidBundle)
	}
	bundle.Manifest.SchemaVersion = 1
	if bundle.Manifest.MemoryCount < 0 || bundle.Manifest.MemoryCount != len(bundle.Manifest.MemoryIDs) {
		return Bundle{}, fmt.Errorf("%w: memory count does not match memory IDs", ErrInvalidBundle)
	}
	if err := sortUniqueStrings(bundle.Manifest.MemoryIDs); err != nil {
		return Bundle{}, fmt.Errorf("%w: memory IDs: %v", ErrInvalidBundle, err)
	}
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	sort.Slice(bundle.Manifest.Changes, func(i, j int) bool {
		return changeKey(bundle.Manifest.Changes[i]) < changeKey(bundle.Manifest.Changes[j])
	})
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	sort.Strings(bundle.Warnings)
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	for index := range bundle.Verification {
		if err := ctx.Err(); err != nil {
			return Bundle{}, err
		}
		// Task 5 permits an execution-only override, never serializable evidence.
		bundle.Verification[index].Command.Environment = nil
		bundle.Verification[index].Command.Args = nonNil(bundle.Verification[index].Command.Args)
	}
	if len(bundle.ExecutionMetadata) == 0 {
		return Bundle{}, fmt.Errorf("%w: execution metadata is required", ErrInvalidBundle)
	} else {
		canonical, err := canonicalExecutionMetadata(bundle.ExecutionMetadata)
		if err != nil {
			return Bundle{}, fmt.Errorf("%w: execution metadata: %v", ErrInvalidBundle, err)
		}
		bundle.ExecutionMetadata = canonical
	}
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	bundle.Manifest.Members = nil
	return bundle, nil
}

func encodePayloadsContext(ctx context.Context, bundle Bundle) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	verificationData, err := stableJSON(bundle.Verification)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	preflightData, err := stableJSON(sortedPreflight(bundle.Preflight))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	warningsData, err := stableJSON(bundle.Warnings)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payloads := map[string][]byte{
		"changes.patch":     append([]byte(nil), bundle.ChangesPatch...),
		"agent-output.txt":  append([]byte(nil), bundle.AgentOutput...),
		"execution.json":    append([]byte(nil), bundle.ExecutionMetadata...),
		"verification.json": verificationData,
		"preflight.json":    preflightData,
		"warnings.json":     warningsData,
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return payloads, nil
}

func membersForContext(ctx context.Context, payloads map[string][]byte) ([]Member, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(memberNames)-1)
	for _, name := range memberNames[1:] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data := payloads[name]
		sum := sha256.Sum256(data)
		members = append(members, Member{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})
	}
	return members, nil
}

func writeTar(ctx context.Context, payloads map[string][]byte, limit int64) ([]byte, error) {
	out := &boundedBuffer{limit: limit}
	writer := tar.NewWriter(out)
	for _, name := range memberNames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, ok := payloads[name]
		if !ok {
			return nil, fmt.Errorf("%w: missing generated member %q", ErrInvalidBundle, name)
		}
		header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}
		if err := writer.WriteHeader(header); err != nil {
			if errors.Is(err, ErrTooLarge) {
				return nil, ErrTooLarge
			}
			return nil, fmt.Errorf("%w: write tar header: %v", ErrInvalidBundle, err)
		}
		if err := writeTarData(ctx, writer, data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, ErrTooLarge
		}
		return nil, fmt.Errorf("%w: close tar: %v", ErrInvalidBundle, err)
	}
	return out.Bytes(), nil
}

func writeTarData(ctx context.Context, writer *tar.Writer, data []byte) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := len(data)
		if size > 32*1024 {
			size = 32 * 1024
		}
		if _, err := writer.Write(data[:size]); err != nil {
			if errors.Is(err, ErrTooLarge) {
				return ErrTooLarge
			}
			return fmt.Errorf("%w: write tar member: %v", ErrInvalidBundle, err)
		}
		data = data[size:]
	}
	return nil
}

func validateDecoded(bundle Bundle, members map[string][]byte) error {
	if bundle.Manifest.SchemaVersion != 1 || bundle.Manifest.MemoryCount < 0 || bundle.Manifest.MemoryCount != len(bundle.Manifest.MemoryIDs) {
		return fmt.Errorf("%w: invalid manifest", ErrDigestMismatch)
	}
	if len(bundle.Manifest.Members) != len(memberNames)-1 {
		return fmt.Errorf("%w: invalid manifest member list", ErrDigestMismatch)
	}
	for index, expected := range memberNames[1:] {
		member := bundle.Manifest.Members[index]
		if member.Name != expected || member.Size != int64(len(members[expected])) || !validDigest(member.SHA256) {
			return fmt.Errorf("%w: invalid member %q", ErrDigestMismatch, expected)
		}
		sum := sha256.Sum256(members[expected])
		if member.SHA256 != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("%w: member %q", ErrDigestMismatch, expected)
		}
	}
	if _, err := normalizeBundle(bundle); err != nil {
		return fmt.Errorf("%w: %v", ErrDigestMismatch, err)
	}
	return nil
}

func stableJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(encoded)
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	var out bytes.Buffer
	if err := writeJSON(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var canonicalNumber = regexp.MustCompile(`^(0|[1-9][0-9]*|-[1-9][0-9]*)(\.[0-9]*[1-9])?$`)

func canonicalExecutionMetadata(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value ExecutionEvidence
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("multiple JSON values")
	}
	if err := validateExecutionEvidence(value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err = canonicalJSON(canonical)
	if err != nil {
		return nil, err
	}
	input, err := canonicalJSON(raw)
	if err != nil || !bytes.Equal(input, canonical) {
		return nil, fmt.Errorf("execution metadata is not canonical")
	}
	return canonical, nil
}

var (
	evidenceNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	evidenceVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	evidenceEnvKeyPattern  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	evidencePlatform       = regexp.MustCompile(`^linux/[a-z0-9][a-z0-9_-]{0,31}$`)
)

func validateExecutionEvidence(value ExecutionEvidence) error {
	portable := value.Profile != nil || value.Runtime != nil || value.Harness != nil ||
		value.Tools != nil || value.Attempts != nil ||
		value.AgentEnvironmentKeys != nil || value.VerificationEnvironmentKeys != nil
	if !portable {
		if value.Duration < 0 {
			return errors.New("legacy execution duration is invalid")
		}
		return nil
	}
	if value.Profile == nil || value.Runtime == nil || value.Harness == nil {
		return errors.New("portable execution identity is incomplete")
	}
	if !evidenceNamePattern.MatchString(value.Profile.Name) || value.Profile.Revision == 0 ||
		!validDigest(value.Profile.Digest) {
		return errors.New("portable worker profile evidence is invalid")
	}
	switch value.Runtime.Kind {
	case "oci":
		if !validDigest(value.Runtime.ImageDigest) || !evidencePlatform.MatchString(value.Runtime.Platform) ||
			!value.Runtime.Isolated {
			return errors.New("portable OCI runtime evidence is invalid")
		}
	case "host":
		if value.Runtime.ImageDigest != "" || value.Runtime.Platform != "" ||
			value.Runtime.Isolated || value.Runtime.Certified {
			return errors.New("portable host runtime evidence is invalid")
		}
	default:
		return errors.New("portable runtime kind is invalid")
	}
	if !evidenceNamePattern.MatchString(value.Harness.ID) ||
		!evidenceVersionPattern.MatchString(value.Harness.DeclaredVersion) ||
		value.Harness.DeclaredVersion != value.Harness.ProbedVersion ||
		!value.Harness.ProbePassed || value.Harness.Sequence < 0 {
		return errors.New("portable harness evidence is invalid")
	}
	var tools ToolEvidenceList
	if value.Tools != nil {
		tools = *value.Tools
	}
	toolNames := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if !evidenceNamePattern.MatchString(tool.Name) ||
			!evidenceVersionPattern.MatchString(tool.DeclaredVersion) ||
			tool.DeclaredVersion != tool.ProbedVersion || !tool.ProbePassed || tool.Sequence < 0 {
			return errors.New("portable tool evidence is invalid")
		}
		if _, duplicate := toolNames[tool.Name]; duplicate {
			return errors.New("portable tool evidence is duplicated")
		}
		toolNames[tool.Name] = struct{}{}
	}
	var attemptList AttemptEvidenceList
	if value.Attempts != nil {
		attemptList = *value.Attempts
	}
	attempts := make(map[string]struct{}, len(attemptList))
	agentFound := false
	for _, attempt := range attemptList {
		if err := attempt.ID.Validate(); err != nil || attempt.Duration < 0 ||
			attempt.Started && !attempt.Created || attempt.Completed && !attempt.Started {
			return errors.New("portable attempt evidence is invalid")
		}
		key := attempt.ID.Key()
		if _, duplicate := attempts[key]; duplicate {
			return errors.New("portable attempt evidence is duplicated")
		}
		attempts[key] = struct{}{}
		if attempt.ID.Purpose == executor.PurposeAgent {
			if agentFound || attempt.ID.Sequence != 0 {
				return errors.New("portable agent attempt evidence is invalid")
			}
			agentFound = true
			if value.ExitCode != attempt.ExitCode || value.Started != attempt.Started ||
				value.Completed != attempt.Completed || value.Truncated != attempt.Truncated {
				return errors.New("portable agent evidence does not match generic execution fields")
			}
		}
	}
	if !agentFound {
		return errors.New("portable agent attempt evidence is missing")
	}
	var agentKeys, verificationKeys EnvironmentKeyList
	if value.AgentEnvironmentKeys != nil {
		agentKeys = *value.AgentEnvironmentKeys
	}
	if value.VerificationEnvironmentKeys != nil {
		verificationKeys = *value.VerificationEnvironmentKeys
	}
	for _, keys := range []EnvironmentKeyList{agentKeys, verificationKeys} {
		previous := ""
		for _, key := range keys {
			if !evidenceEnvKeyPattern.MatchString(key) || (previous != "" && key <= previous) {
				return errors.New("portable environment key evidence is invalid")
			}
			previous = key
		}
	}
	return nil
}

func writeJSON(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil, bool, string, json.Number:
		if number, ok := typed.(json.Number); ok && !canonicalNumber.MatchString(number.String()) {
			return fmt.Errorf("non-canonical number %q", number)
		}
		data, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		out.Write(data)
	case []any:
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			keyJSON, _ := json.Marshal(key)
			out.Write(keyJSON)
			out.WriteByte(':')
			if err := writeJSON(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", value)
	}
	return nil
}

type keyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func sortedPreflight(values map[string]string) []keyValue {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]keyValue, 0, len(keys))
	for _, key := range keys {
		result = append(result, keyValue{Key: key, Value: values[key]})
	}
	return result
}
func sortUniqueStrings(values []string) error {
	sort.Strings(values)
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			return fmt.Errorf("duplicate %q", values[i])
		}
	}
	return nil
}
func changeKey(change Change) string {
	return change.Path + "\x00" + change.OldPath + "\x00" + change.Status + "\x00" + change.OldMode + "\x00" + change.NewMode
}
func isMemberName(name string) bool {
	for _, candidate := range memberNames {
		if name == candidate {
			return true
		}
	}
	return false
}
func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

// ValidDigest reports whether value is a safe lowercase SHA-256 digest.
func ValidDigest(value string) bool { return validDigest(value) }

// CloneBundle returns a fully independent bundle copy.
func CloneBundle(bundle Bundle) Bundle {
	clone := bundle
	clone.Manifest.Changes = append([]Change(nil), bundle.Manifest.Changes...)
	clone.Manifest.Members = append([]Member(nil), bundle.Manifest.Members...)
	clone.Manifest.MemoryIDs = append([]string(nil), bundle.Manifest.MemoryIDs...)
	clone.ChangesPatch = append([]byte(nil), bundle.ChangesPatch...)
	clone.AgentOutput = append([]byte(nil), bundle.AgentOutput...)
	clone.ExecutionMetadata = append(json.RawMessage(nil), bundle.ExecutionMetadata...)
	clone.Verification = append([]verification.Result(nil), bundle.Verification...)
	for index := range clone.Verification {
		clone.Verification[index].Command.Args = append([]string(nil), bundle.Verification[index].Command.Args...)
		clone.Verification[index].Command.Environment = cloneStringMap(bundle.Verification[index].Command.Environment)
	}
	clone.Preflight = cloneStringMap(bundle.Preflight)
	clone.Warnings = append([]string(nil), bundle.Warnings...)
	return clone
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

type boundedBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if b.limit > 0 && !withinLimit(int64(b.Len()), int64(len(data)), b.limit) {
		return 0, ErrTooLarge
	}
	return b.Buffer.Write(data)
}

func withinLimit(used, added, limit int64) bool {
	return used >= 0 && added >= 0 && limit >= 0 && used <= limit && added <= limit-used
}
