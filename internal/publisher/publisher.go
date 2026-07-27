// Package publisher defines provider-neutral change publication contracts.
package publisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
)

var (
	// ErrInvalidRequest indicates invalid publication input or output.
	ErrInvalidRequest = errors.New("invalid publication request")
	// ErrConflict indicates existing provider state conflicts with the requested
	// immutable publication.
	ErrConflict = errors.New("publication conflict")
	// ErrProviderUnavailable indicates the configured provider cannot serve the
	// request.
	ErrProviderUnavailable = errors.New("publication provider unavailable")
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Request describes an immutable artifact publication.
type Request struct {
	RunID             string                     `json:"run_id"`
	Repository        string                     `json:"repository"`
	BaseSHA           string                     `json:"base_sha"`
	TargetRef         string                     `json:"target_ref"`
	Branch            string                     `json:"branch"`
	Artifact          artifact.Reference         `json:"artifact"`
	ArtifactManifest  artifact.Manifest          `json:"artifact_manifest"`
	WorkerProfile     workerprofile.Snapshot     `json:"worker_profile"`
	ExecutionEvidence artifact.ExecutionEvidence `json:"execution_evidence"`
	Verification      []verification.Result      `json:"verification"`
	Title             string                     `json:"title"`
	Body              string                     `json:"body"`
	Draft             bool                       `json:"draft"`

	// VerificationDigest is produced inside the publisher after isolated
	// re-verification. It is excluded from the caller-owned wire contract and
	// bound into the deterministic publication commit before credentials exist.
	VerificationDigest string `json:"-"`
}

// Result identifies an immutable provider publication.
type Result struct {
	Provider           string `json:"provider"`
	Branch             string `json:"branch"`
	CommitSHA          string `json:"commit_sha"`
	PullRequestID      string `json:"pull_request_id"`
	PullRequestURL     string `json:"pull_request_url"`
	VerificationDigest string `json:"verification_digest"`
}

// Publisher publishes an immutable change artifact.
type Publisher interface {
	Publish(context.Context, Request) (Result, error)
}

// Validate rejects incomplete, unsafe, or inconsistent publication requests.
func (r Request) Validate() error {
	return r.validateCore()
}

// ValidatePortable additionally requires the persisted portable worker,
// artifact-manifest, and execution-evidence binding used by real publishers.
// The narrower Validate remains for frozen in-memory port fixtures only.
func (r Request) ValidatePortable() error {
	if err := r.validateCore(); err != nil {
		return err
	}
	switch {
	case r.ArtifactManifest.RunID != r.RunID:
		return invalid("artifact manifest run ID does not match request")
	case r.ArtifactManifest.Repository != r.Repository:
		return invalid("artifact manifest repository does not match request")
	case r.ArtifactManifest.BaseSHA != r.BaseSHA:
		return invalid("artifact manifest base SHA does not match request")
	case !present(r.ArtifactManifest.TreeSHA):
		return invalid("artifact manifest tree SHA is required")
	case len(r.ArtifactManifest.Changes) == 0:
		return invalid("artifact manifest changes are required")
	default:
		if err := validateVerificationPlanBinding(r); err != nil {
			return invalid(err.Error())
		}
		if err := validatePortableEvidence(r); err != nil {
			return invalid(err.Error())
		}
		if r.VerificationDigest != "" && !validHex(r.VerificationDigest, 64) {
			return invalid("publisher verification digest must be 64 hexadecimal characters")
		}
		return nil
	}
}

func (r Request) validateCore() error {
	switch {
	case !validRunID(r.RunID):
		return invalid("run ID is invalid")
	case !present(r.Repository):
		return invalid("repository is required")
	case !validHex(r.BaseSHA, 40):
		return invalid("base SHA must be 40 hexadecimal characters")
	case !present(r.TargetRef):
		return invalid("target ref is required")
	case r.Branch != publicationBranch(r.RunID):
		return invalid("branch must be " + publicationBranch(r.RunID))
	case r.Artifact.RunID != r.RunID:
		return invalid("artifact run ID does not match request")
	case !validHex(r.Artifact.Digest, 64):
		return invalid("artifact digest must be 64 hexadecimal characters")
	case r.Artifact.Size <= 0:
		return invalid("artifact size must be positive")
	case !present(r.Title):
		return invalid("title is required")
	default:
		return nil
	}
}

// Validate rejects provider results that are incomplete or inconsistent with
// req.
func (r Result) Validate(req Request) error {
	if err := req.validateCore(); err != nil {
		return fmt.Errorf("%w: result request: %v", ErrInvalidRequest, err)
	}
	if !present(r.Provider) {
		return invalid("provider is required")
	}
	if r.Branch != req.Branch {
		return invalid("result branch does not match request")
	}
	if !validHex(r.CommitSHA, 40) {
		return invalid("commit SHA must be 40 hexadecimal characters")
	}
	if !present(r.PullRequestID) {
		return invalid("pull request ID is required")
	}
	parsed, err := url.Parse(r.PullRequestURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return invalid("pull request URL must be an absolute HTTPS URL")
	}
	return nil
}

// ValidateVerified additionally requires the exact isolated publisher
// verification receipt produced before credentials are prepared.
func (r Result) ValidateVerified(req Request) error {
	if err := r.Validate(req); err != nil {
		return err
	}
	if !validHex(r.VerificationDigest, 64) {
		return invalid("publisher verification digest must be 64 hexadecimal characters")
	}
	if req.VerificationDigest != "" && r.VerificationDigest != req.VerificationDigest {
		return invalid("publisher verification digest does not match request")
	}
	return nil
}

// CloneRequest returns an independent copy of req.
func CloneRequest(req Request) Request {
	cloned := req
	cloned.Artifact = artifact.Reference{
		RunID:  req.Artifact.RunID,
		Digest: req.Artifact.Digest,
		Size:   req.Artifact.Size,
	}
	cloned.ArtifactManifest.Changes = append([]artifact.Change(nil), req.ArtifactManifest.Changes...)
	cloned.ArtifactManifest.Members = append([]artifact.Member(nil), req.ArtifactManifest.Members...)
	cloned.ArtifactManifest.MemoryIDs = append([]string(nil), req.ArtifactManifest.MemoryIDs...)
	cloned.WorkerProfile = req.WorkerProfile.Clone()
	cloned.ExecutionEvidence = cloneExecutionEvidence(req.ExecutionEvidence)
	cloned.Verification = cloneVerification(req.Verification)
	return cloned
}

// CloneVerification returns an independent copy of the frozen artifact
// verification plan carried across the workflow/publisher boundary.
func CloneVerification(source []verification.Result) []verification.Result {
	clone := cloneVerification(source)
	if clone == nil {
		return []verification.Result{}
	}
	return clone
}

// RequestsEqual compares the complete immutable publication subject.
func RequestsEqual(left, right Request) bool {
	return reflect.DeepEqual(left, right)
}

// DecodeExecutionEvidence strictly decodes the safe portable evidence carried
// by a canonical artifact. It never accepts unknown or trailing fields.
func DecodeExecutionEvidence(raw json.RawMessage) (artifact.ExecutionEvidence, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var evidence artifact.ExecutionEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return artifact.ExecutionEvidence{}, fmt.Errorf("decode execution evidence: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return artifact.ExecutionEvidence{}, errors.New("decode execution evidence: trailing data")
	}
	return evidence, nil
}

func validatePortableEvidence(request Request) error {
	canonical, err := workerprofile.Canonicalize(request.WorkerProfile.Clone())
	if err != nil || request.WorkerProfile.Digest == "" ||
		!reflect.DeepEqual(canonical, request.WorkerProfile) {
		return errors.New("worker profile snapshot is not exact and canonical")
	}
	evidence := request.ExecutionEvidence
	if evidence.Profile == nil || evidence.Runtime == nil || evidence.Harness == nil ||
		evidence.Tools == nil || evidence.Attempts == nil ||
		evidence.VerificationEnvironmentKeys == nil {
		return errors.New("portable execution evidence is incomplete")
	}
	if evidence.Profile.Name != request.WorkerProfile.Metadata.Name ||
		evidence.Profile.Revision != request.WorkerProfile.Metadata.Revision ||
		evidence.Profile.Digest != request.WorkerProfile.Digest {
		return errors.New("portable execution profile evidence does not match request")
	}
	if err := validateRuntimeEvidence(request.WorkerProfile, *evidence.Runtime); err != nil {
		return err
	}
	if evidence.Harness.ID != request.WorkerProfile.Harness.ID ||
		evidence.Harness.DeclaredVersion != request.WorkerProfile.Harness.Version ||
		evidence.Harness.ProbedVersion != request.WorkerProfile.Harness.Version ||
		!evidence.Harness.ProbePassed {
		return errors.New("portable harness evidence does not match worker profile")
	}
	tools := *evidence.Tools
	if len(tools) != len(request.WorkerProfile.Tools) {
		return errors.New("portable tool evidence does not match worker profile")
	}
	for index, tool := range tools {
		profileTool := request.WorkerProfile.Tools[index]
		if tool.Name != profileTool.Name || tool.DeclaredVersion != profileTool.Version ||
			tool.ProbedVersion != profileTool.Version || !tool.ProbePassed {
			return errors.New("portable tool evidence does not match worker profile")
		}
	}
	attempts := *evidence.Attempts
	agentIndex := -1
	attemptKeys := make(map[string]struct{}, len(attempts))
	for index, attempt := range attempts {
		if err := attempt.ID.Validate(); err != nil || attempt.ID.RunID != request.RunID ||
			attempt.Started && !attempt.Created || attempt.Completed && !attempt.Started {
			return errors.New("portable attempt evidence does not match run")
		}
		if _, duplicate := attemptKeys[attempt.ID.Key()]; duplicate {
			return errors.New("portable attempt evidence is duplicated")
		}
		attemptKeys[attempt.ID.Key()] = struct{}{}
		if attempt.ID.Purpose == executor.PurposeAgent {
			if agentIndex >= 0 || attempt.ID.Sequence != 0 ||
				evidence.ExitCode != attempt.ExitCode || evidence.Started != attempt.Started ||
				evidence.Completed != attempt.Completed || evidence.Truncated != attempt.Truncated {
				return errors.New("portable agent evidence is inconsistent")
			}
			agentIndex = index
		}
	}
	if agentIndex < 0 {
		return errors.New("portable agent evidence is missing")
	}
	agentAttempt := attempts[agentIndex]
	verificationAttempts := make(map[int]artifact.AttemptEvidence)
	for _, attempt := range attempts {
		if attempt.ID.Stage != agentAttempt.ID.Stage ||
			attempt.ID.Attempt != agentAttempt.ID.Attempt ||
			!attempt.ID.StartedAt.Equal(agentAttempt.ID.StartedAt) {
			return errors.New("portable attempt generation evidence drifted")
		}
		if attempt.ID.Purpose != executor.PurposeVerification {
			continue
		}
		if attempt.ID.Sequence <= 0 {
			return errors.New("portable verification attempt sequence is invalid")
		}
		if _, duplicate := verificationAttempts[attempt.ID.Sequence]; duplicate {
			return errors.New("portable verification attempt sequence is duplicated")
		}
		verificationAttempts[attempt.ID.Sequence] = attempt
	}
	if err := validateAgentEnvironmentEvidence(request.WorkerProfile, evidence, agentAttempt.Started); err != nil {
		return err
	}
	if err := validateVerificationEnvironmentEvidence(
		evidence.VerificationEnvironmentKeys,
		request.Verification,
		verificationAttempts,
	); err != nil {
		return err
	}
	return nil
}

func validateAgentEnvironmentEvidence(
	profile workerprofile.Snapshot,
	evidence artifact.ExecutionEvidence,
	confirmed bool,
) error {
	if !confirmed {
		if evidence.AgentEnvironmentKeys != nil {
			return errors.New("portable agent environment evidence claims an unconfirmed presentation")
		}
		return nil
	}
	if evidence.AgentEnvironmentKeys == nil {
		return errors.New("portable confirmed agent environment evidence is missing")
	}
	expected := map[string]struct{}{"HOME": {}, "PATH": {}, "TMPDIR": {}}
	for _, requirement := range profile.Secrets {
		switch {
		case requirement.Capability == "harness.codex-auth":
			if requirement.BindingRevision == 0 || requirement.Stage != workerprofile.StageAgent ||
				requirement.Delivery != workerprofile.DeliveryDirectory ||
				requirement.Target != "/run/paje/secrets/codex" || !requirement.Required {
				return errors.New("portable Codex auth environment requirement is not exact")
			}
			expected["CODEX_HOME"] = struct{}{}
		case strings.HasPrefix(requirement.Capability, "harness."):
			return errors.New("portable harness environment requirement is unsupported")
		}
		if requirement.Delivery == workerprofile.DeliveryEnvironment {
			expected[requirement.Target] = struct{}{}
		}
	}
	want := make([]string, 0, len(expected))
	for key := range expected {
		want = append(want, key)
	}
	sort.Strings(want)
	if !reflect.DeepEqual([]string(*evidence.AgentEnvironmentKeys), want) {
		return errors.New("portable agent environment evidence does not match exact declaration")
	}
	return nil
}

func validateVerificationEnvironmentEvidence(
	keys *artifact.EnvironmentKeyList,
	plan []verification.Result,
	attempts map[int]artifact.AttemptEvidence,
) error {
	if keys == nil {
		return errors.New("portable verification environment evidence is missing")
	}
	actual := []string(*keys)
	if len(plan) == 0 {
		if len(attempts) != 0 || len(actual) != 0 {
			return errors.New("portable verification evidence contradicts the frozen zero-attempt plan")
		}
		return nil
	}
	if len(attempts) != len(plan) {
		return errors.New("portable verification attempt evidence does not match frozen plan")
	}
	expected := make(map[string]struct{}, 4)
	for index, scheduled := range plan {
		attempt, found := attempts[index+1]
		if !found || !attempt.Started {
			return errors.New("portable verification attempt confirmation is missing")
		}
		expected["HOME"] = struct{}{}
		expected["PATH"] = struct{}{}
		expected["TMPDIR"] = struct{}{}
		for _, key := range scheduled.Command.EnvironmentKeys {
			expected[key] = struct{}{}
		}
	}
	want := make([]string, 0, len(expected))
	for key := range expected {
		want = append(want, key)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(actual, want) {
		return errors.New("portable verification environment evidence does not match exact declaration")
	}
	return nil
}

func validateVerificationPlanBinding(request Request) error {
	if request.Verification == nil {
		return errors.New("portable frozen verification plan is missing")
	}
	for _, result := range request.Verification {
		if result.Command.Environment != nil {
			return errors.New("portable frozen verification declaration drifted")
		}
		if err := validateVerificationEnvironmentDeclaration(result.Command.EnvironmentKeys); err != nil {
			return err
		}
	}
	want, err := verificationPlanMember(request.Verification, request.ArtifactManifest)
	if err != nil {
		return errors.New("portable frozen verification plan cannot be encoded")
	}
	found := false
	for _, member := range request.ArtifactManifest.Members {
		if member.Name != "verification.json" {
			continue
		}
		if found || member != want {
			return errors.New("portable frozen verification plan does not match artifact digest")
		}
		found = true
	}
	if !found {
		return errors.New("portable frozen verification plan digest is missing")
	}
	return nil
}

func validateVerificationEnvironmentDeclaration(keys []string) error {
	if keys == nil {
		return errors.New("portable frozen verification environment declaration is missing")
	}
	previous := ""
	for _, key := range keys {
		if key != "GOWORK" || previous != "" && key <= previous {
			return errors.New("portable frozen verification environment declaration is invalid")
		}
		previous = key
	}
	return nil
}

func verificationPlanMember(
	plan []verification.Result,
	manifest artifact.Manifest,
) (artifact.Member, error) {
	manifest.Members = nil
	normalized, _, _, err := artifact.Canonicalize(artifact.Bundle{
		Manifest:     manifest,
		ChangesPatch: []byte("frozen verification plan binding"),
		ExecutionMetadata: json.RawMessage(
			`{"completed":false,"duration":0,"exit_code":0,"started":false,"truncated":false}`,
		),
		Verification: cloneVerification(plan),
		Preflight:    map[string]string{},
		Warnings:     []string{},
	})
	if err != nil {
		return artifact.Member{}, err
	}
	for _, member := range normalized.Manifest.Members {
		if member.Name == "verification.json" {
			return member, nil
		}
	}
	return artifact.Member{}, errors.New("canonical verification member is missing")
}

func validateRuntimeEvidence(profile workerprofile.Snapshot, evidence artifact.RuntimeEvidence) error {
	if evidence.Kind != profile.Runtime.Kind {
		return errors.New("portable runtime evidence does not match worker profile")
	}
	switch profile.Runtime.Kind {
	case workerprofile.RuntimeOCI:
		const marker = "@sha256:"
		index := strings.LastIndex(profile.Runtime.Image, marker)
		if index < 0 || evidence.ImageDigest != profile.Runtime.Image[index+len(marker):] ||
			evidence.Platform != profile.Runtime.Platform || !evidence.Isolated {
			return errors.New("portable runtime evidence does not match worker profile")
		}
	case workerprofile.RuntimeHost:
		if evidence.ImageDigest != "" || evidence.Platform != "" || evidence.Isolated || evidence.Certified {
			return errors.New("portable host runtime evidence is invalid")
		}
	default:
		return errors.New("portable runtime kind is unsupported")
	}
	return nil
}

func cloneExecutionEvidence(source artifact.ExecutionEvidence) artifact.ExecutionEvidence {
	clone := source
	if source.Profile != nil {
		value := *source.Profile
		clone.Profile = &value
	}
	if source.Runtime != nil {
		value := *source.Runtime
		clone.Runtime = &value
	}
	if source.Harness != nil {
		value := *source.Harness
		clone.Harness = &value
	}
	if source.Tools != nil {
		value := make(artifact.ToolEvidenceList, len(*source.Tools))
		copy(value, *source.Tools)
		clone.Tools = &value
	}
	if source.Attempts != nil {
		value := make(artifact.AttemptEvidenceList, len(*source.Attempts))
		copy(value, *source.Attempts)
		clone.Attempts = &value
	}
	if source.AgentEnvironmentKeys != nil {
		value := make(artifact.EnvironmentKeyList, len(*source.AgentEnvironmentKeys))
		copy(value, *source.AgentEnvironmentKeys)
		clone.AgentEnvironmentKeys = &value
	}
	if source.VerificationEnvironmentKeys != nil {
		value := make(artifact.EnvironmentKeyList, len(*source.VerificationEnvironmentKeys))
		copy(value, *source.VerificationEnvironmentKeys)
		clone.VerificationEnvironmentKeys = &value
	}
	return clone
}

func cloneVerification(source []verification.Result) []verification.Result {
	if source == nil {
		return nil
	}
	clone := make([]verification.Result, len(source))
	copy(clone, source)
	for index := range clone {
		clone[index].Command.Args = append([]string(nil), source[index].Command.Args...)
		if source[index].Command.EnvironmentKeys != nil {
			clone[index].Command.EnvironmentKeys = append(
				[]string{}, source[index].Command.EnvironmentKeys...,
			)
		}
		if source[index].Command.Environment != nil {
			clone[index].Command.Environment = make(map[string]string, len(source[index].Command.Environment))
			for key, value := range source[index].Command.Environment {
				clone[index].Command.Environment[key] = value
			}
		}
	}
	return clone
}

func invalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, message)
}

func publicationBranch(runID string) string {
	return "paje/code-change/" + runID
}

func validRunID(value string) bool {
	return runIDPattern.MatchString(value)
}

func present(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00')
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !('0' <= char && char <= '9') &&
			!('a' <= char && char <= 'f') &&
			!('A' <= char && char <= 'F') {
			return false
		}
	}
	return true
}
