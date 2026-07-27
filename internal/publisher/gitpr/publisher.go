// Package gitpr publishes immutable Pajé artifacts as deterministic Git
// branches and pull requests.
package gitpr

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/executor/commandrunner"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workspace"
)

const (
	cleanupTimeout                   = 30 * time.Second
	publisherVerificationOutputLimit = int64(1 << 20)
	publisherVerificationStage       = "publish-verification"
)

var gitObjectPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PullRequestRequest is the provider-neutral identity of one pull request.
type PullRequestRequest struct {
	Repository string
	Head       string
	Base       string
	Title      string
	Body       string
	Draft      bool
}

// PullRequest is the immutable provider result needed by the publisher.
type PullRequest struct {
	ID      string
	URL     string
	HeadSHA string
}

// PullRequests finds and creates pull requests.
type PullRequests interface {
	Find(context.Context, PullRequestRequest) (*PullRequest, error)
	Create(context.Context, PullRequestRequest) (PullRequest, error)
}

// Credentials creates one isolated Git credential environment. The cleanup
// callback must be safe to invoke with a canceled context.
type Credentials interface {
	Prepare(context.Context) (map[string]string, func(context.Context) error, error)
}

// Dependencies are the ports needed for immutable Git publication.
type Dependencies struct {
	Artifacts    artifact.Store
	Workspaces   workspace.Manager
	Changes      gitcapture.Capturer
	Executors    *executor.Registry
	PullRequests PullRequests
	Credentials  Credentials
	PushURL      func(repository string) (string, error)
}

// Publisher applies, verifies, and publishes an artifact.
type Publisher struct {
	artifacts    artifact.Store
	workspaces   workspace.Manager
	changes      gitcapture.Capturer
	executors    *executor.Registry
	pullRequests PullRequests
	credentials  Credentials
	pushURL      func(string) (string, error)
	git          *gitClient
}

var _ publisher.Publisher = (*Publisher)(nil)

// New validates dependencies and constructs a publisher.
func New(dependencies Dependencies) (*Publisher, error) {
	for _, dependency := range []struct {
		name  string
		value any
	}{
		{"artifact store", dependencies.Artifacts},
		{"workspace manager", dependencies.Workspaces},
		{"Git change adapter", dependencies.Changes},
		{"executor registry", dependencies.Executors},
		{"pull request provider", dependencies.PullRequests},
		{"credential provider", dependencies.Credentials},
	} {
		if nilInterface(dependency.value) {
			return nil, fmt.Errorf("create Git PR publisher: %s is required", dependency.name)
		}
	}
	if dependencies.PushURL == nil {
		return nil, fmt.Errorf("create Git PR publisher: push URL resolver is required")
	}
	git, err := newGitClient()
	if err != nil {
		return nil, err
	}
	return &Publisher{
		artifacts: dependencies.Artifacts, workspaces: dependencies.Workspaces,
		changes: dependencies.Changes, executors: dependencies.Executors,
		pullRequests: dependencies.PullRequests, credentials: dependencies.Credentials,
		pushURL: dependencies.PushURL, git: git,
	}, nil
}

