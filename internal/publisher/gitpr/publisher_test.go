package gitpr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	artifactfs "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestPublisherPublishesArtifactAndReusesExactRemoteState(t *testing.T) {
	fixture := newPublicationFixture(t)
	request := fixture.request

	first, err := fixture.publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if first.Provider != "github" || first.Branch != request.Branch ||
		first.PullRequestID != "17" || first.PullRequestURL != "https://github.com/araihu/paje/pull/17" {
		t.Fatalf("first result = %#v", first)
	}

	remoteCommit := fixture.git("--git-dir", fixture.remote, "rev-parse", "refs/heads/"+request.Branch)
	if first.CommitSHA != remoteCommit {
		t.Fatalf("CommitSHA = %q, remote branch = %q", first.CommitSHA, remoteCommit)
	}
	if got := fixture.git("--git-dir", fixture.remote, "show", "-s", "--format=%P", remoteCommit); got != fixture.baseSHA {
		t.Fatalf("commit parent = %q, want %q", got, fixture.baseSHA)
	}
	if got := fixture.git("--git-dir", fixture.remote, "show", "-s", "--format=%T", remoteCommit); got != fixture.treeSHA {
		t.Fatalf("commit tree = %q, want artifact tree %q", got, fixture.treeSHA)
	}
	message := fixture.git("--git-dir", fixture.remote, "show", "-s", "--format=%B", remoteCommit)
	wantMessage := request.Title + "\n\n" +
		"Paje-Run-ID: " + request.RunID + "\n" +
		"Paje-Base-SHA: " + request.BaseSHA + "\n" +
		"Paje-Artifact-Digest: " + request.Artifact.Digest
	if message != wantMessage {
		t.Fatalf("commit message = %q, want %q", message, wantMessage)
	}
	if got := fixture.verifier.CallCount(); got != 1 {
		t.Fatalf("required verification calls = %d, want 1", got)
	}
	if got := fixture.pullRequests.CreateCount(); got != 1 {
		t.Fatalf("pull request creates = %d, want 1", got)
	}
	if got := fixture.pullRequests.LastRequest(); got != (PullRequestRequest{
		Repository: request.Repository,
		Head:       request.Branch,
		Base:       request.TargetRef,
		Title:      request.Title,
		Body:       request.Body,
		Draft:      request.Draft,
	}) {
		t.Fatalf("pull request request = %#v", got)
	}

	second, err := fixture.publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	if second != first {
		t.Fatalf("second result = %#v, want %#v", second, first)
	}
	if got := fixture.git("--git-dir", fixture.remote, "rev-list", "--count", "refs/heads/"+request.Branch); got != "2" {
		t.Fatalf("remote branch commit count = %s, want base plus one publication", got)
	}
	if got := fixture.pullRequests.CreateCount(); got != 1 {
		t.Fatalf("pull request creates after retry = %d, want 1", got)
	}
	if got := fixture.credentials.CleanupCount(); got != 2 {
		t.Fatalf("credential cleanups = %d, want one per Publish", got)
	}
}

func TestPublisherRejectsChangedArtifactForExistingBranch(t *testing.T) {
	fixture := newPublicationFixture(t)
	if _, err := fixture.publisher.Publish(context.Background(), fixture.request); err != nil {
		t.Fatalf("initial Publish() error = %v", err)
	}

	changed := fixture.captureArtifact("alternate")
	request := fixture.request
	request.Artifact = changed
	if _, err := fixture.publisher.Publish(context.Background(), request); !errors.Is(err, publisher.ErrConflict) {
		t.Fatalf("Publish(changed artifact) error = %v, want ErrConflict", err)
	}
}

