// Package gitpr publishes immutable Pajé artifacts as deterministic Git
// branches and pull requests.
package gitpr

import (
	"context"
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
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workspace"
)

const cleanupTimeout = 30 * time.Second

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
	Artifacts               artifact.Store
	Workspaces              workspace.Manager
	Changes                 gitcapture.Capturer
	Verification            verification.Runner
	VerificationEnvironment map[string]string
	PullRequests            PullRequests
	Credentials             Credentials
	PushURL                 func(repository string) (string, error)
}

// Publisher applies, verifies, and publishes an artifact.
type Publisher struct {
	artifacts               artifact.Store
	workspaces              workspace.Manager
	changes                 gitcapture.Capturer
	verification            verification.Runner
	verificationEnvironment map[string]string
	pullRequests            PullRequests
	credentials             Credentials
	pushURL                 func(string) (string, error)
	git                     *gitClient
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
		{"verification runner", dependencies.Verification},
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
	if err := validateVerificationEnvironment(dependencies.VerificationEnvironment); err != nil {
		return nil, err
	}
	git, err := newGitClient()
	if err != nil {
		return nil, err
	}
	return &Publisher{
		artifacts: dependencies.Artifacts, workspaces: dependencies.Workspaces,
		changes: dependencies.Changes, verification: dependencies.Verification,
		verificationEnvironment: cloneStrings(dependencies.VerificationEnvironment),
		pullRequests:            dependencies.PullRequests, credentials: dependencies.Credentials,
		pushURL: dependencies.PushURL, git: git,
	}, nil
}

// Publish publishes req exactly once or verifies and reuses exact existing
// branch and provider state.
func (p *Publisher) Publish(ctx context.Context, req publisher.Request) (result publisher.Result, returnErr error) {
	if err := ctx.Err(); err != nil {
		return publisher.Result{}, err
	}
	if err := req.Validate(); err != nil {
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
	defer func() {
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
	if err := p.runRequiredVerification(ctx, prepared.Path(), bundle.Verification); err != nil {
		return publisher.Result{}, err
	}

	pushURL, err := p.pushURL(req.Repository)
	if err != nil {
		return publisher.Result{}, fmt.Errorf("resolve publication push URL: %w", err)
	}
	if err := validatePushURL(pushURL); err != nil {
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

	commitSHA, exists, err := p.git.remoteBranch(ctx, prepared.Path(), pushURL, req.Branch, credentialEnvironment)
	if err != nil {
		return publisher.Result{}, markRetryable(fmt.Errorf("inspect publication branch: %w", err))
	}
	if exists {
		if err := p.git.verifyRemoteCommit(
			ctx, prepared.Path(), pushURL, req.Branch, commitSHA,
			req, bundle.Manifest.TreeSHA, credentialEnvironment,
		); err != nil {
			return publisher.Result{}, err
		}
	} else {
		commitSHA, err = p.git.commit(ctx, prepared.Path(), commitMessage(req))
		if err != nil {
			return publisher.Result{}, fmt.Errorf("create publication commit: %w", err)
		}
		if err := p.git.verifyLocalCommit(ctx, prepared.Path(), commitSHA, req, bundle.Manifest.TreeSHA); err != nil {
			return publisher.Result{}, err
		}
		if err := p.git.push(ctx, prepared.Path(), pushURL, req.Branch, credentialEnvironment); err != nil {
			inspectContext, cancelInspect := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			winner, winnerExists, inspectErr := p.git.remoteBranch(
				inspectContext, prepared.Path(), pushURL, req.Branch, credentialEnvironment,
			)
			cancelInspect()
			if inspectErr != nil {
				return publisher.Result{}, markRetryable(errors.Join(fmt.Errorf("push publication branch: %w", err), inspectErr))
			}
			if !winnerExists {
				return publisher.Result{}, markRetryable(fmt.Errorf("push publication branch: %w", err))
			}
			if winner != commitSHA {
				return publisher.Result{}, fmt.Errorf("%w: publication branch won by commit %s", publisher.ErrConflict, winner)
			}
			commitSHA = winner
		}
	}

	pullRequest, err := p.findOrCreatePullRequest(ctx, req, commitSHA)
	if err != nil {
		return publisher.Result{}, err
	}
	result = publisher.Result{
		Provider: "github", Branch: req.Branch, CommitSHA: commitSHA,
		PullRequestID: pullRequest.ID, PullRequestURL: pullRequest.URL,
	}
	if err := result.Validate(req); err != nil {
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
	}
	return bundle, nil
}

func (p *Publisher) runRequiredVerification(ctx context.Context, root string, evidence []verification.Result) error {
	for _, prior := range evidence {
		if !prior.Command.Required {
			continue
		}
		if !prior.Passed {
			return fmt.Errorf("%w: artifact records failed required verification %q", publisher.ErrConflict, prior.Command.Name)
		}
		command := prior.Command
		directory, err := containedDirectory(root, command.Directory)
		if err != nil {
			return fmt.Errorf("%w: reconstruct verification %q: %v", publisher.ErrConflict, command.Name, err)
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
		result := p.verification.Run(ctx, command, cloneStrings(p.verificationEnvironment))
		if !result.Passed {
			return fmt.Errorf("required publication verification %q failed: %s/%s", command.Name, result.FailureClass, result.CauseCode)
		}
	}
	return nil
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

func commitMessage(req publisher.Request) string {
	return req.Title + "\n\n" +
		"Paje-Run-ID: " + req.RunID + "\n" +
		"Paje-Base-SHA: " + req.BaseSHA + "\n" +
		"Paje-Artifact-Digest: " + req.Artifact.Digest
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

func validateVerificationEnvironment(environment map[string]string) error {
	for key, value := range environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("create Git PR publisher: invalid verification environment")
		}
		if strings.HasPrefix(key, "HATCHET_") || strings.HasPrefix(key, "MEM0_") ||
			strings.HasPrefix(key, "GITHUB_") || strings.HasPrefix(key, "PAJE_GIT_") ||
			key == "GH_TOKEN" || key == "GIT_ASKPASS" || key == "CODEX_HOME" {
			return fmt.Errorf("create Git PR publisher: verification environment contains service credential %q", key)
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

// IsRetryable reports whether err explicitly permits a publication retry.
func IsRetryable(err error) bool {
	var marker retryableMarker
	return errors.As(err, &marker) && marker.Retryable()
}
