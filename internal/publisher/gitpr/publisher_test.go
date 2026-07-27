package gitpr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	artifactfs "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/executor"
	executormock "github.com/araihu/paje/internal/executor/mock"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
	"github.com/araihu/paje/internal/workspace"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestPublisherVerifiesPersistedProfileInSecretFreeSandboxBeforeCredentials(t *testing.T) {
	fixture := newPublicationFixture(t)
	var eventsMu sync.Mutex
	var events []string
	var verificationWorkspace string
	fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, "verify")
		verificationWorkspace = request.Workspace.HostPath
		if request.Profile.Digest != fixture.request.WorkerProfile.Digest {
			t.Errorf("verification profile digest = %q, want %q", request.Profile.Digest, fixture.request.WorkerProfile.Digest)
		}
		if len(request.Secrets) != 0 || len(request.Environment) != 3 ||
			request.Environment["HOME"] != "/home/paje" ||
			request.Environment["PATH"] != "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin" ||
			request.Environment["TMPDIR"] != "/tmp" {
			t.Errorf("verification request carries ambient values or secrets: %#v", request)
		}
	})
	fixture.credentials.beforePrepare = func() {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		if verificationWorkspace == "" {
			t.Error("credentials prepared without a verification workspace")
		} else if _, err := os.Stat(verificationWorkspace); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("verification workspace still exists before credentials: %v", err)
		}
		events = append(events, "credentials")
	}

	if _, err := fixture.publisher.Publish(context.Background(), fixture.request); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if strings.Join(events, ",") != "verify,credentials" {
		t.Fatalf("events = %v, want verification before credentials", events)
	}
}

func TestPublisherVerificationAttemptIdentitiesAreDeterministicDistinctAndReplayStable(t *testing.T) {
	fixture := newPublicationFixture(t)
	request := fixture.requestWithVerificationCommands([]verification.Command{
		{
			Name: "go test", Directory: ".", Executable: "go",
			Args: []string{"test", "./..."}, EnvironmentKeys: []string{"GOWORK"}, Timeout: time.Minute, Required: true,
		},
		{
			Name: "go vet", Directory: ".", Executable: "go",
			Args: []string{"vet", "./..."}, EnvironmentKeys: []string{"GOWORK"}, Timeout: time.Minute, Required: true,
		},
	})

	first, err := fixture.publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if len(first.VerificationDigest) != 64 {
		t.Fatalf("publisher verification digest = %q", first.VerificationDigest)
	}
	firstRequests := fixture.executor.Requests()
	if len(firstRequests) != 2 {
		t.Fatalf("first verification requests = %d, want 2", len(firstRequests))
	}
	if firstRequests[0].Attempt.Stage != "publish-verification" ||
		firstRequests[1].Attempt.Stage != "publish-verification" ||
		firstRequests[0].Attempt.Sequence != 1 || firstRequests[1].Attempt.Sequence != 2 ||
		firstRequests[0].Attempt.Key() == firstRequests[1].Attempt.Key() {
		t.Fatalf("verification attempt identities = %#v, %#v", firstRequests[0].Attempt, firstRequests[1].Attempt)
	}

	second, err := fixture.publisher.Publish(context.Background(), request)
	if err != nil || second != first {
		t.Fatalf("replay Publish() result=%#v error=%v want=%#v", second, err, first)
	}
	allRequests := fixture.executor.Requests()
	if len(allRequests) != 4 ||
		allRequests[0].Attempt.Key() != allRequests[2].Attempt.Key() ||
		allRequests[1].Attempt.Key() != allRequests[3].Attempt.Key() {
		t.Fatalf("replay attempt identities = %#v", allRequests)
	}
}

