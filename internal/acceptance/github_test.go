package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	artifactfilesystem "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/publisher"
	githubpublisher "github.com/araihu/paje/internal/publisher/github"
	"github.com/araihu/paje/internal/publisher/gitpr"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestGitHubPublicationAcceptance(t *testing.T) {
	requireOptIn(t, "PAJE_GITHUB_ACCEPTANCE", "the live GitHub publication acceptance test")
	variables := requireEnvironment(t,
		"PAJE_GITHUB_TOKEN",
		"PAJE_GITHUB_TEST_REPOSITORY",
		"PAJE_GITHUB_TEST_BASE_REF",
		"PAJE_GITHUB_TEST_RUN_ID",
	)
	token := variables["PAJE_GITHUB_TOKEN"]
	repositoryURI := variables["PAJE_GITHUB_TEST_REPOSITORY"]
	baseRef := variables["PAJE_GITHUB_TEST_BASE_REF"]
	runID := variables["PAJE_GITHUB_TEST_RUN_ID"]
	branch := "paje/code-change/" + runID

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	httpClient := &http.Client{
		Timeout: 45 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	api := githubAPI{
		baseURL: "https://api.github.com", repository: repositoryURI,
		token: token, client: httpClient,
	}
	baseBefore := api.refSHA(t, baseRef)

	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspaces")
	workspaces, err := gitworktree.New(workspaceRoot)
	if err != nil {
		t.Fatalf("create GitHub acceptance workspace manager: %v", err)
	}
	revision, err := workspaces.Resolve(ctx, repositoryURI, baseRef)
	if err != nil {
		t.Fatalf("resolve dedicated GitHub acceptance repository: %v", err)
	}
	if revision.SHA != baseBefore {
		t.Fatalf("resolved base SHA = %q, GitHub base SHA = %q", revision.SHA, baseBefore)
	}
	prepared, err := workspaces.Prepare(ctx, repositoryURI, revision.SHA)
	if err != nil {
		t.Fatalf("prepare GitHub acceptance artifact workspace: %v", err)
	}
	writeFile(t, filepath.Join(prepared.Path(), "paje-beta-acceptance.txt"),
		"Pajé beta acceptance run: "+runID+"\n")

	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatalf("create Git capture adapter: %v", err)
	}
	limits := verification.DefaultLimits
	verifier, err := verification.NewExecutor(limits)
	if err != nil {
		t.Fatalf("create publication verifier: %v", err)
	}
	verificationEnvironment := baselineEnvironment()
	check := verification.Command{
		Name: "acceptance file exists", Directory: prepared.Path(),
		Executable: "test", Args: []string{"-f", "paje-beta-acceptance.txt"},
		Timeout: time.Minute, Required: true,
	}
	checkResult := verifier.Run(ctx, check, verificationEnvironment)
	if !checkResult.Passed {
		t.Fatalf("acceptance artifact verification failed: %s/%s", checkResult.FailureClass, checkResult.CauseCode)
	}
	// Wall-clock duration proves execution locally but cannot participate in the
	// stable cross-process publication identity used by this acceptance test.
	checkResult.Duration = 0
	checkResult.Command.Directory = "."
	captured, err := capturer.Capture(ctx, gitcapture.Request{
		Workspace: prepared.Path(), BaseSHA: revision.SHA, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("capture GitHub acceptance artifact: %v", err)
	}
	executionMetadata, err := json.Marshal(artifact.ExecutionEvidence{
		ExitCode: 0, Duration: 0, Started: true, Completed: true,
	})
	if err != nil {
		t.Fatalf("encode GitHub acceptance execution metadata: %v", err)
	}
	artifactStore, err := artifactfilesystem.New(filepath.Join(root, "artifacts"), 10<<20)
	if err != nil {
		t.Fatalf("create GitHub acceptance artifact store: %v", err)
	}
	t.Cleanup(func() { _ = artifactStore.Close() })
	reference, err := artifactStore.Save(ctx, artifact.Bundle{
		Manifest: artifact.Manifest{
			RunID: runID, Template: template.ID{Name: "code-change", Version: 1},
			Repository: repositoryURI, BaseSHA: revision.SHA, TreeSHA: captured.TreeSHA,
			Changes: captured.Changes,
		},
		ChangesPatch: captured.Patch, AgentOutput: []byte("deterministic acceptance change"),
		ExecutionMetadata: executionMetadata,
		Verification:      []verification.Result{checkResult},
		Preflight:         map[string]string{"base_sha": revision.SHA},
		Warnings:          []string{},
	})
	if err != nil {
		t.Fatalf("persist GitHub acceptance artifact: %v", err)
	}
	if err := prepared.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup GitHub acceptance artifact workspace: %v", err)
	}

	pullRequests, err := githubpublisher.NewClient("https://api.github.com", token, httpClient)
	if err != nil {
		t.Fatalf("create GitHub pull request client: %v", err)
	}
	credentialRoot := filepath.Join(root, "publisher-credentials")
	credentials, err := githubpublisher.NewCredentials(credentialRoot, token)
	if err != nil {
		t.Fatalf("create isolated GitHub credentials: %v", err)
	}
	publication, err := gitpr.New(gitpr.Dependencies{
		Artifacts: artifactStore, Workspaces: workspaces, Changes: capturer,
		Verification: verifier, VerificationEnvironment: verificationEnvironment,
		PullRequests: pullRequests, Credentials: credentials, PushURL: githubpublisher.PushURL,
	})
	if err != nil {
		t.Fatalf("create GitHub Git-PR publisher: %v", err)
	}
	request := publisher.Request{
		RunID: runID, Repository: repositoryURI, BaseSHA: revision.SHA,
		TargetRef: baseRef, Branch: branch, Artifact: reference,
		Title: "Pajé beta acceptance " + runID,
		Body:  "Dedicated Pajé beta publication acceptance. Do not merge.",
		Draft: true,
	}

	first, err := publication.Publish(ctx, request)
	safeError(t, "first live GitHub Publish", err)
	firstRemote := api.inspectPublication(t, branch, baseRef, revision.SHA)
	assertPublicationBinding(t, request, first, firstRemote)
	assertDirectoryEmpty(t, credentialRoot)
	assertDirectoryEmpty(t, filepath.Join(workspaceRoot, "worktrees"))

	second, err := publication.Publish(ctx, request)
	safeError(t, "second live GitHub Publish", err)
	secondRemote := api.inspectPublication(t, branch, baseRef, revision.SHA)
	assertPublicationBinding(t, request, second, secondRemote)
	if second != first {
		t.Fatalf("second publication result = %#v, want exact first result %#v", second, first)
	}
	if secondRemote != firstRemote {
		t.Fatalf("remote publication changed on retry: first=%#v second=%#v", firstRemote, secondRemote)
	}
	if baseAfter := api.refSHA(t, baseRef); baseAfter != baseBefore {
		t.Fatalf("target base ref changed from %q to %q", baseBefore, baseAfter)
	}
	assertDirectoryEmpty(t, credentialRoot)
	assertDirectoryEmpty(t, filepath.Join(workspaceRoot, "worktrees"))
}

func assertPublicationBinding(
	t *testing.T,
	request publisher.Request,
	result publisher.Result,
	remote remotePublication,
) {
	t.Helper()
	if result.Provider != "github" || result.Branch != request.Branch ||
		result.CommitSHA != remote.commitSHA || result.PullRequestID != remote.pullRequestID ||
		result.PullRequestURL != remote.pullRequestURL {
		t.Fatalf("publisher result does not match exact remote state: result=%#v remote=%#v", result, remote)
	}
	wantMessage := request.Title + "\n\n" +
		"Paje-Run-ID: " + request.RunID + "\n" +
		"Paje-Base-SHA: " + request.BaseSHA + "\n" +
		"Paje-Artifact-Digest: " + request.Artifact.Digest
	if remote.commitMessage != wantMessage {
		t.Fatalf("remote commit message does not exactly match the deterministic title and trailers")
	}
}