// Publish publishes req exactly once or verifies and reuses exact existing
// branch and provider state.
func (p *Publisher) Publish(ctx context.Context, req publisher.Request) (result publisher.Result, returnErr error) {
	if err := ctx.Err(); err != nil {
		return publisher.Result{}, err
	}
	if err := req.ValidatePortable(); err != nil {
		return publisher.Result{}, err
	}
	if strings.ContainsAny(req.Title, "\r\n") || strings.ContainsAny(req.TargetRef, "\r\n") {
		return publisher.Result{}, fmt.Errorf("%w: title and target ref must be single-line", publisher.ErrInvalidRequest)
	}

	bundle, err := p.loadBoundArtifact(ctx, req)
	if err != nil {
		return publisher.Result{}, err
	}
	prepared, err := p.workspaces.Prepare(ctx, req.Repository, req.BaseSHA)
	if err != nil {
		return publisher.Result{}, fmt.Errorf("prepare publication workspace: %w", err)
	}
	workspaceCleaned := false
	defer func() {
		if workspaceCleaned {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, prepared.Cleanup(cleanupContext))
	}()

	if err := p.changes.Apply(ctx, gitcapture.ApplyRequest{
		Workspace: prepared.Path(), BaseSHA: req.BaseSHA,
		Patch: bundle.ChangesPatch, ExpectedTreeSHA: bundle.Manifest.TreeSHA,
	}); err != nil {
		return publisher.Result{}, fmt.Errorf("apply publication artifact: %w", err)
	}
	verificationDigest, err := p.runRequiredVerification(ctx, prepared.Path(), req, bundle.Verification)
	if err != nil {
		return publisher.Result{}, err
	}
	req.VerificationDigest = verificationDigest

	commitSHA, err := p.git.commit(ctx, prepared.Path(), commitMessage(req))
	if err != nil {
		return publisher.Result{}, fmt.Errorf("create publication commit: %w", err)
	}
	trusted, err := p.git.importTrusted(ctx, prepared.Path(), commitSHA)
	if err != nil {
		return publisher.Result{}, fmt.Errorf("import publication commit into trusted repository: %w", err)
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, trusted.Cleanup(cleanupContext))
	}()
	if err := p.git.verifyLocalCommit(ctx, trusted.path, commitSHA, req, bundle.Manifest.TreeSHA); err != nil {
		return publisher.Result{}, err
	}
	cleanupContext, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	cleanupErr := prepared.Cleanup(cleanupContext)
	cancelCleanup()
	if cleanupErr != nil {
		return publisher.Result{}, fmt.Errorf("cleanup publication verification workspace: %w", cleanupErr)
	}
	workspaceCleaned = true

	pushURL, err := p.pushURL(req.Repository)
	if err != nil {
		return publisher.Result{}, fmt.Errorf("resolve publication push URL: %w", err)
	}
	if err := validatePushURL(pushURL); err != nil {
		return publisher.Result{}, err
	}
	if err := p.git.validateEffectiveURL(ctx, trusted, pushURL); err != nil {
		return publisher.Result{}, err
	}
	credentialEnvironment, cleanupCredentials, err := p.credentials.Prepare(ctx)
	if err != nil {
		return publisher.Result{}, fmt.Errorf("prepare publication credentials: %w", err)
	}
	if cleanupCredentials == nil {
		return publisher.Result{}, fmt.Errorf("prepare publication credentials: cleanup is required")
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, cleanupCredentials(cleanupContext))
	}()
	if err := validateCredentialEnvironment(credentialEnvironment); err != nil {
		return publisher.Result{}, err
	}

	remoteCommit, exists, err := p.git.remoteBranch(ctx, trusted, pushURL, req.Branch, credentialEnvironment)
	if err != nil {
		return publisher.Result{}, retryRemoteError(fmt.Errorf("inspect publication branch: %w", err))
	}
	if exists {
		if remoteCommit != commitSHA {
			return publisher.Result{}, fmt.Errorf("%w: deterministic publication commit does not match existing branch", publisher.ErrConflict)
		}
		if err := p.git.verifyRemoteCommit(
			ctx, trusted, pushURL, req.Branch, remoteCommit,
			req, bundle.Manifest.TreeSHA, credentialEnvironment,
		); err != nil {
			return publisher.Result{}, err
		}
	} else {
		if err := p.git.push(ctx, trusted, pushURL, req.Branch, credentialEnvironment); err != nil {
			if cancellationError(err) {
				return publisher.Result{}, err
			}
			inspectContext, cancelInspect := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			winner, winnerExists, inspectErr := p.git.remoteBranch(
				inspectContext, trusted, pushURL, req.Branch, credentialEnvironment,
			)
			cancelInspect()
			if inspectErr != nil {
				return publisher.Result{}, retryRemoteError(errors.Join(fmt.Errorf("push publication branch: %w", err), inspectErr))
			}
			if !winnerExists {
				return publisher.Result{}, retryRemoteError(fmt.Errorf("push publication branch: %w", err))
			}
			if winner != commitSHA {
				return publisher.Result{}, fmt.Errorf("%w: publication branch won by commit %s", publisher.ErrConflict, winner)
			}
		}
	}

	pullRequest, err := p.findOrCreatePullRequest(ctx, req, commitSHA)
	if err != nil {
		return publisher.Result{}, err
	}
	result = publisher.Result{
		Provider: "github", Branch: req.Branch, CommitSHA: commitSHA,
		PullRequestID: pullRequest.ID, PullRequestURL: pullRequest.URL,
		VerificationDigest: req.VerificationDigest,
	}
	if err := result.ValidateVerified(req); err != nil {
		return publisher.Result{}, err
	}
	return result, nil
}