func TestPublisherVerificationFailuresBlockCredentialPreparation(t *testing.T) {
	providerSecret := "provider-secret-must-not-be-reported"
	tests := []struct {
		name       string
		result     executor.Result
		executeErr error
		destroyErr error
	}{
		{
			name:   "nonzero",
			result: executor.Result{Created: true, Started: true, Completed: true, ExitCode: 1},
		},
		{
			name:       "provider error",
			executeErr: executor.WrapError("environment", "provider_unavailable", errors.New(providerSecret)),
		},
		{
			name:       "ambiguous attempt",
			result:     executor.Result{Created: true, Started: true},
			executeErr: executor.WrapError("internal", "ambiguous_attempt", errors.New(providerSecret)),
		},
		{
			name:       "timeout",
			result:     executor.Result{Created: true, Started: true},
			executeErr: context.DeadlineExceeded,
		},
		{
			name:       "cleanup failure",
			result:     executor.Result{Created: true, Started: true, Completed: true},
			destroyErr: errors.New("destroy failed"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			target := &faultExecutor{
				Executor: fixture.executor, result: test.result,
				executeErr: test.executeErr, destroyErr: test.destroyErr,
			}
			registry, err := executor.NewRegistry(executor.Registration{
				RuntimeKind: workerprofile.RuntimeHost, Executor: target,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.publisher.executors = registry

			if _, err := fixture.publisher.Publish(context.Background(), fixture.request); err == nil {
				t.Fatal("Publish() error = nil")
			} else if strings.Contains(err.Error(), providerSecret) {
				t.Fatalf("Publish() leaked provider detail: %v", err)
			}
			if got := fixture.credentials.PrepareCount(); got != 0 {
				t.Fatalf("credential preparations = %d, want 0", got)
			}
			if got := fixture.pullRequests.CreateCount(); got != 0 {
				t.Fatalf("pull request creates = %d, want 0", got)
			}
		})
	}
}

func TestPublisherVerificationWorkspaceCleanupFailureBlocksCredentials(t *testing.T) {
	fixture := newPublicationFixture(t)
	fixture.publisher.workspaces = cleanupFailureManager{
		Manager: fixture.publisher.workspaces, err: errors.New("cleanup not confirmed"),
	}

	if _, err := fixture.publisher.Publish(context.Background(), fixture.request); err == nil {
		t.Fatal("Publish() error = nil, want cleanup failure")
	}
	if got := fixture.credentials.PrepareCount(); got != 0 {
		t.Fatalf("credential preparations = %d, want 0", got)
	}
	if got := fixture.pullRequests.CreateCount(); got != 0 {
		t.Fatalf("pull request creates = %d, want 0", got)
	}
}

func TestPublisherRejectsPortableBindingDriftBeforeVerificationOrCredentials(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *publisher.Request)
	}{
		{
			name: "profile drift",
			mutate: func(t *testing.T, request *publisher.Request) {
				profile := request.WorkerProfile.Clone()
				profile.Metadata.Revision++
				profile.Digest = ""
				canonical, err := workerprofile.Canonicalize(profile)
				if err != nil {
					t.Fatal(err)
				}
				request.WorkerProfile = canonical
			},
		},
		{
			name: "artifact manifest drift",
			mutate: func(_ *testing.T, request *publisher.Request) {
				request.ArtifactManifest.TreeSHA = strings.Repeat("f", 40)
			},
		},
		{
			name: "execution evidence drift",
			mutate: func(_ *testing.T, request *publisher.Request) {
				request.ExecutionEvidence.Profile.Digest = strings.Repeat("f", 64)
			},
		},
		{
			name: "environment evidence drift",
			mutate: func(_ *testing.T, request *publisher.Request) {
				keys := artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR", "TOKEN"}
				request.ExecutionEvidence.VerificationEnvironmentKeys = &keys
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			request := publisher.CloneRequest(fixture.request)
			test.mutate(t, &request)
			if _, err := fixture.publisher.Publish(context.Background(), request); err == nil {
				t.Fatal("Publish(drift) error = nil")
			}
			if got := len(fixture.executor.Requests()); got != 0 {
				t.Fatalf("verification requests = %d, want 0", got)
			}
			if got := fixture.credentials.PrepareCount(); got != 0 {
				t.Fatalf("credential preparations = %d, want 0", got)
			}
		})
	}
}

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
	verificationDigest := verificationDigestFromCommitMessage(message)
	if len(verificationDigest) != 64 {
		t.Fatalf("publisher verification digest = %q", verificationDigest)
	}
	if first.VerificationDigest != verificationDigest {
		t.Fatalf("result verification digest = %q, commit receipt = %q", first.VerificationDigest, verificationDigest)
	}
	wantMessage := request.Title + "\n\n" +
		"Paje-Run-ID: " + request.RunID + "\n" +
		"Paje-Base-SHA: " + request.BaseSHA + "\n" +
		"Paje-Artifact-Digest: " + request.Artifact.Digest + "\n" +
		"Paje-Publisher-Verification-Digest: " + verificationDigest
	if message != wantMessage {
		t.Fatalf("commit message = %q, want %q", message, wantMessage)
	}
	if got := len(fixture.executor.Requests()); got != 1 {
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
	request.ArtifactManifest.Repository = request.Repository
	if _, err := fixture.publisher.Publish(context.Background(), request); !errors.Is(err, publisher.ErrConflict) {
		t.Fatalf("Publish(repository mismatch) error = %v, want ErrConflict", err)
	}
	if got := len(fixture.executor.Requests()); got != 0 {
		t.Fatalf("verification calls = %d, want 0", got)
	}
}