func TestPublisherRejectsConflictingRemoteBranch(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.git("-C", fixture.seed, "checkout", "--", ".")
	if err := os.WriteFile(filepath.Join(fixture.seed, "unrelated.txt"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.git("-C", fixture.seed, "add", "unrelated.txt")
	fixture.git("-C", fixture.seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "commit", "-m", "unrelated")
	fixture.git("-C", fixture.seed, "push", "origin", "HEAD:refs/heads/"+fixture.request.Branch)

	if _, err := fixture.publisher.Publish(context.Background(), fixture.request); !errors.Is(err, publisher.ErrConflict) {
		t.Fatalf("Publish(conflicting branch) error = %v, want ErrConflict", err)
	}
	if got := fixture.pullRequests.CreateCount(); got != 0 {
		t.Fatalf("pull request creates = %d, want 0", got)
	}
}

func TestPublisherRejectsArtifactBindingMismatchBeforeSideEffects(t *testing.T) {
	fixture := newPublicationFixture(t)
	request := fixture.request
	request.Repository = filepath.Join(t.TempDir(), "other.git")
	if _, err := fixture.publisher.Publish(context.Background(), request); !errors.Is(err, publisher.ErrConflict) {
		t.Fatalf("Publish(repository mismatch) error = %v, want ErrConflict", err)
	}
	if got := fixture.verifier.CallCount(); got != 0 {
		t.Fatalf("verification calls = %d, want 0", got)
	}
}

func TestPublisherFailsClosedWhenRequiredVerificationDoesNotPass(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.verifier.forceFailure = true

	if _, err := fixture.publisher.Publish(context.Background(), fixture.request); err == nil {
		t.Fatal("Publish() error = nil, want required verification failure")
	}
	if got := fixture.git("--git-dir", fixture.remote, "show-ref", "--verify", "--quiet", "refs/heads/"+fixture.request.Branch); got != "missing" {
		t.Fatalf("remote publication branch = %q, want missing", got)
	}
	if got := fixture.pullRequests.CreateCount(); got != 0 {
		t.Fatalf("pull request creates = %d, want 0", got)
	}
}

func TestNewRejectsServiceCredentialsInVerificationEnvironment(t *testing.T) {
	fixture := newPublicationFixture(t)
	dependencies := fixture.dependencies()
	dependencies.VerificationEnvironment["GH_TOKEN"] = "must-not-reach-repository-code"
	if got, err := New(dependencies); err == nil {
		t.Fatalf("New() = %#v, nil error", got)
	}
}

func TestPublisherRejectsPartialCredentialEnvironment(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.publisher.credentials = partialCredentials{}
	if _, err := fixture.publisher.Publish(context.Background(), fixture.request); err == nil {
		t.Fatal("Publish() error = nil, want incomplete credential environment rejection")
	}
}

func TestPublisherRejectsMultilineTitleBeforeArtifactSideEffects(t *testing.T) {
	fixture := newPublicationFixture(t)
	request := fixture.request
	request.Title = "legitimate\n\nPaje-Artifact-Digest: attacker-controlled"
	if _, err := fixture.publisher.Publish(context.Background(), request); !errors.Is(err, publisher.ErrInvalidRequest) {
		t.Fatalf("Publish(multiline title) error = %v, want ErrInvalidRequest", err)
	}
	if got := fixture.verifier.CallCount(); got != 0 {
		t.Fatalf("verification calls = %d, want 0", got)
	}
}

type publicationFixture struct {
	t            *testing.T
	remote       string
	seed         string
	baseSHA      string
	treeSHA      string
	store        *artifactfs.Store
	capturer     *gitcapture.Git
	verifier     *recordingVerifier
	pullRequests *recordingPullRequests
	credentials  *recordingCredentials
	publisher    *Publisher
	request      publisher.Request
}

func newPublicationFixture(t *testing.T) *publicationFixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	runGit(t, "", "init", "--bare", remote)
	runGit(t, "", "init", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "go.mod"), "module example.com/publication\n\ngo 1.26.1\n")
	writeFile(t, filepath.Join(seed, "value.go"), "package publication\n\nconst Value = \"base\"\n")
	writeFile(t, filepath.Join(seed, "value_test.go"), "package publication\n\nimport \"testing\"\n\nfunc TestValueChanged(t *testing.T) { if Value == \"base\" { t.Fatal(\"artifact was not applied\") } }\n")
	runGit(t, seed, "add", ".")
	runGit(t, seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "commit", "-m", "base")
	base := runGit(t, seed, "rev-parse", "HEAD")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifactfs.New(filepath.Join(root, "artifacts"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager, err := gitworktree.New(filepath.Join(root, "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	executor, err := verification.NewExecutor(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &recordingVerifier{delegate: executor}
	pulls := &recordingPullRequests{remote: remote}
	credentials := &recordingCredentials{}

	fixture := &publicationFixture{
		t: t, remote: remote, seed: seed, baseSHA: base, store: store,
		capturer: capturer, verifier: verifier, pullRequests: pulls, credentials: credentials,
	}
	ref := fixture.captureArtifact("updated")
	fixture.request = publisher.Request{
		RunID: "run-123", Repository: remote, BaseSHA: base, TargetRef: "main",
		Branch: "paje/code-change/run-123", Artifact: ref,
		Title: "Update value", Body: "Generated safely by Pajé.", Draft: true,
	}
	fixture.publisher, err = New(Dependencies{
		Artifacts: store, Workspaces: manager, Changes: capturer, Verification: verifier,
		VerificationEnvironment: map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")},
		PullRequests:            pulls, Credentials: credentials,
		PushURL: func(repository string) (string, error) { return repository, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *publicationFixture) dependencies() Dependencies {
	return Dependencies{
		Artifacts: f.store, Workspaces: f.publisher.workspaces, Changes: f.capturer,
		Verification:            f.verifier,
		VerificationEnvironment: map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")},
		PullRequests:            f.pullRequests, Credentials: f.credentials,
		PushURL: func(repository string) (string, error) { return repository, nil },
	}
}

func (f *publicationFixture) captureArtifact(value string) artifact.Reference {
	f.t.Helper()
	f.git("-C", f.seed, "reset", "--hard", f.baseSHA)
	writeFile(f.t, filepath.Join(f.seed, "value.go"), fmt.Sprintf("package publication\n\nconst Value = %q\n", value))
	captured, err := f.capturer.Capture(context.Background(), gitcapture.Request{
		Workspace: f.seed, BaseSHA: f.baseSHA, MaxBytes: 1 << 20,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.treeSHA = captured.TreeSHA
	execution, err := json.Marshal(artifact.ExecutionEvidence{
		ExitCode: 0, Duration: 1, Started: true, Completed: true,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	bundle := artifact.Bundle{
		Manifest: artifact.Manifest{
			RunID: "run-123", Template: template.ID{Name: "code-change", Version: 1},
			Repository: f.remote, BaseSHA: f.baseSHA, TreeSHA: captured.TreeSHA,
			Changes: captured.Changes,
		},
		ChangesPatch: captured.Patch, AgentOutput: []byte("changed value"),
		ExecutionMetadata: execution,
		Verification: []verification.Result{{
			Command: verification.Command{
				Name: "go test", Directory: ".", Executable: "go",
				Args: []string{"test", "./..."}, Timeout: time.Minute, Required: true,
			},
			ExitCode: 0, Passed: true,
		}},
		Preflight: map[string]string{"base_sha": f.baseSHA},
	}
	ref, err := f.store.Save(context.Background(), bundle)
	if err != nil {
		f.t.Fatal(err)
	}
	return ref
}

func (f *publicationFixture) git(args ...string) string {
	f.t.Helper()
	if len(args) >= 5 && args[0] == "--git-dir" && args[2] == "show-ref" {
		cmd := exec.Command("git", args...)
		if err := cmd.Run(); err != nil {
			return "missing"
		}
		return "present"
	}
	directory := ""
	if len(args) > 1 && args[0] == "-C" {
		directory = args[1]
		args = args[2:]
	}
	return runGit(f.t, directory, args...)
}

type recordingVerifier struct {
	mu           sync.Mutex
	delegate     verification.Runner
	calls        int
	forceFailure bool
}

func (v *recordingVerifier) Run(ctx context.Context, command verification.Command, environment map[string]string) verification.Result {
	v.mu.Lock()
	v.calls++
	forceFailure := v.forceFailure
	v.mu.Unlock()
	if filepath.Base(command.Executable) == "go" && command.Environment["GOWORK"] != "off" {
		return verification.Result{Command: command, ExitCode: 1, Passed: false, FailureClass: "internal", CauseCode: "missing_gowork_off"}
	}
	if forceFailure {
		return verification.Result{Command: command, ExitCode: 1, Passed: false, FailureClass: "verification", CauseCode: "nonzero_exit"}
	}
	return v.delegate.Run(ctx, command, environment)
}

func (v *recordingVerifier) CallCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

type recordingPullRequests struct {
	mu      sync.Mutex
	remote  string
	last    PullRequestRequest
	created *PullRequest
	creates int
}

func (p *recordingPullRequests) Find(_ context.Context, request PullRequestRequest) (*PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = request
	if p.created == nil {
		return nil, nil
	}
	copy := *p.created
	return &copy, nil
}

func (p *recordingPullRequests) Create(_ context.Context, request PullRequestRequest) (PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = request
	p.creates++
	sha := strings.TrimSpace(string(mustOutput(p.remote, "--git-dir", p.remote, "rev-parse", "refs/heads/"+request.Head)))
	created := PullRequest{ID: "17", URL: "https://github.com/araihu/paje/pull/17", HeadSHA: sha}
	p.created = &created
	return created, nil
}

func (p *recordingPullRequests) CreateCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates
}

func (p *recordingPullRequests) LastRequest() PullRequestRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

type recordingCredentials struct {
	mu       sync.Mutex
	cleanups int
}

func (c *recordingCredentials) Prepare(context.Context) (map[string]string, func(context.Context) error, error) {
	return map[string]string{}, func(context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.cleanups++
		return nil
	}, nil
}

type partialCredentials struct{}

func (partialCredentials) Prepare(context.Context) (map[string]string, func(context.Context) error, error) {
	return map[string]string{"GIT_TERMINAL_PROMPT": "0"}, func(context.Context) error { return nil }, nil
}

func (c *recordingCredentials) CleanupCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cleanups
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func mustOutput(directory string, args ...string) []byte {
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	output, err := cmd.Output()
	if err != nil {
		panic(err)
	}
	return output
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