func (p *Publisher) loadBoundArtifact(ctx context.Context, req publisher.Request) (artifact.Bundle, error) {
	bundle, err := p.artifacts.Load(ctx, req.Artifact)
	if err != nil {
		return artifact.Bundle{}, fmt.Errorf("load publication artifact: %w", err)
	}
	reference, err := artifact.ReferenceFor(bundle)
	if err != nil {
		return artifact.Bundle{}, fmt.Errorf("verify publication artifact: %w", err)
	}
	if reference != req.Artifact {
		return artifact.Bundle{}, fmt.Errorf("%w: artifact reference is not exact", publisher.ErrConflict)
	}
	executionEvidence, err := publisher.DecodeExecutionEvidence(bundle.ExecutionMetadata)
	if err != nil {
		return artifact.Bundle{}, fmt.Errorf("%w: artifact execution evidence is invalid", publisher.ErrConflict)
	}
	switch {
	case bundle.Manifest.RunID != req.RunID:
		return artifact.Bundle{}, fmt.Errorf("%w: artifact run ID does not match", publisher.ErrConflict)
	case bundle.Manifest.Repository != req.Repository:
		return artifact.Bundle{}, fmt.Errorf("%w: artifact repository does not match", publisher.ErrConflict)
	case bundle.Manifest.BaseSHA != req.BaseSHA:
		return artifact.Bundle{}, fmt.Errorf("%w: artifact base SHA does not match", publisher.ErrConflict)
	case !gitObjectPattern.MatchString(bundle.Manifest.TreeSHA):
		return artifact.Bundle{}, fmt.Errorf("%w: artifact tree SHA is invalid", publisher.ErrConflict)
	case len(bundle.ChangesPatch) == 0:
		return artifact.Bundle{}, fmt.Errorf("%w: artifact patch is empty", publisher.ErrConflict)
	case !reflect.DeepEqual(bundle.Manifest, req.ArtifactManifest):
		return artifact.Bundle{}, fmt.Errorf("%w: artifact manifest is not exact", publisher.ErrConflict)
	case !reflect.DeepEqual(executionEvidence, req.ExecutionEvidence):
		return artifact.Bundle{}, fmt.Errorf("%w: artifact execution evidence is not exact", publisher.ErrConflict)
	}
	return bundle, nil
}