func TestPublisherFailsClosedWhenRequiredVerificationDoesNotPass(t *testing.T) {
	fixture := newPublicationFixture(t)
	attempt := publisherVerificationIdentity(fixture.request)
	attempt.Sequence = 1
	fixture.executor.SetResult(attempt, executor.Result{
		Created: true, Started: true, Completed: true, ExitCode: 1,
	}, nil)

	if _, err := fixture.publisher.Publish(context.Background(), fixture.request); err == nil {
		t.Fatal("Publish() error = nil, want required verification failure")
	}
	if got := fixture.git("--git-dir", fixture.remote, "show-ref", "--verify", "--quiet", "refs/heads/"+fixture.request.Branch); got != "missing" {
		t.Fatalf("remote publication branch = %q, want missing", got)
	}
	if got := fixture.pullRequests.CreateCount(); got != 0 {
		t.Fatalf("pull request creates = %d, want 0", got)
	}
	if got := fixture.credentials.PrepareCount(); got != 0 {
		t.Fatalf("credential preparations = %d, want 0", got)
	}
}

func TestNewRejectsMissingExecutorRegistryWithoutHostRunnerFallback(t *testing.T) {
	fixture := newPublicationFixture(t)
	dependencies := fixture.dependencies()
	dependencies.Executors = nil
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
	if got := len(fixture.executor.Requests()); got != 0 {
		t.Fatalf("verification calls = %d, want 0", got)
	}
}

func TestPublisherNeverUsesAgentRepositoryConfigWithCredentials(t *testing.T) {
	fixture := newPublicationFixture(t)
	var redirectedRequests atomic.Int64
	redirected := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="hostile"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	defer redirected.Close()

	hostileHelper := filepath.Join(t.TempDir(), "hostile-credential-helper")
	helperMarker := hostileHelper + ".executed"
	writeFile(t, hostileHelper, "#!/bin/sh\nprintf invoked >"+helperMarker+"\nexit 0\n")
	if err := os.Chmod(hostileHelper, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
		workspace := request.Workspace.HostPath
		runGit(t, workspace, "config", "--local", "credential.helper", hostileHelper)
		runGit(t, workspace, "config", "--local", "url."+redirected.URL+".insteadOf", fixture.remote)
		runGit(t, workspace, "config", "--local", "http.proxy", "http://127.0.0.1:1")
		runGit(t, workspace, "config", "--local", "http."+redirected.URL+".proxy", "")
		runGit(t, workspace, "config", "--local", "http.sslVerify", "false")
	})
	const token = "round-one-token-must-remain-private"
	fixture.publisher.credentials = newTokenCredentials(t, token)

	result, err := fixture.publisher.Publish(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("hostile redirected endpoint requests = %d, want 0", redirectedRequests.Load())
	}
	if _, err := os.Stat(helperMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository credential helper executed: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%#v", result), token) ||
		strings.Contains(fmt.Sprintf("%#v", fixture.pullRequests.LastRequest()), token) {
		t.Fatal("publication evidence disclosed credential")
	}
	if got := fixture.git("--git-dir", fixture.remote, "rev-parse", "refs/heads/"+fixture.request.Branch); got != result.CommitSHA {
		t.Fatalf("intended remote branch = %q, want %q", got, result.CommitSHA)
	}
}

