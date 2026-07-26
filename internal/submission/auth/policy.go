package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/araihu/paje/internal/submission"
	"golang.org/x/sys/unix"
)

const (
	policySchemaVersion = 1
	maxPolicyBytes      = 1 << 20
)

const (
	ActionSubmitArtifact    = "submit:artifact"
	ActionSubmitPullRequest = "submit:pull_request"
	ActionRead              = "read"
	ActionCancel            = "cancel"
	ActionControlCreate     = "control:create"
	ActionTaskCreate        = "task:create"
	ActionWorkDispatch      = "work:dispatch"
	ActionWorkObserve       = "work:observe"
	ActionWorkSend          = "work:send"
	ActionWorkWait          = "work:wait"
	ActionWorkInterrupt     = "work:interrupt"
	ActionWorkClose         = "work:close"
	ActionEvidenceWrite     = "evidence:write"
	ActionControlClose      = "control:close"
)

var (
	knownActions = map[string]submission.Action{
		ActionSubmitArtifact:    submission.ActionSubmitArtifact,
		ActionSubmitPullRequest: submission.ActionSubmitPullRequest,
		ActionRead:              submission.ActionRead,
		ActionCancel:            submission.ActionCancel,
		ActionControlCreate:     "",
		ActionTaskCreate:        "",
		ActionWorkDispatch:      "",
		ActionWorkObserve:       "",
		ActionWorkSend:          "",
		ActionWorkWait:          "",
		ActionWorkInterrupt:     "",
		ActionWorkClose:         "",
		ActionEvidenceWrite:     "",
		ActionControlClose:      "",
	}
	projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

type policyDocument struct {
	SchemaVersion int                `json:"schema_version"`
	Credentials   []policyCredential `json:"credentials"`
}

type policyCredential struct {
	ID                 string              `json:"id"`
	SecretHash         string              `json:"secret_hash"`
	Subject            string              `json:"subject"`
	UserID             string              `json:"user_id"`
	AppID              string              `json:"app_id"`
	Repositories       []string            `json:"repositories"`
	Actions            []string            `json:"actions"`
	Harnesses          []string            `json:"harnesses"`
	Projects           []string            `json:"projects,omitempty"`
	CommunicationEdges []communicationEdge `json:"communication_edges,omitempty"`
	MaxDepth           int                 `json:"max_depth"`
	ExpiresAt          string              `json:"expires_at,omitempty"`
	Revoked            bool                `json:"revoked,omitempty"`
}

type communicationEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// LoadPolicy opens a strict owner-only, non-symlink policy and snapshots its
// hashed credential entries before serving.
func LoadPolicy(policyPath string, now func() time.Time) (*Authenticator, error) {
	if strings.TrimSpace(policyPath) == "" {
		return nil, errors.New("load credential policy: path is required")
	}
	if now == nil {
		return nil, errors.New("load credential policy: clock is required")
	}
	raw, err := readPolicyFile(policyPath)
	if err != nil {
		return nil, err
	}
	var document policyDocument
	if err := strictDecode(raw, &document); err != nil {
		return nil, fmt.Errorf("load credential policy: invalid document")
	}
	if document.SchemaVersion != policySchemaVersion || len(document.Credentials) == 0 {
		return nil, errors.New("load credential policy: unsupported or empty policy")
	}
	authenticator := &Authenticator{
		credentials: make(map[string]credential, len(document.Credentials)),
		now:         now,
		dummyHash:   sha256.Sum256([]byte("paje-v1-unknown-credential")),
	}
	hashes := make(map[[sha256.Size]byte]bool, len(document.Credentials))
	for _, candidate := range document.Credentials {
		entry, err := validateCredential(candidate)
		if err != nil {
			return nil, errors.New("load credential policy: invalid credential entry")
		}
		if _, duplicate := authenticator.credentials[candidate.ID]; duplicate || hashes[entry.secretHash] {
			return nil, errors.New("load credential policy: duplicate credential binding")
		}
		authenticator.credentials[candidate.ID] = entry
		hashes[entry.secretHash] = true
	}
	return authenticator, nil
}

func readPolicyFile(policyPath string) ([]byte, error) {
	fd, err := unix.Open(policyPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("load credential policy: open failed")
	}
	file := os.NewFile(uintptr(fd), "credential-policy")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("load credential policy: open failed")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("load credential policy: file must be regular and mode 0600")
	}
	limited := io.LimitReader(file, maxPolicyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("load credential policy: read failed")
	}
	if len(raw) == 0 || len(raw) > maxPolicyBytes {
		return nil, errors.New("load credential policy: invalid size")
	}
	return raw, nil
}