func (p *Publisher) runRequiredVerification(
	ctx context.Context,
	root string,
	req publisher.Request,
	evidence []verification.Result,
) (string, error) {
	target, err := p.executors.Resolve(req.WorkerProfile.Clone())
	if err != nil {
		return "", fmt.Errorf("resolve publication verification executor: %w", err)
	}
	receipt := publisherVerificationReceipt{
		Version: "paje-publisher-verification-v1", RunID: req.RunID,
		ArtifactDigest: req.Artifact.Digest, TreeSHA: req.ArtifactManifest.TreeSHA,
		ProfileDigest: req.WorkerProfile.Digest, Environment: publisherSandboxEnvironment(),
	}
	identity := publisherVerificationIdentity(req)
	for index, prior := range evidence {
		if !prior.Command.Required {
			continue
		}
		if !prior.Passed {
			return "", fmt.Errorf("%w: artifact records failed required verification %q", publisher.ErrConflict, prior.Command.Name)
		}
		command := prior.Command
		directory, err := repositoryRelativeDirectory(root, command.Directory)
		if err != nil {
			return "", fmt.Errorf("%w: reconstruct verification %q: %v", publisher.ErrConflict, command.Name, err)
		}
		command.Directory = directory
		command.Args = append([]string(nil), command.Args...)
		command.Environment = cloneStrings(command.Environment)
		if filepath.Base(command.Executable) == "go" {
			if command.Environment == nil {
				command.Environment = make(map[string]string, 1)
			}
			command.Environment["GOWORK"] = "off"
		}
		attempt := identity
		attempt.Sequence = index
		runner, err := commandrunner.New(commandrunner.Config{
			Executor: target, Profile: req.WorkerProfile.Clone(), Attempt: attempt,
			Workspace: root, Environment: publisherSandboxEnvironment(),
			OutputLimit: publisherVerificationOutputLimit, Writable: false,
		})
		if err != nil {
			return "", fmt.Errorf("construct publication verification %q: %w", command.Name, err)
		}
		result := runner.Run(ctx, command)
		if !result.Passed {
			return "", fmt.Errorf("required publication verification %q failed: %s/%s", command.Name, result.FailureClass, result.CauseCode)
		}
		receipt.Commands = append(receipt.Commands, publisherVerificationCommandReceipt{
			Index: index, AttemptKey: publisherVerificationAttemptKey(identity, index),
			Command: command, ExitCode: result.ExitCode, Truncated: result.Truncated,
			Passed: result.Passed, FailureClass: result.FailureClass, CauseCode: result.CauseCode,
		})
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return "", errors.New("encode publisher verification receipt")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

type publisherVerificationReceipt struct {
	Version        string                                `json:"version"`
	RunID          string                                `json:"run_id"`
	ArtifactDigest string                                `json:"artifact_digest"`
	TreeSHA        string                                `json:"tree_sha"`
	ProfileDigest  string                                `json:"profile_digest"`
	Environment    map[string]string                     `json:"environment"`
	Commands       []publisherVerificationCommandReceipt `json:"commands"`
}

type publisherVerificationCommandReceipt struct {
	Index        int                  `json:"index"`
	AttemptKey   string               `json:"attempt_key"`
	Command      verification.Command `json:"command"`
	ExitCode     int                  `json:"exit_code"`
	Truncated    bool                 `json:"truncated"`
	Passed       bool                 `json:"passed"`
	FailureClass string               `json:"failure_class,omitempty"`
	CauseCode    string               `json:"cause_code,omitempty"`
}

func publisherVerificationIdentity(req publisher.Request) executor.AttemptID {
	digest := sha256.Sum256([]byte(
		"paje-publisher-verification-v1\x00" + req.RunID + "\x00" + req.Artifact.Digest + "\x00" +
			req.ArtifactManifest.TreeSHA + "\x00" + req.WorkerProfile.Digest,
	))
	seconds := int64(binary.BigEndian.Uint32(digest[0:4]))
	nanoseconds := int64(binary.BigEndian.Uint32(digest[4:8]) % 1_000_000_000)
	attempt := int(binary.BigEndian.Uint32(digest[8:12])&0x7fffffff) + 1
	return executor.AttemptID{
		RunID: req.RunID, Stage: publisherVerificationStage, Attempt: attempt,
		StartedAt: time.Unix(946684800+seconds, nanoseconds).UTC(),
		Purpose:   executor.PurposeVerification,
	}
}

func publisherVerificationAttemptKey(identity executor.AttemptID, index int) string {
	identity.Sequence = index + 1
	return identity.Key()
}

func publisherSandboxEnvironment() map[string]string {
	return map[string]string{
		"HOME": "/home/paje", "PATH": "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "TMPDIR": "/tmp",
	}
}

func (p *Publisher) findOrCreatePullRequest(
	ctx context.Context,
	req publisher.Request,
	commitSHA string,
) (PullRequest, error) {
	request := PullRequestRequest{
		Repository: req.Repository, Head: req.Branch, Base: req.TargetRef,
		Title: req.Title, Body: req.Body, Draft: req.Draft,
	}
	existing, err := p.pullRequests.Find(ctx, request)
	if err != nil {
		return PullRequest{}, fmt.Errorf("find pull request: %w", err)
	}
	var pullRequest PullRequest
	if existing != nil {
		pullRequest = *existing
	} else {
		pullRequest, err = p.pullRequests.Create(ctx, request)
		if err != nil {
			return PullRequest{}, fmt.Errorf("create pull request: %w", err)
		}
	}
	if err := validatePullRequest(pullRequest, commitSHA); err != nil {
		return PullRequest{}, err
	}
	return pullRequest, nil
}

func validatePullRequest(result PullRequest, commitSHA string) error {
	parsed, err := url.Parse(result.URL)
	switch {
	case strings.TrimSpace(result.ID) == "":
		return fmt.Errorf("%w: pull request ID is empty", publisher.ErrConflict)
	case err != nil || parsed.Scheme != "https" || parsed.Host == "":
		return fmt.Errorf("%w: pull request URL is invalid", publisher.ErrConflict)
	case result.HeadSHA != commitSHA:
		return fmt.Errorf("%w: pull request head SHA does not match branch", publisher.ErrConflict)
	default:
		return nil
	}
}

func containedDirectory(root, relative string) (string, error) {
	if strings.ContainsRune(relative, '\x00') || filepath.IsAbs(relative) {
		return "", errors.New("directory must be repository-relative")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("directory escapes repository")
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(canonicalRoot, clean))
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(canonicalRoot, candidate)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", errors.New("directory escapes repository through symlink")
	}
	return candidate, nil
}

func repositoryRelativeDirectory(root, relative string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		relative = "."
	}
	clean := filepath.Clean(relative)
	if _, err := containedDirectory(root, clean); err != nil {
		return "", err
	}
	return filepath.ToSlash(clean), nil
}