func TestGitClientRunPreservesCancellationIdentity(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	runGit(t, "", "init", "--bare", repository)
	blocker := filepath.Join(root, "blocking-git")
	writeFile(t, blocker, "#!/bin/sh\nsleep 10\n")
	if err := os.Chmod(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	client := &gitClient{command: blocker, baseEnv: map[string]string{"PATH": os.Getenv("PATH")}}
	tests := []struct {
		name string
		args []string
	}{
		{name: "remote inspection", args: []string{"ls-remote", "--heads", "remote", "refs/heads/main"}},
		{name: "fetch import", args: []string{"fetch", "--no-tags", "source", "HEAD:refs/heads/candidate"}},
		{name: "commit", args: []string{"commit", "-m", "message"}},
		{name: "push", args: []string{"push", "remote", "HEAD:refs/heads/main"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			result := client.run(ctx, repository, nil, test.args...)
			if !errors.Is(result.err, context.DeadlineExceeded) {
				t.Fatalf("run() error = %v, want DeadlineExceeded identity", result.err)
			}
			classified := retryRemoteError(fmt.Errorf("remote operation: %w", result.err))
			if !errors.Is(classified, context.DeadlineExceeded) ||
				errors.Is(classified, publisher.ErrConflict) || IsRetryable(classified) {
				t.Fatalf("canceled run misclassified: %v", classified)
			}
		})
	}
}

func TestCommitVerificationCancellationIsNotConflict(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	runGit(t, "", "init", "--bare", repository)
	blocker := filepath.Join(root, "blocking-git")
	writeFile(t, blocker, "#!/bin/sh\nsleep 10\n")
	if err := os.Chmod(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	client := &gitClient{command: blocker, baseEnv: map[string]string{"PATH": os.Getenv("PATH")}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := client.verifyLocalCommit(
		ctx, repository, strings.Repeat("a", 40),
		publisher.Request{BaseSHA: strings.Repeat("b", 40)},
		strings.Repeat("c", 40),
	)
	if !errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, publisher.ErrConflict) || IsRetryable(err) {
		t.Fatalf("verifyLocalCommit() cancellation misclassified: %v", err)
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
	pullRequests *recordingPullRequests
	credentials  *recordingCredentials
	executor     *executormock.Executor
	profile      workerprofile.Snapshot
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
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "host-dev", Revision: 1},
		Runtime:    workerprofile.Runtime{Kind: workerprofile.RuntimeHost},
		Harness:    workerprofile.Harness{ID: "codex", Version: "0.144.5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	targetExecutor := executormock.New()
	executors, err := executor.NewRegistry(executor.Registration{
		RuntimeKind: workerprofile.RuntimeHost, Executor: targetExecutor,
	})
	if err != nil {
		t.Fatal(err)
	}
	pulls := &recordingPullRequests{remote: remote}
	credentials := &recordingCredentials{}

	fixture := &publicationFixture{
		t: t, remote: remote, seed: seed, baseSHA: base, store: store,
		capturer: capturer, executor: targetExecutor, profile: profile,
		pullRequests: pulls, credentials: credentials,
	}
	ref := fixture.captureArtifact("updated")
	bundle, err := store.Load(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	executionEvidence, err := publisher.DecodeExecutionEvidence(bundle.ExecutionMetadata)
	if err != nil {
		t.Fatal(err)
	}
	fixture.request = publisher.Request{
		RunID: "run-123", Repository: remote, BaseSHA: base, TargetRef: "main",
		Branch: "paje/code-change/run-123", Artifact: ref,
		ArtifactManifest: bundle.Manifest, WorkerProfile: profile,
		ExecutionEvidence: executionEvidence,
		Verification:      publisher.CloneVerification(bundle.Verification),
		Title:             "Update value", Body: "Generated safely by Pajé.", Draft: true,
	}
	fixture.publisher, err = New(Dependencies{
		Artifacts: store, Workspaces: manager, Changes: capturer, Executors: executors,
		PullRequests: pulls, Credentials: credentials,
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
		Executors:    f.publisher.executors,
		PullRequests: f.pullRequests, Credentials: f.credentials,
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
	tools := artifact.ToolEvidenceList{}
	attempts := artifact.AttemptEvidenceList{{
		ID: executor.AttemptID{
			RunID: "run-123", Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeAgent,
		},
		Created: true, Started: true, Completed: true, Destroyed: true,
	}, {
		ID: executor.AttemptID{
			RunID: "run-123", Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeVerification, Sequence: 1,
		},
		Created: true, Started: true, Completed: true, Destroyed: true,
	}}
	agentEnvironment := artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"}
	verificationEnvironment := artifact.EnvironmentKeyList{"GOWORK", "HOME", "PATH", "TMPDIR"}
	execution, err := json.Marshal(artifact.ExecutionEvidence{
		ExitCode: 0, Duration: 1, Started: true, Completed: true,
		Profile: &artifact.WorkerProfileEvidence{
			Name: f.profile.Metadata.Name, Revision: f.profile.Metadata.Revision, Digest: f.profile.Digest,
		},
		Runtime: &artifact.RuntimeEvidence{Kind: workerprofile.RuntimeHost},
		Harness: &artifact.HarnessEvidence{
			ID: f.profile.Harness.ID, DeclaredVersion: f.profile.Harness.Version,
			ProbedVersion: f.profile.Harness.Version, ProbePassed: true,
		},
		Tools: &tools, Attempts: &attempts,
		AgentEnvironmentKeys:        &agentEnvironment,
		VerificationEnvironmentKeys: &verificationEnvironment,
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
				Args: []string{"test", "./..."}, EnvironmentKeys: []string{"GOWORK"}, Timeout: time.Minute, Required: true,
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

func (f *publicationFixture) requestWithVerificationCommands(commands []verification.Command) publisher.Request {
	f.t.Helper()
	bundle, err := f.store.Load(context.Background(), f.request.Artifact)
	if err != nil {
		f.t.Fatal(err)
	}
	bundle.Verification = make([]verification.Result, len(commands))
	for index, command := range commands {
		if command.EnvironmentKeys == nil {
			command.EnvironmentKeys = []string{}
		}
		bundle.Verification[index] = verification.Result{Command: command, Passed: true}
	}
	evidence, err := publisher.DecodeExecutionEvidence(bundle.ExecutionMetadata)
	if err != nil {
		f.t.Fatal(err)
	}
	attempts := artifact.AttemptEvidenceList{}
	for _, attempt := range *evidence.Attempts {
		if attempt.ID.Purpose != executor.PurposeVerification {
			attempts = append(attempts, attempt)
		}
	}
	verificationKeys := map[string]struct{}{}
	for index, command := range commands {
		attempt := confirmedVerificationAttemptForFixture(attempts[0].ID, index+1)
		attempts = append(attempts, attempt)
		verificationKeys["HOME"] = struct{}{}
		verificationKeys["PATH"] = struct{}{}
		verificationKeys["TMPDIR"] = struct{}{}
		for _, key := range command.EnvironmentKeys {
			verificationKeys[key] = struct{}{}
		}
	}
	keys := make(artifact.EnvironmentKeyList, 0, len(verificationKeys))
	for key := range verificationKeys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	evidence.Attempts = &attempts
	evidence.VerificationEnvironmentKeys = &keys
	bundle.ExecutionMetadata, err = json.Marshal(evidence)
	if err != nil {
		f.t.Fatal(err)
	}
	reference, err := f.store.Save(context.Background(), bundle)
	if err != nil {
		f.t.Fatal(err)
	}
	bound, err := f.store.Load(context.Background(), reference)
	if err != nil {
		f.t.Fatal(err)
	}
	evidence, err = publisher.DecodeExecutionEvidence(bound.ExecutionMetadata)
	if err != nil {
		f.t.Fatal(err)
	}
	request := publisher.CloneRequest(f.request)
	request.Artifact = reference
	request.ArtifactManifest = bound.Manifest
	request.ExecutionEvidence = evidence
	request.Verification = publisher.CloneVerification(bound.Verification)
	return request
}

func confirmedVerificationAttemptForFixture(
	agent executor.AttemptID,
	sequence int,
) artifact.AttemptEvidence {
	agent.Purpose = executor.PurposeVerification
	agent.Sequence = sequence
	return artifact.AttemptEvidence{
		ID: agent, Created: true, Started: true, Completed: true, Destroyed: true,
	}
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

type recordingPullRequests struct {
	mu      sync.Mutex
	remote  string
	last    PullRequestRequest
	created *PullRequest
	creates int
}

type faultExecutor struct {
	*executormock.Executor
	mu         sync.Mutex
	result     executor.Result
	executeErr error
	destroyErr error
	requests   []executor.Request
	destroys   []executor.AttemptID
}

func (target *faultExecutor) Execute(ctx context.Context, request executor.Request) (executor.Result, error) {
	if err := ctx.Err(); err != nil {
		return executor.Result{}, err
	}
	if err := request.Validate(); err != nil {
		return executor.Result{}, err
	}
	target.mu.Lock()
	target.requests = append(target.requests, request.Clone())
	result, executeErr := target.result.Clone(), target.executeErr
	target.mu.Unlock()
	return result, executeErr
}

func (target *faultExecutor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := attempt.Validate(); err != nil {
		return err
	}
	target.mu.Lock()
	target.destroys = append(target.destroys, attempt)
	err := target.destroyErr
	target.mu.Unlock()
	return err
}

type cleanupFailureManager struct {
	workspace.Manager
	err error
}

func (manager cleanupFailureManager) Prepare(
	ctx context.Context,
	repository string,
	baseSHA string,
) (workspace.Workspace, error) {
	prepared, err := manager.Manager.Prepare(ctx, repository, baseSHA)
	if err != nil {
		return nil, err
	}
	return cleanupFailureWorkspace{Workspace: prepared, err: manager.err}, nil
}

type cleanupFailureWorkspace struct {
	workspace.Workspace
	err error
}

func (prepared cleanupFailureWorkspace) Cleanup(ctx context.Context) error {
	return errors.Join(prepared.Workspace.Cleanup(ctx), prepared.err)
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
	mu            sync.Mutex
	cleanups      int
	preparations  int
	beforePrepare func()
}

func (c *recordingCredentials) Prepare(context.Context) (map[string]string, func(context.Context) error, error) {
	c.mu.Lock()
	c.preparations++
	beforePrepare := c.beforePrepare
	c.mu.Unlock()
	if beforePrepare != nil {
		beforePrepare()
	}
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

type tokenCredentials struct {
	helper string
	token  string
}

func newTokenCredentials(t *testing.T, token string) tokenCredentials {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "askpass")
	writeFile(t, helper, "#!/bin/sh\ncase \"$1\" in\n*Username*|*username*) printf '%s\\n' \"$PAJE_GIT_USERNAME\" ;;\n*) printf '%s\\n' \"$PAJE_GIT_PASSWORD\" ;;\nesac\n")
	if err := os.Chmod(helper, 0o700); err != nil {
		t.Fatal(err)
	}
	return tokenCredentials{helper: helper, token: token}
}

func (c tokenCredentials) Prepare(context.Context) (map[string]string, func(context.Context) error, error) {
	environment := map[string]string{
		"GIT_ASKPASS": c.helper, "GIT_TERMINAL_PROMPT": "0",
		"PAJE_GIT_USERNAME": "x-access-token", "PAJE_GIT_PASSWORD": c.token,
	}
	return environment, func(context.Context) error {
		environment["PAJE_GIT_PASSWORD"] = ""
		return nil
	}, nil
}

func (c *recordingCredentials) CleanupCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cleanups
}

func (c *recordingCredentials) PrepareCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.preparations
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

func verificationDigestFromCommitMessage(message string) string {
	const prefix = "Paje-Publisher-Verification-Digest: "
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}
