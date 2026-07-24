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
	"sort"
	"strings"
	"time"

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
	sum := sha256.Sum256(canonicalTar)
	return Reference{RunID: bundle.Manifest.RunID, Digest: hex.EncodeToString(sum[:]), Size: int64(len(canonicalTar))}, nil
}

// Canonicalize returns the normalized bundle, its canonical tar stream, and
// its logical reference. It is shared by all artifact stores.
func Canonicalize(bundle Bundle) (Bundle, []byte, Reference, error) {
	normalized, err := normalizeBundle(bundle)
	if err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	payloads, err := encodePayloads(normalized)
	if err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	normalized.Manifest.Members = membersFor(payloads)
	manifest, err := stableJSON(normalized.Manifest)
	if err != nil {
		return Bundle{}, nil, Reference{}, fmt.Errorf("%w: encode manifest: %v", ErrInvalidBundle, err)
	}
	payloads["manifest.json"] = manifest
	tarBytes, err := writeTar(payloads)
	if err != nil {
		return Bundle{}, nil, Reference{}, err
	}
	sum := sha256.Sum256(tarBytes)
	return normalized, tarBytes, Reference{RunID: normalized.Manifest.RunID, Digest: hex.EncodeToString(sum[:]), Size: int64(len(tarBytes))}, nil
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
	bundle = CloneBundle(bundle)
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
	sort.Slice(bundle.Manifest.Changes, func(i, j int) bool {
		return changeKey(bundle.Manifest.Changes[i]) < changeKey(bundle.Manifest.Changes[j])
	})
	sort.Strings(bundle.Warnings)
	for index := range bundle.Verification {
		// Task 5 permits an execution-only override, never serializable evidence.
		bundle.Verification[index].Command.Environment = nil
	}
	if len(bundle.ExecutionMetadata) == 0 {
		bundle.ExecutionMetadata = json.RawMessage("null")
	} else {
		canonical, err := canonicalJSON(bundle.ExecutionMetadata)
		if err != nil {
			return Bundle{}, fmt.Errorf("%w: execution metadata: %v", ErrInvalidBundle, err)
		}
		bundle.ExecutionMetadata = canonical
	}
	bundle.Manifest.Members = nil
	return bundle, nil
}

func encodePayloads(bundle Bundle) (map[string][]byte, error) {
	verificationData, err := stableJSON(bundle.Verification)
	if err != nil {
		return nil, err
	}
	preflightData, err := stableJSON(sortedPreflight(bundle.Preflight))
	if err != nil {
		return nil, err
	}
	warningsData, err := stableJSON(bundle.Warnings)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"changes.patch":     append([]byte(nil), bundle.ChangesPatch...),
		"agent-output.txt":  append([]byte(nil), bundle.AgentOutput...),
		"execution.json":    append([]byte(nil), bundle.ExecutionMetadata...),
		"verification.json": verificationData,
		"preflight.json":    preflightData,
		"warnings.json":     warningsData,
	}, nil
}

func membersFor(payloads map[string][]byte) []Member {
	members := make([]Member, 0, len(memberNames)-1)
	for _, name := range memberNames[1:] {
		data := payloads[name]
		sum := sha256.Sum256(data)
		members = append(members, Member{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})
	}
	return members
}

func writeTar(payloads map[string][]byte) ([]byte, error) {
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	for _, name := range memberNames {
		data, ok := payloads[name]
		if !ok {
			return nil, fmt.Errorf("%w: missing generated member %q", ErrInvalidBundle, name)
		}
		header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}
		if err := writer.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("%w: write tar header: %v", ErrInvalidBundle, err)
		}
		if _, err := writer.Write(data); err != nil {
			return nil, fmt.Errorf("%w: write tar member: %v", ErrInvalidBundle, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("%w: close tar: %v", ErrInvalidBundle, err)
	}
	return out.Bytes(), nil
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

func writeJSON(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil, bool, string, json.Number:
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