func validateCredential(candidate policyCredential) (credential, error) {
	if !publicIDPattern.MatchString(candidate.ID) || !safeIdentity(candidate.Subject) ||
		!safeIdentity(candidate.UserID) || !safeIdentity(candidate.AppID) ||
		candidate.MaxDepth < 0 || candidate.MaxDepth > 1 {
		return credential{}, errors.New("invalid identity")
	}
	decodedHash, err := hex.DecodeString(candidate.SecretHash)
	if err != nil || len(decodedHash) != sha256.Size || candidate.SecretHash != strings.ToLower(candidate.SecretHash) {
		return credential{}, errors.New("invalid hash")
	}
	entry := credential{
		principal: submission.Principal{
			CredentialID: candidate.ID,
			Subject:      candidate.Subject,
			UserID:       candidate.UserID,
			AppID:        candidate.AppID,
			Actions:      make(map[submission.Action]bool),
			Harnesses:    make(map[string]bool),
			MaxDepth:     candidate.MaxDepth,
		},
		actions:            make(map[string]bool),
		projects:           make(map[string]bool),
		communicationEdges: make(map[string]bool),
		revoked:            candidate.Revoked,
	}
	copy(entry.secretHash[:], decodedHash)
	if candidate.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, candidate.ExpiresAt)
		if err != nil || candidate.ExpiresAt != expiresAt.Format(time.RFC3339) {
			return credential{}, errors.New("invalid expiry")
		}
		expiresAt = expiresAt.UTC()
		entry.expiresAt = &expiresAt
	}
	for _, rawRepository := range candidate.Repositories {
		repository, err := canonicalRepository(rawRepository)
		if err != nil {
			return credential{}, err
		}
		if containsRepository(entry.principal.Repositories, repository) {
			return credential{}, errors.New("duplicate repository")
		}
		entry.principal.Repositories = append(entry.principal.Repositories, repository)
	}
	if len(entry.principal.Repositories) == 0 {
		return credential{}, errors.New("repository scope is required")
	}
	for _, action := range candidate.Actions {
		leaf, known := knownActions[action]
		if !known || entry.actions[action] {
			return credential{}, errors.New("invalid action")
		}
		entry.actions[action] = true
		if leaf != "" {
			entry.principal.Actions[leaf] = true
		}
	}
	if len(entry.actions) == 0 {
		return credential{}, errors.New("action scope is required")
	}
	for _, harness := range candidate.Harnesses {
		if !projectIDPattern.MatchString(harness) || entry.principal.Harnesses[harness] {
			return credential{}, errors.New("invalid harness")
		}
		entry.principal.Harnesses[harness] = true
	}
	if len(entry.principal.Harnesses) == 0 {
		return credential{}, errors.New("harness scope is required")
	}
	for _, project := range candidate.Projects {
		if !projectIDPattern.MatchString(project) || entry.projects[project] {
			return credential{}, errors.New("invalid project")
		}
		entry.projects[project] = true
	}
	for _, edge := range candidate.CommunicationEdges {
		key := edgeKey(edge.From, edge.To)
		if edge.From == edge.To || !entry.projects[edge.From] || !entry.projects[edge.To] || entry.communicationEdges[key] {
			return credential{}, errors.New("invalid communication edge")
		}
		entry.communicationEdges[key] = true
	}
	return entry, nil
}

func canonicalRepository(raw string) (submission.RepositoryScope, error) {
	if strings.TrimSpace(raw) != raw || raw == "" || strings.Contains(raw, "\\") {
		return submission.RepositoryScope{}, errors.New("invalid repository")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" ||
		parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return submission.RepositoryScope{}, errors.New("invalid repository")
	}
	if parsed.Hostname() != strings.ToLower(parsed.Hostname()) || !validRepositoryHost(parsed.Hostname()) {
		return submission.RepositoryScope{}, errors.New("invalid repository")
	}
	cleaned := path.Clean(parsed.Path)
	if cleaned != parsed.Path || strings.HasSuffix(parsed.Path, "/") {
		return submission.RepositoryScope{}, errors.New("invalid repository")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 2 {
		return submission.RepositoryScope{}, errors.New("invalid repository")
	}
	name := strings.TrimSuffix(parts[1], ".git")
	if !safeRepositoryPart(parts[0]) || !safeRepositoryPart(name) || parts[1] != name && parts[1] != name+".git" {
		return submission.RepositoryScope{}, errors.New("invalid repository")
	}
	owner := parts[0]
	if parsed.Hostname() == "github.com" {
		owner = strings.ToLower(owner)
		name = strings.ToLower(name)
	}
	return submission.RepositoryScope{Host: parsed.Hostname(), Owner: owner, Name: name}, nil
}

func validRepositoryHost(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, character := range label {
			if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func safeIdentity(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= 256 &&
		!strings.ContainsAny(value, "\r\n\x00")
}

func safeRepositoryPart(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 253 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func containsRepository(values []submission.RepositoryScope, want submission.RepositoryScope) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func strictDecode(raw []byte, destination any) error {
	if err := ValidateExactJSON(raw, destination); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func validateUniqueJSONNames(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := visitJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func visitJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		names := make(map[string]bool)
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok || names[name] {
				return errors.New("invalid or duplicate object name")
			}
			names[name] = true
			if err := visitJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := visitJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("array is not closed")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value, allowed := range values {
		if allowed {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