func commitMessage(req publisher.Request) string {
	message := req.Title + "\n\n" +
		"Paje-Run-ID: " + req.RunID + "\n" +
		"Paje-Base-SHA: " + req.BaseSHA + "\n" +
		"Paje-Artifact-Digest: " + req.Artifact.Digest
	if req.VerificationDigest != "" {
		message += "\nPaje-Publisher-Verification-Digest: " + req.VerificationDigest
	}
	return message
}

func validatePushURL(value string) error {
	if strings.TrimSpace(value) == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%w: publication push URL is invalid", publisher.ErrInvalidRequest)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%w: publication push URL is invalid", publisher.ErrInvalidRequest)
	}
	if parsed.Scheme == "" {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%w: local publication push URL must be absolute", publisher.ErrInvalidRequest)
		}
		return nil
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%w: publication push URL must not contain credentials", publisher.ErrInvalidRequest)
	}
	return nil
}

func validateCredentialEnvironment(environment map[string]string) error {
	if len(environment) == 0 {
		return nil
	}
	if len(environment) != 4 ||
		!filepath.IsAbs(environment["GIT_ASKPASS"]) ||
		environment["GIT_TERMINAL_PROMPT"] != "0" ||
		environment["PAJE_GIT_USERNAME"] == "" ||
		environment["PAJE_GIT_PASSWORD"] == "" {
		return fmt.Errorf("prepare publication credentials: incomplete credential environment")
	}
	for key, value := range environment {
		switch key {
		case "GIT_ASKPASS", "GIT_TERMINAL_PROMPT", "PAJE_GIT_USERNAME", "PAJE_GIT_PASSWORD":
		default:
			return fmt.Errorf("prepare publication credentials: unexpected environment key %q", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("prepare publication credentials: invalid environment value")
		}
	}
	return nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

type retryableMarker interface {
	Retryable() bool
}

type retryableError struct{ error }

func (retryableError) Retryable() bool { return true }

func markRetryable(err error) error {
	if err == nil || IsRetryable(err) {
		return err
	}
	return retryableError{error: err}
}

func retryRemoteError(err error) error {
	if cancellationError(err) {
		return err
	}
	return markRetryable(err)
}

func cancellationError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// IsRetryable reports whether err explicitly permits a publication retry.
func IsRetryable(err error) bool {
	var marker retryableMarker
	return errors.As(err, &marker) && marker.Retryable()
}
