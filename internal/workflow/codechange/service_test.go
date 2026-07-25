package codechange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	approvalmock "github.com/araihu/paje/internal/approval/mock"
	"github.com/araihu/paje/internal/artifact"
	artifactfilesystem "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/publisher"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	runfilesystem "github.com/araihu/paje/internal/run/filesystem"
	runmock "github.com/araihu/paje/internal/run/mock"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestServiceArtifactOnlyRealGitFlowReloadsFilesystemStores(t *testing.T) {
	source, baseSHA := createGitSource(t)
	managerRoot := t.TempDir()
	manager, err := gitworktree.New(managerRoot)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := t.TempDir()
	envPolicy, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: runtimeRoot, Source: map[string]string{"PATH": os.Getenv("PATH")},
		CodexHome:  t.TempDir(),
		CodexAgent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	changePolicy, err := policy.NewChangePolicy(policy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	runRoot, artifactRoot := t.TempDir(), t.TempDir()
	runs, err := runfilesystem.New(runRoot)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactfilesystem.New(artifactRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := &outcomeMemory{}
	agent := &writingAgent{}
	profile := &workspaceProfile{}
	verifier := &recordingVerifier{}
	pub := publishermock.NewPublisher(publisher.Result{}, errors.New("must not publish"))
	gate := approvalmock.NewGate(approval.Result{}, errors.New("must not request approval"))
	now := time.Unix(100, 0).UTC()
	clock := func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	dependencies := Dependencies{
		Templates: registry, Runs: runs, Memory: outcomes, Resolver: manager,
		Workspaces: manager, Profiles: map[string]repository.Profile{
			"generic": profile, "go": &fakeProfile{name: "go"},
		},
		Environments: envPolicy, Agent: agent, Verifier: verifier,
		Capturer: capturer, Policy: changePolicy, Artifacts: artifacts,
		Publisher: pub, Clock: clock, NewID: func() string { return "run-real" },
	}
	service, err := New(dependencies)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := service.Resolve(context.Background(), rawForRepository(source))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if _, err := service.Execute(context.Background(), resolved.RunID); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := service.Approval(context.Background(), resolved.RunID, gate); err != nil {
		t.Fatalf("Approval() error = %v", err)
	}
	if _, err := service.Publish(context.Background(), resolved.RunID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	want, err := service.Finalize(context.Background(), resolved.RunID)
	if err != nil || want.Status != run.StatusSucceeded || want.BaseSHA != baseSHA {
		t.Fatalf("Finalize() result=%#v error=%v", want, err)
	}
	if len(gate.Requests()) != 0 || pub.CallCount() != 0 || outcomes.SaveCount() != 1 {
		t.Fatalf("side effects gate=%d publisher=%d memory=%d", len(gate.Requests()), pub.CallCount(), outcomes.SaveCount())
	}
	if _, err := os.Stat(filepath.Join(source, "changed.txt")); !os.IsNotExist(err) {
		t.Fatalf("source repository changed: %v", err)
	}
	assertDirectoryEmpty(t, filepath.Join(managerRoot, "worktrees"))
	if entries, err := os.ReadDir(runtimeRoot); err != nil || len(entries) != 0 {
		t.Fatalf("runtime entries=%v error=%v", entries, err)
	}

	if err := artifacts.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedRuns, err := runfilesystem.New(runRoot)
	if err != nil {
		t.Fatal(err)
	}
	reopenedArtifacts, err := artifactfilesystem.New(artifactRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedArtifacts.Close()
	dependencies.Runs = reopenedRuns
	dependencies.Artifacts = reopenedArtifacts
	dependencies.NewID = func() string { return "unused" }
	restarted, err := New(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Finalize(context.Background(), resolved.RunID)
	if err != nil || !reflect.DeepEqual(got, want) || outcomes.SaveCount() != 1 {
		t.Fatalf("Finalize(restarted)=%#v error=%v want=%#v saves=%d", got, err, want, outcomes.SaveCount())
	}
}

func TestServiceArtifactOnlySkipsApprovalAndPublishAndFinalizesOnce(t *testing.T) {
	fixture := completedServiceFixture(t, "artifact")
	outcomes := &outcomeMemory{}
	fixture.service.memory = outcomes
	gate := approvalmock.NewGate(approval.Result{}, errors.New("must not be called"))
	pub := &sequencePublisher{errors: []error{errors.New("must not be called")}}
	fixture.service.publisher = pub

	approved, err := fixture.service.Approval(context.Background(), "run-123", gate)
	if err != nil || approved.Status != run.StatusExecuting {
		t.Fatalf("Approval() result=%#v error=%v", approved, err)
	}
	published, err := fixture.service.Publish(context.Background(), "run-123")
	if err != nil || published.Status != run.StatusExecuting {
		t.Fatalf("Publish() result=%#v error=%v", published, err)
	}
	result, err := fixture.service.Finalize(context.Background(), "run-123")
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if result.Status != run.StatusSucceeded || result.RunID != "run-123" ||
		result.BaseSHA != fixture.resolver.revision.SHA || result.Artifact.Digest == "" {
		t.Fatalf("Finalize() = %#v", result)
	}
	if len(gate.Requests()) != 0 || pub.CallCount() != 0 {
		t.Fatalf("skipped adapters were called: gate=%d publisher=%d", len(gate.Requests()), pub.CallCount())
	}
	if got := latestStageStatus(t, fixture.runs.Store, "run-123", "approval"); got != run.StageSkipped {
		t.Fatalf("approval stage = %q, want skipped", got)
	}
	if got := latestStageStatus(t, fixture.runs.Store, "run-123", "publish"); got != run.StageSkipped {
		t.Fatalf("publish stage = %q, want skipped", got)
	}
	if outcomes.SaveCount() != 1 {
		t.Fatalf("outcome saves = %d, want 1", outcomes.SaveCount())
	}
	saved := outcomes.Snapshot()[0]
	for _, want := range []string{
		"Pajé run run-123 completed",
		"Template: code-change@v1",
		"Base SHA: " + fixture.resolver.revision.SHA,
		"Artifact: sha256:" + result.Artifact.Digest,
		"resolve=succeeded", "execute=succeeded", "approval=skipped",
		"publish=skipped", "finalize=succeeded",
	} {
		if !strings.Contains(saved.Content, want) {
			t.Errorf("outcome memory missing %q:\n%s", want, saved.Content)
		}
	}
	if got := saved.Metadata; got["user_id"] != "guilhermecastro" ||
		got["app_id"] != "araihu-paje" || got["run_id"] != "run-123" ||
		got["paje_status"] != string(run.StatusSucceeded) {
		t.Fatalf("outcome tags = %#v", got)
	}
	for _, secret := range []string{"agent-secret", "/tmp/workspace", "memory-secret"} {
		if strings.Contains(saved.Content, secret) {
			t.Fatalf("outcome memory leaked %q: %s", secret, saved.Content)
		}
	}

	again, err := fixture.service.Finalize(context.Background(), "run-123")
	if err != nil || !reflect.DeepEqual(result, again) || outcomes.SaveCount() != 1 {
		t.Fatalf("Finalize(second)=%#v error=%v saves=%d", again, err, outcomes.SaveCount())
	}
}

func TestServicePullRequestApprovalIsArtifactBoundAndPublishesOnce(t *testing.T) {
	fixture := completedServiceFixture(t, "pull_request")
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	bundle, _ := fixture.artifacts.Load(context.Background(), *record.Artifact)
	decision := approval.Result{
		RunID: "run-123", ArtifactDigest: record.Artifact.Digest, Approved: true,
		Actor: "reviewer@example.test", DecidedAt: time.Unix(200, 0).UTC(),
	}
	gate := approvalmock.NewGate(decision, nil)
	pubResult := publisher.Result{
		Provider: "github", Branch: "paje/code-change/run-123",
		CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
		PullRequestURL: "https://example.test/pull/42",
	}
	pub := &sequencePublisher{results: []publisher.Result{pubResult}}
	fixture.service.publisher = pub

	if _, err := fixture.service.Approval(context.Background(), "run-123", gate); err != nil {
		t.Fatalf("Approval() error = %v", err)
	}
	requests := gate.Requests()
	if len(requests) != 1 {
		t.Fatalf("approval requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.RunID != "run-123" || request.TemplateID != "code-change@v1" ||
		request.Repository != record.RepositoryURI || request.BaseSHA != record.BaseSHA ||
		request.TargetBranch != "main" || request.PublicationBranch != "paje/code-change/run-123" ||
		request.ArtifactDigest != record.Artifact.Digest ||
		len(request.ChangedPaths) != len(bundle.Manifest.Changes) ||
		len(request.Verification) != len(bundle.Verification) ||
		!reflect.DeepEqual(request.Warnings, bundle.Warnings) {
		t.Fatalf("approval request = %#v", request)
	}
	if request.AgentSummary == "" || strings.Contains(request.AgentSummary, "agent-secret") {
		t.Fatalf("unsafe agent summary = %q", request.AgentSummary)
	}

	published, err := fixture.service.Publish(context.Background(), "run-123")
	if err != nil || published.Status != run.StatusPublishing || pub.CallCount() != 1 {
		t.Fatalf("Publish() result=%#v error=%v calls=%d", published, err, pub.CallCount())
	}
	if got := pub.Requests()[0].Repository; got != request.Repository ||
		got != "https://github.com/araihu/paje.git" {
		t.Fatalf("repository identity approval=%q publisher=%q", request.Repository, got)
	}
	again, err := fixture.service.Publish(context.Background(), "run-123")
	if err != nil || again.Status != run.StatusPublishing || pub.CallCount() != 1 {
		t.Fatalf("Publish(second) result=%#v error=%v calls=%d", again, err, pub.CallCount())
	}

	fixture.service.memory = &outcomeMemory{}
	final, err := fixture.service.Finalize(context.Background(), "run-123")
	if err != nil || final.Status != run.StatusSucceeded ||
		final.Publication == nil || *final.Publication != pubResult ||
		final.Approval == nil || !final.Approval.Approved {
		t.Fatalf("Finalize() result=%#v error=%v", final, err)
	}
}

func TestServiceApprovalRejectsMismatchedDecisionsAndDeclineIsTerminal(t *testing.T) {
	tests := []struct {
		name     string
		decision func(run.Record) approval.Result
		gateErr  error
		declined bool
	}{
		{
			name: "run ID mismatch",
			decision: func(record run.Record) approval.Result {
				return approval.Result{RunID: "other-run", ArtifactDigest: record.Artifact.Digest, Approved: true, Actor: "reviewer", DecidedAt: time.Unix(200, 0).UTC()}
			},
		},
		{
			name: "artifact mismatch",
			decision: func(record run.Record) approval.Result {
				return approval.Result{RunID: record.ID, ArtifactDigest: strings.Repeat("d", 64), Approved: true, Actor: "reviewer", DecidedAt: time.Unix(200, 0).UTC()}
			},
		},
		{
			name: "declined",
			decision: func(record run.Record) approval.Result {
				return approval.Result{RunID: record.ID, ArtifactDigest: record.Artifact.Digest, Actor: "reviewer", Reason: "needs changes", DecidedAt: time.Unix(200, 0).UTC()}
			},
			declined: true,
		},
		{
			name: "untyped gate failure",
			decision: func(run.Record) approval.Result {
				return approval.Result{}
			},
			gateErr: errors.New("provider secret-detail"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := completedServiceFixture(t, "pull_request")
			record, _ := fixture.runs.Load(context.Background(), "run-123")
			gate := approvalmock.NewGate(test.decision(record), test.gateErr)
			result, err := fixture.service.Approval(context.Background(), "run-123", gate)
			if test.declined {
				if err != nil || result.Status != run.StatusDeclined {
					t.Fatalf("Approval() result=%#v error=%v", result, err)
				}
				if _, err := fixture.service.Publish(context.Background(), "run-123"); err != nil {
					t.Fatalf("Publish(declined) error = %v", err)
				}
				if fixture.publisherCalls() != 0 {
					t.Fatalf("publisher calls = %d, want 0", fixture.publisherCalls())
				}
				return
			}
			if err == nil || result.FailureClass != run.FailureApproval || result.Retryable {
				t.Fatalf("Approval() result=%#v error=%v", result, err)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "secret-detail") {
				t.Fatalf("phase result leaked provider error: %s", encoded)
			}
		})
	}
}

func TestServiceApprovalAndPublishPersistCancellationAfterProviderReturns(t *testing.T) {
	t.Run("approval", func(t *testing.T) {
		fixture := completedServiceFixture(t, "pull_request")
		record, _ := fixture.runs.Load(context.Background(), "run-123")
		ctx, cancel := context.WithCancel(context.Background())
		gate := gateFunc(func(context.Context, approval.Request) (approval.Result, error) {
			cancel()
			return approval.Result{
				RunID: record.ID, ArtifactDigest: record.Artifact.Digest, Approved: true,
				Actor: "reviewer", DecidedAt: time.Unix(200, 0).UTC(),
			}, nil
		})
		result, err := fixture.service.Approval(ctx, "run-123", gate)
		if err == nil || result.Status != run.StatusCanceled ||
			result.FailureClass != run.FailureCanceled {
			t.Fatalf("Approval(canceled) result=%#v error=%v", result, err)
		}
		record, _ = fixture.runs.Load(context.Background(), "run-123")
		if record.Approval != nil {
			t.Fatalf("canceled approval persisted decision: %#v", record.Approval)
		}
	})
	t.Run("publish", func(t *testing.T) {
		fixture := approvedServiceFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		fixture.service.publisher = publisherFunc(func(context.Context, publisher.Request) (publisher.Result, error) {
			cancel()
			return publisher.Result{
				Provider: "github", Branch: "paje/code-change/run-123",
				CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
				PullRequestURL: "https://example.test/pull/42",
			}, nil
		})
		result, err := fixture.service.Publish(ctx, "run-123")
		if err == nil || result.Status != run.StatusCanceled ||
			result.FailureClass != run.FailureCanceled {
			t.Fatalf("Publish(canceled) result=%#v error=%v", result, err)
		}
		record, _ := fixture.runs.Load(context.Background(), "run-123")
		if record.Publication != nil {
			t.Fatalf("canceled publish persisted result: %#v", record.Publication)
		}
	})
	t.Run("approval CAS persistence", func(t *testing.T) {
		fixture := completedServiceFixture(t, "pull_request")
		record, _ := fixture.runs.Load(context.Background(), "run-123")
		ctx, cancel := context.WithCancel(context.Background())
		fixture.service.runs = &cancelOnEvidenceStore{
			Store: fixture.runs, cancel: cancel, evidence: "approval",
		}
		gate := approvalmock.NewGate(approval.Result{
			RunID: record.ID, ArtifactDigest: record.Artifact.Digest, Approved: true,
			Actor: "reviewer", DecidedAt: time.Unix(200, 0).UTC(),
		}, nil)
		result, err := fixture.service.Approval(ctx, "run-123", gate)
		if err == nil || result.Status != run.StatusCanceled {
			t.Fatalf("Approval(CAS canceled) result=%#v error=%v", result, err)
		}
		record, _ = fixture.runs.Load(context.Background(), "run-123")
		stage, _ := latestStage(record, "approval")
		if record.Approval != nil || stage.Status != run.StageFailed ||
			stage.Failure == nil || stage.Failure.Class != run.FailureCanceled {
			t.Fatalf("approval cancellation record = %#v", record)
		}
	})
	t.Run("publish CAS persistence", func(t *testing.T) {
		fixture := approvedServiceFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		fixture.service.runs = &cancelOnEvidenceStore{
			Store: fixture.runs, cancel: cancel, evidence: "publication",
		}
		fixture.service.publisher = &sequencePublisher{results: []publisher.Result{{
			Provider: "github", Branch: "paje/code-change/run-123",
			CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
			PullRequestURL: "https://example.test/pull/42",
		}}}
		result, err := fixture.service.Publish(ctx, "run-123")
		if err == nil || result.Status != run.StatusCanceled {
			t.Fatalf("Publish(CAS canceled) result=%#v error=%v", result, err)
		}
		record, _ := fixture.runs.Load(context.Background(), "run-123")
		stage, _ := latestStage(record, "publish")
		if record.Publication != nil || stage.Status != run.StageFailed ||
			stage.Failure == nil || stage.Failure.Class != run.FailureCanceled {
			t.Fatalf("publish cancellation record = %#v", record)
		}
	})
}

func TestServiceFinalizeCancellationIsTerminalAndDoesNotRetryMemory(t *testing.T) {
	fixture := completedServiceFixture(t, "artifact")
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	fixture.service.memory = memoryFunc{
		search: func(context.Context, string, int, map[string]string) ([]memory.Memory, error) {
			calls++
			cancel()
			return nil, context.Canceled
		},
	}
	result, err := fixture.service.Finalize(ctx, "run-123")
	if err == nil || result.Status != run.StatusCanceled ||
		result.Failure == nil || result.Failure.Class != run.FailureCanceled ||
		result.Failure.Retryable {
		t.Fatalf("Finalize(canceled) result=%#v error=%v", result, err)
	}
	again, err := fixture.service.Finalize(context.Background(), "run-123")
	if err != nil || again.Status != run.StatusCanceled || calls != 1 {
		t.Fatalf("Finalize(after cancellation) result=%#v error=%v calls=%d", again, err, calls)
	}

	t.Run("marker CAS persistence", func(t *testing.T) {
		fixture := completedServiceFixture(t, "artifact")
		outcomes := &outcomeMemory{}
		fixture.service.memory = outcomes
		ctx, cancel := context.WithCancel(context.Background())
		fixture.service.runs = &cancelOnEvidenceStore{
			Store: fixture.runs, cancel: cancel, evidence: "outcome",
		}
		result, err := fixture.service.Finalize(ctx, "run-123")
		if err == nil || result.Status != run.StatusExecuting ||
			result.Failure != nil || outcomes.SaveCount() != 1 {
			t.Fatalf("Finalize(marker CAS canceled) result=%#v error=%v", result, err)
		}
		record, _ := fixture.runs.Load(context.Background(), "run-123")
		finalize, found := latestStage(record, finalizeStage)
		if record.OutcomeMemorySaved || record.Terminal() || !found ||
			finalize.Status != run.StageRunning {
			t.Fatalf("marker persistence did not remain recoverable: %#v", record)
		}
		again, err := fixture.service.Finalize(context.Background(), "run-123")
		if err != nil || again.Status != run.StatusSucceeded ||
			outcomes.SaveCount() != 1 {
			t.Fatalf("Finalize(marker recovery) result=%#v error=%v saves=%d",
				again, err, outcomes.SaveCount())
		}
	})

	t.Run("cancellation after successful outcome save commits marker", func(t *testing.T) {
		fixture := completedServiceFixture(t, "artifact")
		outcomes := &outcomeMemory{}
		ctx, cancel := context.WithCancel(context.Background())
		fixture.service.memory = memoryFunc{
			search: outcomes.Search,
			save: func(
				saveCtx context.Context,
				content string,
				tags map[string]string,
			) error {
				err := outcomes.Save(saveCtx, content, tags)
				cancel()
				return err
			},
		}
		result, err := fixture.service.Finalize(ctx, "run-123")
		if err != nil || result.Status != run.StatusSucceeded ||
			outcomes.SaveCount() != 1 {
			t.Fatalf("Finalize(save committed) result=%#v error=%v saves=%d",
				result, err, outcomes.SaveCount())
		}
		record, _ := fixture.runs.Load(context.Background(), "run-123")
		if !record.OutcomeMemorySaved || record.Status != run.StatusSucceeded {
			t.Fatalf("committed outcome did not finish run: %#v", record)
		}
		again, err := fixture.service.Finalize(context.Background(), "run-123")
		if err != nil || again.Status != run.StatusSucceeded ||
			outcomes.SaveCount() != 1 {
			t.Fatalf("Finalize(save recovery) result=%#v error=%v saves=%d",
				again, err, outcomes.SaveCount())
		}
	})
}

func TestServiceFinalizeBoundsDetachedResultReconstruction(t *testing.T) {
	fixture := completedServiceFixture(t, "artifact")
	fixture.service.cleanupTimeout = 20 * time.Millisecond
	fixture.service.artifacts = &blockingLoadArtifact{
		Store: fixture.artifacts, blockAt: 2,
	}
	ctx, cancel := context.WithCancel(context.Background())
	fixture.service.memory = memoryFunc{
		search: func(context.Context, string, int, map[string]string) ([]memory.Memory, error) {
			cancel()
			return nil, context.Canceled
		},
	}
	before := time.Now()
	result, err := fixture.service.Finalize(ctx, "run-123")
	if err == nil || result.RunID != "run-123" || result.Status != run.StatusCanceled {
		t.Fatalf("Finalize() result=%#v error=%v", result, err)
	}
	if time.Since(before) > 200*time.Millisecond {
		t.Fatalf("detached result reconstruction was unbounded: %v", time.Since(before))
	}
}

func TestServiceFinalizeRejectsTamperedDurableApprovalBeforeMemoryAndResult(t *testing.T) {
	fixture := approvedServiceFixture(t)
	fixture.service.publisher = &sequencePublisher{results: []publisher.Result{{
		Provider: "github", Branch: "paje/code-change/run-123",
		CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
		PullRequestURL: "https://example.test/pull/42",
	}}}
	if _, err := fixture.service.Publish(context.Background(), "run-123"); err != nil {
		t.Fatal(err)
	}
	outcomes := &outcomeMemory{}
	fixture.service.memory = outcomes
	fixture.runs.loadMutate = func(record run.Record) run.Record {
		for index := range record.Stages {
			if record.Stages[index].Name == "approval" {
				record.Stages[index].Evidence["target_branch"] = "tampered"
			}
		}
		return record
	}
	result, err := fixture.service.Finalize(context.Background(), "run-123")
	if err == nil || result.RunID != "run-123" || outcomes.SaveCount() != 0 {
		t.Fatalf("Finalize(tampered) result=%#v error=%v saves=%d", result, err, outcomes.SaveCount())
	}
}

func TestServiceValidateDurableEvidenceRejectsIncompleteProviderMatrix(t *testing.T) {
	tests := []struct {
		name    string
		fixture func(*testing.T) *serviceFixture
		mutate  func(*run.Record)
		wantErr bool
	}{
		{
			name:    "artifact approval stage missing",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				record.Stages = withoutStage(record.Stages, approvalStage)
			},
			wantErr: true,
		},
		{
			name:    "artifact approval stage succeeded",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				setLatestStageStatus(record, approvalStage, run.StageSucceeded)
			},
			wantErr: true,
		},
		{
			name:    "artifact publish stage running",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				setLatestStageStatus(record, publishStage, run.StageRunning)
			},
			wantErr: true,
		},
		{
			name:    "approval succeeded without decision",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Approval = nil
				record.Publication = nil
				record.Stages = withoutStage(record.Stages, publishStage)
				record.Status = run.StatusAwaitingApproval
			},
			wantErr: true,
		},
		{
			name:    "approval pointer with skipped stage",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				setLatestStageStatus(record, approvalStage, run.StageSkipped)
			},
			wantErr: true,
		},
		{
			name:    "approval pointer with missing stage",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Stages = withoutStage(record.Stages, approvalStage)
			},
			wantErr: true,
		},
		{
			name:    "approval pointer with failed stage",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				setLatestStageStatus(record, approvalStage, run.StageFailed)
			},
			wantErr: true,
		},
		{
			name:    "publish succeeded without result",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Publication = nil
			},
			wantErr: true,
		},
		{
			name:    "publication pointer with skipped stage",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				setLatestStageStatus(record, publishStage, run.StageSkipped)
			},
			wantErr: true,
		},
		{
			name:    "publication pointer with missing stage",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Stages = withoutStage(record.Stages, publishStage)
			},
			wantErr: true,
		},
		{
			name:    "publication pointer with running stage",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				setLatestStageStatus(record, publishStage, run.StageRunning)
			},
			wantErr: true,
		},
		{
			name:    "publication without approval",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Approval = nil
				record.Stages = withoutStage(record.Stages, approvalStage)
			},
			wantErr: true,
		},
		{
			name:    "duplicate latest approval evidence",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				stage, _ := latestStage(*record, approvalStage)
				record.Stages = append(record.Stages, stage)
			},
			wantErr: true,
		},
		{
			name:    "duplicate latest publication evidence",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				stage, _ := latestStage(*record, publishStage)
				record.Stages = append(record.Stages, stage)
			},
			wantErr: true,
		},
		{
			name:    "declined decision and stage match terminal status",
			fixture: declinedServiceFixture,
		},
		{
			name:    "declined status with approved decision",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Status = run.StatusDeclined
				record.Publication = nil
				record.Stages = withoutStage(record.Stages, publishStage)
			},
			wantErr: true,
		},
		{
			name:    "declined decision outside declined status",
			fixture: declinedServiceFixture,
			mutate: func(record *run.Record) {
				record.Status = run.StatusAwaitingApproval
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.fixture(t)
			record, err := fixture.runs.Load(context.Background(), "run-123")
			if err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(&record)
			}
			input, err := validateRunBinding(record)
			if err != nil {
				t.Fatalf("validateRunBinding() error = %v", err)
			}
			err = fixture.service.validateDurableEvidence(context.Background(), record, input)
			if test.wantErr && err == nil {
				t.Fatalf("validateDurableEvidence() error = nil for record %#v", record)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateDurableEvidence() error = %v", err)
			}
			_, resultErr := fixture.service.resultFromRecord(context.Background(), record)
			if test.wantErr && resultErr == nil {
				t.Fatalf("resultFromRecord() error = nil for record %#v", record)
			}
			if !test.wantErr && resultErr != nil {
				t.Fatalf("resultFromRecord() error = %v", resultErr)
			}
		})
	}
}

func TestServiceFinalizeRejectsFalseProviderStatusBeforeMemory(t *testing.T) {
	fixture := artifactReadyServiceFixture(t)
	outcomes := &outcomeMemory{}
	fixture.service.memory = outcomes
	fixture.service.runs = &mutateSaveResultStore{
		Store: fixture.runs,
		mutate: func(record run.Record) run.Record {
			setLatestStageStatus(&record, approvalStage, run.StageSucceeded)
			return record
		},
	}

	result, err := fixture.service.Finalize(context.Background(), "run-123")
	if err == nil || result.RunID != "run-123" || outcomes.SaveCount() != 0 {
		t.Fatalf("Finalize(false provider status) result=%#v error=%v saves=%d",
			result, err, outcomes.SaveCount())
	}
}

func TestServiceValidateDurableProviderHistoryShapeAndChronology(t *testing.T) {
	approvalFailed := func(t *testing.T) *serviceFixture {
		return approvalFailedServiceFixture(t, false)
	}
	approvalRetryable := func(t *testing.T) *serviceFixture {
		return approvalFailedServiceFixture(t, true)
	}
	publishFailed := func(t *testing.T) *serviceFixture {
		return publishFailedServiceFixture(t, false)
	}
	publishRetryable := func(t *testing.T) *serviceFixture {
		return publishFailedServiceFixture(t, true)
	}
	pullRequestReady := func(t *testing.T) *serviceFixture {
		return completedServiceFixture(t, "pull_request")
	}
	tests := []struct {
		name         string
		fixture      func(*testing.T) *serviceFixture
		mutate       func(*run.Record)
		wantErr      bool
		skipFinalize bool
	}{
		{name: "canonical artifact skips", fixture: artifactReadyServiceFixture},
		{name: "canonical pull request success", fixture: publishedServiceFixture},
		{name: "canonical decline", fixture: declinedServiceFixture},
		{name: "canonical terminal approval failure", fixture: approvalFailed},
		{name: "canonical retryable approval failure", fixture: approvalRetryable},
		{name: "canonical terminal publish failure", fixture: publishFailed},
		{name: "canonical retryable publish failure", fixture: publishRetryable},
		{name: "canonical approval retry history", fixture: approvalRetriedPublishedServiceFixture},
		{name: "canonical publish retry history", fixture: publishRetriedServiceFixture},
		{
			name:    "provider stages without artifact",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				record.Artifact = nil
			},
			wantErr: true,
		},
		{
			name:    "failed publish stage without artifact",
			fixture: publishFailed,
			mutate: func(record *run.Record) {
				record.Artifact = nil
				record.Approval = nil
				record.Stages = withoutStage(record.Stages, approvalStage)
			},
			wantErr: true,
		},
		{
			name:    "skipped stage carries failure",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				failure := providerFailure(approvalStage, run.FailureApproval, false)
				providerStagePointer(record, approvalStage, 1).Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "succeeded approval carries failure",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				failure := providerFailure(approvalStage, run.FailureApproval, false)
				providerStagePointer(record, approvalStage, 1).Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "succeeded publish carries failure",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				failure := providerFailure(publishStage, run.FailurePublication, false)
				providerStagePointer(record, publishStage, 1).Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "failed approval lacks failure",
			fixture: approvalFailed,
			mutate: func(record *run.Record) {
				providerStagePointer(record, approvalStage, 1).Failure = nil
			},
			wantErr: true,
		},
		{
			name:    "failed publish carries success evidence",
			fixture: publishFailed,
			mutate: func(record *run.Record) {
				providerStagePointer(record, publishStage, 1).Evidence =
					map[string]string{"provider": "github"}
			},
			wantErr: true,
		},
		{
			name:    "publish failure names approval",
			fixture: publishFailed,
			mutate: func(record *run.Record) {
				failure := *providerStagePointer(record, publishStage, 1).Failure
				failure.Stage = approvalStage
				providerStagePointer(record, publishStage, 1).Failure = &failure
				record.Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "approval failure has publication class",
			fixture: approvalFailed,
			mutate: func(record *run.Record) {
				failure := *providerStagePointer(record, approvalStage, 1).Failure
				failure.Class = run.FailurePublication
				providerStagePointer(record, approvalStage, 1).Failure = &failure
				record.Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "latest failed stage differs from top level failure",
			fixture: publishFailed,
			mutate: func(record *run.Record) {
				failure := *record.Failure
				failure.CauseCode = "different_failure"
				record.Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "provider failure lacks stage",
			fixture: pullRequestReady,
			mutate: func(record *run.Record) {
				failure := providerFailure(approvalStage, run.FailureApproval, true)
				record.Failure = &failure
			},
			wantErr:      true,
			skipFinalize: true,
		},
		{
			name:    "succeeded publish retains stale provider failure",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				failure := providerFailure(publishStage, run.FailurePublication, false)
				record.Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "failed stage retains result pointer",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				stage := providerStagePointer(record, publishStage, 1)
				failure := providerFailure(publishStage, run.FailurePublication, false)
				stage.Status = run.StageFailed
				stage.Failure = &failure
				record.Failure = &failure
				record.Status = run.StatusFailed
			},
			wantErr: true,
		},
		{
			name:    "retryable approval failure has executing status",
			fixture: approvalRetryable,
			mutate: func(record *run.Record) {
				record.Status = run.StatusExecuting
			},
			wantErr:      true,
			skipFinalize: true,
		},
		{
			name:    "nonretryable approval failure has canceled status",
			fixture: approvalFailed,
			mutate: func(record *run.Record) {
				record.Status = run.StatusCanceled
			},
			wantErr: true,
		},
		{
			name:    "approval running in executing status",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Approval = nil
				record.Publication = nil
				record.Stages = withoutStage(record.Stages, publishStage)
				stage := providerStagePointer(record, approvalStage, 1)
				stage.Status = run.StageRunning
				stage.FinishedAt = time.Time{}
				stage.Evidence = nil
				record.Status = run.StatusExecuting
			},
			wantErr: true,
		},
		{
			name:    "approval running carries failure",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Approval = nil
				record.Publication = nil
				record.Stages = withoutStage(record.Stages, publishStage)
				stage := providerStagePointer(record, approvalStage, 1)
				failure := providerFailure(approvalStage, run.FailureApproval, true)
				stage.Status = run.StageRunning
				stage.FinishedAt = time.Time{}
				stage.Evidence = nil
				stage.Failure = &failure
				record.Status = run.StatusAwaitingApproval
			},
			wantErr: true,
		},
		{
			name:    "approval running carries evidence",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Approval = nil
				record.Publication = nil
				record.Stages = withoutStage(record.Stages, publishStage)
				stage := providerStagePointer(record, approvalStage, 1)
				stage.Status = run.StageRunning
				stage.FinishedAt = time.Time{}
				record.Status = run.StatusAwaitingApproval
			},
			wantErr: true,
		},
		{
			name:    "terminal record retains running approval",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Approval = nil
				record.Publication = nil
				record.Stages = withoutStage(record.Stages, publishStage)
				stage := providerStagePointer(record, approvalStage, 1)
				stage.Status = run.StageRunning
				stage.FinishedAt = time.Time{}
				stage.Evidence = nil
				failure := providerFailure("execute", run.FailureAgent, false)
				record.Failure = &failure
				record.Status = run.StatusFailed
			},
			wantErr: true,
		},
		{
			name:    "running publish has finished timestamp",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Publication = nil
				stage := providerStagePointer(record, publishStage, 1)
				stage.Status = run.StageRunning
				stage.Evidence = nil
			},
			wantErr: true,
		},
		{
			name:    "publish running in awaiting approval status",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Publication = nil
				stage := providerStagePointer(record, publishStage, 1)
				stage.Status = run.StageRunning
				stage.FinishedAt = time.Time{}
				stage.Evidence = nil
				record.Status = run.StatusAwaitingApproval
			},
			wantErr: true,
		},
		{
			name:    "approval warning",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Approval = nil
				record.Publication = nil
				record.Stages = withoutStage(record.Stages, publishStage)
				providerStagePointer(record, approvalStage, 1).Status = run.StageWarning
				record.Status = run.StatusAwaitingApproval
			},
			wantErr: true,
		},
		{
			name:    "publish warning",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Publication = nil
				providerStagePointer(record, publishStage, 1).Status = run.StageWarning
			},
			wantErr: true,
		},
		{
			name:    "publish starts before approval finishes",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				approval := providerStagePointer(record, approvalStage, 1)
				publish := providerStagePointer(record, publishStage, 1)
				publish.StartedAt = approval.FinishedAt.Add(-time.Nanosecond)
			},
			wantErr: true,
		},
		{
			name:    "publish starts when approval finishes",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				approval := providerStagePointer(record, approvalStage, 1)
				providerStagePointer(record, publishStage, 1).StartedAt = approval.FinishedAt
			},
			wantErr: true,
		},
		{
			name:    "publish has zero start",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				providerStagePointer(record, publishStage, 1).StartedAt = time.Time{}
			},
			wantErr: true,
		},
		{
			name:    "publish is ordered before approval",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				approvalIndex, publishIndex := -1, -1
				for index, stage := range record.Stages {
					if stage.Name == approvalStage {
						approvalIndex = index
					}
					if stage.Name == publishStage {
						publishIndex = index
					}
				}
				record.Stages[approvalIndex], record.Stages[publishIndex] =
					record.Stages[publishIndex], record.Stages[approvalIndex]
			},
			wantErr: true,
		},
		{
			name:    "historical approval failure lacks failure",
			fixture: approvalRetriedPublishedServiceFixture,
			mutate: func(record *run.Record) {
				providerStagePointer(record, approvalStage, 1).Failure = nil
			},
			wantErr: true,
		},
		{
			name:    "historical approval warning",
			fixture: approvalRetriedPublishedServiceFixture,
			mutate: func(record *run.Record) {
				stage := providerStagePointer(record, approvalStage, 1)
				stage.Status = run.StageWarning
				stage.Failure = nil
			},
			wantErr: true,
		},
		{
			name:    "duplicate historical approval attempt",
			fixture: approvalRetriedPublishedServiceFixture,
			mutate: func(record *run.Record) {
				duplicate := *providerStagePointer(record, approvalStage, 1)
				record.Stages = append(record.Stages, duplicate)
			},
			wantErr: true,
		},
		{
			name:    "approval attempt gap",
			fixture: approvalRetriedPublishedServiceFixture,
			mutate: func(record *run.Record) {
				providerStagePointer(record, approvalStage, 2).Attempts = 3
			},
			wantErr: true,
		},
		{
			name:    "approval retry starts when failure finishes",
			fixture: approvalRetriedPublishedServiceFixture,
			mutate: func(record *run.Record) {
				first := providerStagePointer(record, approvalStage, 1)
				providerStagePointer(record, approvalStage, 2).StartedAt = first.FinishedAt
			},
			wantErr: true,
		},
		{
			name:    "approval succeeds before later success",
			fixture: approvalRetriedPublishedServiceFixture,
			mutate: func(record *run.Record) {
				first := providerStagePointer(record, approvalStage, 1)
				latest := providerStagePointer(record, approvalStage, 2)
				first.Status = run.StageSucceeded
				first.Failure = nil
				first.Evidence = cloneStringMap(latest.Evidence)
			},
			wantErr: true,
		},
		{
			name:    "historical publish has approval class",
			fixture: publishRetriedServiceFixture,
			mutate: func(record *run.Record) {
				first := providerStagePointer(record, publishStage, 1)
				failure := *first.Failure
				failure.Class = run.FailureApproval
				first.Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "publish retry overlaps prior attempt",
			fixture: publishRetriedServiceFixture,
			mutate: func(record *run.Record) {
				first := providerStagePointer(record, publishStage, 1)
				providerStagePointer(record, publishStage, 2).StartedAt =
					first.FinishedAt.Add(-time.Nanosecond)
			},
			wantErr: true,
		},
		{
			name:    "decline retains publish stage",
			fixture: declinedServiceFixture,
			mutate: func(record *run.Record) {
				approval := providerStagePointer(record, approvalStage, 1)
				record.Stages = append(record.Stages, run.StageResult{
					Name: publishStage, Status: run.StageRunning,
					StartedAt: approval.FinishedAt.Add(time.Second), Attempts: 1,
				})
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.fixture(t)
			record, err := fixture.runs.Load(context.Background(), "run-123")
			if err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(&record)
			}
			input, err := validateRunBinding(record)
			if err != nil {
				t.Fatalf("validateRunBinding() error = %v", err)
			}
			err = fixture.service.validateDurableEvidence(context.Background(), record, input)
			if test.wantErr && err == nil {
				t.Fatal("validateDurableEvidence() error = nil")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateDurableEvidence() error = %v", err)
			}
			_, resultErr := fixture.service.resultFromRecord(context.Background(), record)
			if test.wantErr && resultErr == nil {
				t.Fatal("resultFromRecord() error = nil")
			}
			if !test.wantErr && resultErr != nil {
				t.Fatalf("resultFromRecord() error = %v", resultErr)
			}
			if !test.wantErr || test.skipFinalize {
				return
			}

			finalizeFixture := test.fixture(t)
			outcomes := &outcomeMemory{}
			finalizeFixture.service.memory = outcomes
			finalizeFixture.service.runs = &mutateSaveResultStore{
				Store: finalizeFixture.runs,
				mutate: func(record run.Record) run.Record {
					test.mutate(&record)
					return record
				},
			}
			if _, err := finalizeFixture.service.Finalize(
				context.Background(), "run-123",
			); err == nil || outcomes.SaveCount() != 0 {
				t.Fatalf("Finalize() error=%v memory saves=%d, want rejection before memory",
					err, outcomes.SaveCount())
			}
		})
	}
}

func TestServiceRejectsUnsafePullRequestRepositoryBeforePorts(t *testing.T) {
	for _, repositoryURI := range []string{
		"/tmp/repository", "git@github.com:owner/repo.git",
		"https://token@github.com/owner/repo.git",
		"https://github.com/owner/repo.git?token=secret",
		"https://github.com/owner/../repo.git",
		"https://example.test/owner/repo.git",
	} {
		t.Run(repositoryURI, func(t *testing.T) {
			fixture := newServiceFixture(t)
			var value map[string]any
			_ = json.Unmarshal(validRawInput("change", "unsafe-repo"), &value)
			value["repository_uri"] = repositoryURI
			value["publication"] = map[string]any{
				"mode": "pull_request", "provider": "github", "target_branch": "main",
			}
			raw, _ := json.Marshal(value)
			if _, err := fixture.service.Resolve(context.Background(), raw); err == nil {
				t.Fatalf("Resolve(%q) error = nil", repositoryURI)
			}
			if fixture.runs.reserveCalls != 0 || fixture.resolver.calls != 0 {
				t.Fatalf("unsafe repository reached ports: reserve=%d resolver=%d",
					fixture.runs.reserveCalls, fixture.resolver.calls)
			}
		})
	}
}

func TestServiceFinalizeWaiterHonorsContext(t *testing.T) {
	fixture := completedServiceFixture(t, "artifact")
	started := make(chan struct{})
	release := make(chan struct{})
	fixture.service.memory = &blockingMemory{started: started, release: release}
	done := make(chan error, 1)
	go func() {
		_, err := fixture.service.Finalize(context.Background(), "run-123")
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	before := time.Now()
	if _, err := fixture.service.Finalize(ctx, "run-123"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Finalize(waiter) error = %v", err)
	}
	if time.Since(before) > 200*time.Millisecond {
		t.Fatalf("canceled lock waiter returned too slowly: %v", time.Since(before))
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first Finalize() error = %v", err)
	}
}

func TestServicePublishClassifiesConflictAndRetriesTypedOutage(t *testing.T) {
	t.Run("missing approval fails closed before provider", func(t *testing.T) {
		fixture := completedServiceFixture(t, "pull_request")
		pub := &sequencePublisher{}
		fixture.service.publisher = pub
		result, err := fixture.service.Publish(context.Background(), "run-123")
		if err == nil || result.Status != run.StatusFailed ||
			result.FailureClass != run.FailureApproval ||
			result.Retryable || pub.CallCount() != 0 {
			t.Fatalf("Publish() result=%#v error=%v calls=%d", result, err, pub.CallCount())
		}
		record, loadErr := fixture.runs.Load(context.Background(), "run-123")
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		approvalAttempt, found := latestStage(record, approvalStage)
		if !found || approvalAttempt.Status != run.StageFailed ||
			approvalAttempt.Failure == nil ||
			approvalAttempt.Failure.Class != run.FailureApproval ||
			approvalAttempt.Failure.CauseCode != "publication_requires_approval" ||
			record.Approval != nil || record.Publication != nil ||
			len(providerStages(record, publishStage)) != 0 {
			t.Fatalf("Publish() durable precondition failure = %#v", record)
		}

		outcomes := &outcomeMemory{}
		fixture.service.memory = outcomes
		final, finalizeErr := fixture.service.Finalize(context.Background(), "run-123")
		if finalizeErr != nil || final.Status != run.StatusFailed ||
			final.Failure == nil || final.Failure.Class != run.FailureApproval ||
			final.Failure.CauseCode != "publication_requires_approval" ||
			final.Publication != nil || outcomes.SaveCount() != 1 {
			t.Fatalf("Finalize(precondition failure) result=%#v error=%v saves=%d",
				final, finalizeErr, outcomes.SaveCount())
		}
	})
	t.Run("conflict is terminal", func(t *testing.T) {
		fixture := approvedServiceFixture(t)
		fixture.service.publisher = &sequencePublisher{errors: []error{publisher.ErrConflict}}
		result, err := fixture.service.Publish(context.Background(), "run-123")
		if err == nil || result.Status != run.StatusFailed ||
			result.FailureClass != run.FailurePublication || result.Retryable {
			t.Fatalf("Publish() result=%#v error=%v", result, err)
		}
	})
	t.Run("typed outage retries then succeeds", func(t *testing.T) {
		fixture := approvedServiceFixture(t)
		success := publisher.Result{
			Provider: "github", Branch: "paje/code-change/run-123",
			CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
			PullRequestURL: "https://example.test/pull/42",
		}
		pub := &sequencePublisher{
			results: []publisher.Result{{}, success},
			errors:  []error{retryableTestError{errors.New("provider unavailable secret")}, nil},
		}
		fixture.service.publisher = pub
		first, err := fixture.service.Publish(context.Background(), "run-123")
		if err == nil || !first.Retryable || first.Status != run.StatusPublishing {
			t.Fatalf("Publish(first) result=%#v error=%v", first, err)
		}
		second, err := fixture.service.Publish(context.Background(), "run-123")
		if err != nil || second.Retryable || second.Status != run.StatusPublishing || pub.CallCount() != 2 {
			t.Fatalf("Publish(second) result=%#v error=%v calls=%d", second, err, pub.CallCount())
		}
	})
	for _, test := range []struct {
		name   string
		result publisher.Result
	}{
		{
			name: "wrong provider",
			result: publisher.Result{
				Provider: "gitlab", Branch: "paje/code-change/run-123",
				CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
				PullRequestURL: "https://example.test/pull/42",
			},
		},
		{
			name: "credential-bearing URL",
			result: publisher.Result{
				Provider: "github", Branch: "paje/code-change/run-123",
				CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
				PullRequestURL: "https://secret@example.test/pull/42",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := approvedServiceFixture(t)
			fixture.service.publisher = &sequencePublisher{results: []publisher.Result{test.result}}
			result, err := fixture.service.Publish(context.Background(), "run-123")
			if err == nil || result.Status != run.StatusFailed ||
				result.Retryable || result.FailureClass != run.FailurePublication {
				t.Fatalf("Publish() result=%#v error=%v", result, err)
			}
			record, _ := fixture.runs.Load(context.Background(), "run-123")
			if record.Publication != nil {
				t.Fatalf("unsafe publication persisted: %#v", record.Publication)
			}
		})
	}
	t.Run("changed target after approval is rejected before provider", func(t *testing.T) {
		fixture := approvedServiceFixture(t)
		pub := &sequencePublisher{}
		fixture.service.publisher = pub
		fixture.runs.loadMutate = func(record run.Record) run.Record {
			if record.Approval == nil {
				return record
			}
			var input map[string]any
			if json.Unmarshal(record.Input, &input) != nil {
				return record
			}
			publication := input["publication"].(map[string]any)
			publication["target_branch"] = "release"
			raw, _ := json.Marshal(input)
			canonical, _ := run.CanonicalInput(raw)
			record.Input = canonical
			sum := sha256.Sum256(canonical)
			record.InputHash = hex.EncodeToString(sum[:])
			return record
		}
		result, err := fixture.service.Publish(context.Background(), "run-123")
		if err == nil || result.RunID != "run-123" || pub.CallCount() != 0 {
			t.Fatalf("Publish(changed target) result=%#v error=%v calls=%d", result, err, pub.CallCount())
		}
	})
}

func TestServiceFinalizeRetriesRecoversVisibleMemoryAndExhaustsSafely(t *testing.T) {
	fixture := completedServiceFixture(t, "artifact")
	outcomes := &outcomeMemory{saveErrors: []error{errors.New("mem0 secret outage")}}
	fixture.service.memory = outcomes

	first, err := fixture.service.Finalize(context.Background(), "run-123")
	if err == nil || first.Status != run.StatusExecuting {
		t.Fatalf("Finalize(first) result=%#v error=%v", first, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if record.OutcomeMemorySaved || record.Failure == nil || !record.Failure.Retryable ||
		record.Failure.CauseCode != "outcome_memory_failed" {
		t.Fatalf("retryable finalize record = %#v", record)
	}
	if strings.Contains(record.Failure.Diagnostic, "secret") {
		t.Fatalf("failure diagnostic leaked provider error: %#v", record.Failure)
	}

	outcomes.saveErrors = nil
	second, err := fixture.service.Finalize(context.Background(), "run-123")
	if err != nil || second.Status != run.StatusSucceeded || outcomes.SaveCount() != 2 {
		t.Fatalf("Finalize(second) result=%#v error=%v saves=%d", second, err, outcomes.SaveCount())
	}

	restart := completedServiceFixture(t, "artifact")
	restartRecord, _ := restart.runs.Load(context.Background(), "run-123")
	restart.service.runs = fixture.runs.Store
	restart.service.artifacts = fixture.artifacts
	restart.service.memory = outcomes
	recovered, err := restart.service.Finalize(context.Background(), "run-123")
	if err != nil || recovered.Status != run.StatusSucceeded || outcomes.SaveCount() != 2 {
		t.Fatalf("Finalize(restart) result=%#v error=%v saves=%d old=%#v", recovered, err, outcomes.SaveCount(), restartRecord)
	}

	exhausted := completedServiceFixture(t, "artifact")
	exhausted.service.memory = &outcomeMemory{saveErrors: []error{errors.New("down")}}
	if _, err := exhausted.service.Finalize(context.Background(), "run-123"); err == nil {
		t.Fatal("Finalize() error = nil")
	}
	result, err := exhausted.service.Exhaust(context.Background(), "run-123", "finalize")
	if err != nil || result.Status != run.StatusFailed {
		t.Fatalf("Exhaust(finalize) result=%#v error=%v", result, err)
	}
	record, _ = exhausted.runs.Load(context.Background(), "run-123")
	if record.OutcomeMemorySaved || record.Failure == nil ||
		record.Failure.Diagnostic != "outcome_memory_failed" {
		t.Fatalf("exhausted finalize record = %#v", record)
	}

	t.Run("restart closes save-before-marker crash window", func(t *testing.T) {
		crashed := completedServiceFixture(t, "artifact")
		memoryStore := &outcomeMemory{}
		crashed.service.memory = memoryStore
		crashed.runs.saveCalls = 0
		crashed.runs.saveErrors = map[int]error{2: errors.New("worker lost after memory save")}

		crashResult, err := crashed.service.Finalize(context.Background(), "run-123")
		if err == nil {
			t.Fatal("Finalize(crash window) error = nil")
		}
		if crashResult.RunID != "run-123" || crashResult.Status != run.StatusExecuting ||
			crashResult.Artifact.Digest == "" {
			t.Fatalf("Finalize(crash window) result=%#v, want durable nonterminal result", crashResult)
		}
		record, _ := crashed.runs.Store.Load(context.Background(), "run-123")
		if record.OutcomeMemorySaved || memoryStore.SaveCount() != 1 {
			t.Fatalf("crash window record=%#v saves=%d", record, memoryStore.SaveCount())
		}

		crashed.runs.saveErrors = nil
		restarted := *crashed.service
		restarted.finalizeLocks = &keyedMutex{}
		result, err := restarted.Finalize(context.Background(), "run-123")
		if err != nil || result.Status != run.StatusSucceeded || memoryStore.SaveCount() != 1 {
			t.Fatalf("Finalize(restarted) result=%#v error=%v saves=%d", result, err, memoryStore.SaveCount())
		}
	})

	t.Run("saved marker before terminal transition resumes without memory", func(t *testing.T) {
		resumed := completedServiceFixture(t, "artifact")
		record, _ := resumed.runs.Load(context.Background(), "run-123")
		record.OutcomeMemorySaved = true
		if _, err := resumed.runs.Save(context.Background(), record, record.Version); err != nil {
			t.Fatal(err)
		}
		memoryStore := &outcomeMemory{}
		resumed.service.memory = memoryStore
		result, err := resumed.service.Finalize(context.Background(), "run-123")
		if err != nil || result.Status != run.StatusSucceeded || memoryStore.SaveCount() != 0 {
			t.Fatalf("Finalize(saved marker) result=%#v error=%v saves=%d", result, err, memoryStore.SaveCount())
		}
	})

	t.Run("marker observed during begin skips lagging memory", func(t *testing.T) {
		resumed := completedServiceFixture(t, "artifact")
		record, _ := resumed.runs.Load(context.Background(), "run-123")
		record.OutcomeMemorySaved = true
		if _, err := resumed.runs.Save(context.Background(), record, record.Version); err != nil {
			t.Fatal(err)
		}
		loads := 0
		resumed.runs.loadMutate = func(record run.Record) run.Record {
			loads++
			if loads == 1 {
				record.OutcomeMemorySaved = false
			}
			return record
		}
		memoryStore := &outcomeMemory{}
		resumed.service.memory = memoryStore
		result, err := resumed.service.Finalize(context.Background(), "run-123")
		if err != nil || result.Status != run.StatusSucceeded || memoryStore.SaveCount() != 0 {
			t.Fatalf("Finalize(marker race) result=%#v error=%v saves=%d", result, err, memoryStore.SaveCount())
		}
	})

	t.Run("terminal upstream failure records finalize exhaustion bookkeeping", func(t *testing.T) {
		terminal := newServiceFixture(t)
		terminal.policy.decision = policy.Decision{
			Allowed: false,
			Findings: []policy.Finding{{
				RuleID: "change-denied", Path: "changed.txt",
			}},
		}
		if _, err := terminal.service.Resolve(
			context.Background(), validRawInput("change", "terminal-finalize"),
		); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if _, err := terminal.service.Execute(context.Background(), "run-123"); err == nil {
			t.Fatal("Execute(failure) error = nil")
		}
		record, _ := terminal.runs.Load(context.Background(), "run-123")
		now := record.UpdatedAt
		terminal.service.clock = func() time.Time {
			now = now.Add(time.Second)
			return now
		}
		if record.Status != run.StatusFailed || record.Failure == nil ||
			record.Failure.Class != run.FailurePolicy || record.Artifact != nil {
			t.Fatalf("terminal upstream fixture = %#v", record)
		}
		if _, err := terminal.service.Exhaust(context.Background(), "run-123", "finalize"); err != nil {
			t.Fatalf("Exhaust(terminal without finalize failure) error = %v", err)
		}
		terminal.service.memory = &outcomeMemory{saveErrors: []error{errors.New("down")}}
		if _, err := terminal.service.Finalize(context.Background(), "run-123"); err == nil {
			t.Fatal("Finalize(terminal) error = nil")
		} else {
			record, _ = terminal.runs.Load(context.Background(), "run-123")
			if stage, found := latestStage(record, "finalize"); !found || stage.Failure == nil {
				t.Fatalf("Finalize(terminal) did not persist retryable stage: error=%v record=%#v", err, record)
			}
		}
		if _, err := terminal.service.Exhaust(context.Background(), "run-123", "finalize"); err != nil {
			t.Fatalf("Exhaust(terminal finalize) error = %v", err)
		}
		record, _ = terminal.runs.Load(context.Background(), "run-123")
		finalize, found := latestStage(record, "finalize")
		if !found || finalize.Failure == nil || finalize.Failure.Retryable ||
			finalize.Failure.CauseCode != "retries_exhausted" ||
			record.OutcomeMemorySaved || record.Failure.Class != run.FailurePolicy {
			t.Fatalf("terminal exhausted record = %#v", record)
		}
		before := terminal.service.memory.(*outcomeMemory).SaveCount()
		if _, err := terminal.service.Exhaust(context.Background(), "run-123", "finalize"); err != nil {
			t.Fatalf("Exhaust(second) error = %v", err)
		}
		result, err := terminal.service.Finalize(context.Background(), "run-123")
		if err != nil || result.Status != run.StatusFailed ||
			terminal.service.memory.(*outcomeMemory).SaveCount() != before {
			t.Fatalf("Finalize(after exhaustion) result=%#v error=%v saves=%d want=%d",
				result, err, terminal.service.memory.(*outcomeMemory).SaveCount(), before)
		}
	})
}

func TestServiceConcurrentFinalizeSavesOneOutcome(t *testing.T) {
	fixture := completedServiceFixture(t, "artifact")
	outcomes := &outcomeMemory{}
	fixture.service.memory = outcomes
	results := make(chan templatecodechange.Result, 2)
	errs := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			result, err := fixture.service.Finalize(context.Background(), "run-123")
			results <- result
			errs <- err
		}()
	}
	start.Done()
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("Finalize() error = %v", err)
		}
		if result := <-results; result.Status != run.StatusSucceeded {
			t.Errorf("Finalize() result = %#v", result)
		}
	}
	if outcomes.SaveCount() != 1 {
		t.Fatalf("outcome saves = %d, want 1", outcomes.SaveCount())
	}
}

func TestServiceValidateProviderPhaseCompatibility(t *testing.T) {
	finalizedArtifact := func(t *testing.T) *serviceFixture {
		fixture := artifactReadyServiceFixture(t)
		fixture.service.memory = &outcomeMemory{}
		if _, err := fixture.service.Finalize(context.Background(), "run-123"); err != nil {
			t.Fatalf("Finalize(artifact) error = %v", err)
		}
		return fixture
	}
	finalizedPublication := func(t *testing.T) *serviceFixture {
		fixture := publishedServiceFixture(t)
		fixture.service.memory = &outcomeMemory{}
		if _, err := fixture.service.Finalize(context.Background(), "run-123"); err != nil {
			t.Fatalf("Finalize(publication) error = %v", err)
		}
		return fixture
	}
	tests := []struct {
		name         string
		fixture      func(*testing.T) *serviceFixture
		mutate       func(*run.Record)
		wantErr      bool
		skipFinalize bool
	}{
		{
			name:    "artifact skips remain in immediate pre-finalize state",
			fixture: artifactReadyServiceFixture,
		},
		{
			name:    "artifact skips progress to succeeded",
			fixture: finalizedArtifact,
		},
		{
			name:    "artifact skips reject awaiting-approval regression",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				record.Status = run.StatusAwaitingApproval
			},
			wantErr: true,
		},
		{
			name:    "artifact skips allow retryable finalize failure",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				installFinalizeFailure(record, run.StatusExecuting, true)
			},
		},
		{
			name:    "artifact skips allow exhausted finalize failure",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				installFinalizeFailure(record, run.StatusFailed, false)
			},
		},
		{
			name:    "artifact skips allow finalize cancellation",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				installFinalizeCancellation(record, run.StatusCanceled)
			},
		},
		{
			name:    "artifact skips reject earlier terminal failure",
			fixture: artifactReadyServiceFixture,
			mutate: func(record *run.Record) {
				installEarlierFailure(record, run.StatusFailed)
			},
			wantErr: true,
		},
		{
			name:    "approved pull request remains awaiting approval before publish",
			fixture: approvedServiceFixture,
		},
		{
			name:    "approved pull request rejects executing regression",
			fixture: approvedServiceFixture,
			mutate: func(record *run.Record) {
				record.Status = run.StatusExecuting
			},
			wantErr: true,
		},
		{
			name:    "approved pull request may have running publish attempt",
			fixture: approvedServiceFixture,
			mutate: func(record *run.Record) {
				installRunningPublish(record)
			},
		},
		{
			name:    "approved pull request cannot publish without attempt history",
			fixture: approvedServiceFixture,
			mutate: func(record *run.Record) {
				record.Status = run.StatusPublishing
			},
			wantErr: true,
		},
		{
			name: "retryable publish failure remains publishing",
			fixture: func(t *testing.T) *serviceFixture {
				return publishFailedServiceFixture(t, true)
			},
		},
		{
			name: "terminal publish failure remains failed",
			fixture: func(t *testing.T) *serviceFixture {
				return publishFailedServiceFixture(t, false)
			},
		},
		{
			name:    "completed publication remains publishing before finalize",
			fixture: publishedServiceFixture,
		},
		{
			name:    "completed publication progresses to succeeded",
			fixture: finalizedPublication,
		},
		{
			name:    "completed publication rejects awaiting-approval regression",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Status = run.StatusAwaitingApproval
			},
			wantErr: true,
		},
		{
			name:    "completed publication rejects executing regression",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				record.Status = run.StatusExecuting
			},
			wantErr: true,
		},
		{
			name:    "completed publication rejects stale earlier active failure",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				installEarlierFailure(record, run.StatusPublishing)
			},
			wantErr: true,
		},
		{
			name:    "completed publication rejects earlier terminal failure",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				installEarlierFailure(record, run.StatusFailed)
			},
			wantErr: true,
		},
		{
			name:    "completed publication rejects terminal failure without finalize evidence",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				failure := finalizeTestFailure(false)
				record.Status = run.StatusFailed
				record.Failure = &failure
			},
			wantErr: true,
		},
		{
			name:    "completed publication rejects unbound retryable finalize failure",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				installFinalizeFailure(record, run.StatusPublishing, true)
				record.Failure = nil
			},
			wantErr: true,
		},
		{
			name:    "completed publication allows exhausted finalize failure",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				installFinalizeFailure(record, run.StatusFailed, false)
			},
		},
		{
			name:    "completed publication allows finalize cancellation",
			fixture: publishedServiceFixture,
			mutate: func(record *run.Record) {
				installFinalizeCancellation(record, run.StatusCanceled)
			},
		},
		{
			name:    "succeeded publication rejects stale earlier failure",
			fixture: finalizedPublication,
			mutate: func(record *run.Record) {
				installEarlierFailure(record, run.StatusSucceeded)
			},
			wantErr:      true,
			skipFinalize: true,
		},
		{
			name:    "completed publication rejects finalize failure after success",
			fixture: finalizedPublication,
			mutate: func(record *run.Record) {
				installFinalizeFailure(record, run.StatusFailed, false)
			},
			wantErr:      true,
			skipFinalize: true,
		},
		{
			name:    "succeeded publication requires canonical finalize evidence",
			fixture: finalizedPublication,
			mutate: func(record *run.Record) {
				record.Stages = withoutStage(record.Stages, finalizeStage)
			},
			wantErr:      true,
			skipFinalize: true,
		},
		{
			name:    "succeeded pull request requires completed publication",
			fixture: approvedServiceFixture,
			mutate: func(record *run.Record) {
				installFinalizeSuccess(record)
				record.Status = run.StatusSucceeded
				record.OutcomeMemorySaved = true
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.fixture(t)
			record, err := fixture.runs.Load(context.Background(), "run-123")
			if err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(&record)
			}
			input, err := validateRunBinding(record)
			if err != nil {
				t.Fatalf("validateRunBinding() error = %v", err)
			}
			err = fixture.service.validateDurableEvidence(context.Background(), record, input)
			if test.wantErr && err == nil {
				t.Fatal("validateDurableEvidence() error = nil")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateDurableEvidence() error = %v", err)
			}
			_, resultErr := fixture.service.resultFromRecord(context.Background(), record)
			if test.wantErr && resultErr == nil {
				t.Fatal("resultFromRecord() error = nil")
			}
			if !test.wantErr && resultErr != nil {
				t.Fatalf("resultFromRecord() error = %v", resultErr)
			}
			if !test.wantErr || test.skipFinalize {
				return
			}

			finalizeFixture := test.fixture(t)
			outcomes := &outcomeMemory{}
			finalizeFixture.service.memory = outcomes
			finalizeFixture.service.runs = &mutateSaveResultStore{
				Store: finalizeFixture.runs,
				mutate: func(record run.Record) run.Record {
					test.mutate(&record)
					return record
				},
			}
			if _, err := finalizeFixture.service.Finalize(
				context.Background(), "run-123",
			); err == nil || outcomes.SaveCount() != 0 {
				t.Fatalf("Finalize() error=%v memory saves=%d, want rejection before memory",
					err, outcomes.SaveCount())
			}
		})
	}
}

func TestServiceValidateFinalizeHistoryForEveryProviderPhase(t *testing.T) {
	upstreamFailure := func(t *testing.T) *serviceFixture {
		fixture := newServiceFixture(t)
		fixture.policy.decision = policy.Decision{
			Allowed: false,
			Findings: []policy.Finding{{
				RuleID: "change-denied", Path: "changed.txt",
			}},
		}
		if _, err := fixture.service.Resolve(
			context.Background(), validRawInput("change", "finalize-upstream"),
		); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if _, err := fixture.service.Execute(context.Background(), "run-123"); err == nil {
			t.Fatal("Execute(failure) error = nil")
		}
		return fixture
	}
	approvalRunning := func(t *testing.T) *serviceFixture {
		fixture := completedServiceFixture(t, "pull_request")
		if _, started, err := fixture.service.beginStage(
			context.Background(), "run-123", approvalStage, run.StatusAwaitingApproval,
		); err != nil || !started {
			t.Fatalf("beginStage(approval) started=%v error=%v", started, err)
		}
		return fixture
	}
	publishRunning := func(t *testing.T) *serviceFixture {
		fixture := approvedServiceFixture(t)
		if _, started, err := fixture.service.beginStage(
			context.Background(), "run-123", publishStage, run.StatusPublishing,
		); err != nil || !started {
			t.Fatalf("beginStage(publish) started=%v error=%v", started, err)
		}
		return fixture
	}
	tests := []struct {
		name        string
		fixture     func(*testing.T) *serviceFixture
		mutate      func(*run.Record)
		checkClosed bool
	}{
		{
			name:        "artifact-less upstream failure rejects malformed finalize",
			fixture:     upstreamFailure,
			mutate:      installMalformedFinalizeFailure,
			checkClosed: true,
		},
		{
			name:    "approval running rejects finalize history",
			fixture: approvalRunning,
			mutate:  installRunningFinalize,
		},
		{
			name: "approval retryable failure rejects finalize history",
			fixture: func(t *testing.T) *serviceFixture {
				return approvalFailedServiceFixture(t, true)
			},
			mutate: installRunningFinalize,
		},
		{
			name: "approval terminal failure rejects malformed finalize",
			fixture: func(t *testing.T) *serviceFixture {
				return approvalFailedServiceFixture(t, false)
			},
			mutate:      installMalformedFinalizeFailure,
			checkClosed: true,
		},
		{
			name:        "declined approval rejects malformed finalize",
			fixture:     declinedServiceFixture,
			mutate:      installMalformedFinalizeFailure,
			checkClosed: true,
		},
		{
			name:    "publish running rejects finalize history",
			fixture: publishRunning,
			mutate:  installRunningFinalize,
		},
		{
			name: "publish retryable failure rejects finalize history",
			fixture: func(t *testing.T) *serviceFixture {
				return publishFailedServiceFixture(t, true)
			},
			mutate: installRunningFinalize,
		},
		{
			name: "publish terminal failure rejects malformed finalize",
			fixture: func(t *testing.T) *serviceFixture {
				return publishFailedServiceFixture(t, false)
			},
			mutate:      installMalformedFinalizeFailure,
			checkClosed: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.fixture(t)
			record, err := fixture.runs.Load(context.Background(), "run-123")
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&record)
			input, err := validateRunBinding(record)
			if err != nil {
				t.Fatalf("validateRunBinding() error = %v", err)
			}
			if err := fixture.service.validateDurableEvidence(
				context.Background(), record, input,
			); err == nil {
				t.Fatal("validateDurableEvidence() error = nil")
			}
			if _, err := fixture.service.resultFromRecord(
				context.Background(), record,
			); err == nil {
				t.Fatal("resultFromRecord() error = nil")
			}
			if !test.checkClosed {
				return
			}

			outcomes := &outcomeMemory{}
			fixture.service.memory = outcomes
			fixture.runs.loadMutate = func(record run.Record) run.Record {
				test.mutate(&record)
				return record
			}
			if result, err := fixture.service.Finalize(
				context.Background(), "run-123",
			); err == nil || result.RunID != "run-123" || outcomes.SaveCount() != 0 {
				t.Fatalf("Finalize(closed malformed) result=%#v error=%v saves=%d",
					result, err, outcomes.SaveCount())
			}
		})
	}
}

func TestServiceValidateFinalizeStartsAfterProviderCompletion(t *testing.T) {
	tests := []struct {
		name     string
		fixture  func(*testing.T) *serviceFixture
		boundary string
		reorder  bool
		mutate   func(*run.Record)
	}{
		{
			name:     "artifact finalize ordered before publish skip",
			fixture:  artifactReadyServiceFixture,
			boundary: publishStage,
			reorder:  true,
			mutate:   installRunningFinalize,
		},
		{
			name:     "artifact finalize starts when publish skip finishes",
			fixture:  artifactReadyServiceFixture,
			boundary: publishStage,
			mutate:   installRunningFinalize,
		},
		{
			name:     "published finalize ordered before publish success",
			fixture:  publishedServiceFixture,
			boundary: publishStage,
			reorder:  true,
			mutate:   installRunningFinalize,
		},
		{
			name:     "published finalize starts when publish success finishes",
			fixture:  publishedServiceFixture,
			boundary: publishStage,
			mutate:   installRunningFinalize,
		},
		{
			name: "terminal approval failure finalize starts before approval",
			fixture: func(t *testing.T) *serviceFixture {
				return approvalFailedServiceFixture(t, false)
			},
			boundary: approvalStage,
			mutate:   installUpstreamFinalizeFailure,
		},
		{
			name:     "declined finalize starts before approval completion",
			fixture:  declinedServiceFixture,
			boundary: approvalStage,
			mutate:   installRunningFinalize,
		},
		{
			name: "terminal publish failure finalize starts before publish",
			fixture: func(t *testing.T) *serviceFixture {
				return publishFailedServiceFixture(t, false)
			},
			boundary: publishStage,
			mutate:   installUpstreamFinalizeFailure,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.fixture(t)
			record, err := fixture.runs.Load(context.Background(), "run-123")
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&record)
			if test.reorder {
				moveLatestStageBefore(&record, finalizeStage, test.boundary)
			} else {
				startFinalizeAtProviderFinish(&record, test.boundary)
			}
			input, err := validateRunBinding(record)
			if err != nil {
				t.Fatalf("validateRunBinding() error = %v", err)
			}
			if err := fixture.service.validateDurableEvidence(
				context.Background(), record, input,
			); err == nil {
				t.Fatal("validateDurableEvidence() error = nil")
			}
			if _, err := fixture.service.resultFromRecord(
				context.Background(), record,
			); err == nil {
				t.Fatal("resultFromRecord() error = nil")
			}
		})
	}
}

func TestServiceValidateFinalizeStartsAfterUpstreamFailure(t *testing.T) {
	fixtures := []struct {
		name  string
		stage string
		build func(*testing.T) *serviceFixture
	}{
		{name: "resolve", stage: "resolve", build: resolveFailedServiceFixture},
		{name: "execute", stage: "execute", build: executeFailedServiceFixture},
	}
	mutations := []struct {
		name   string
		mutate func(*run.Record, string)
	}{
		{
			name: "reordered",
			mutate: func(record *run.Record, stage string) {
				moveLatestStageBefore(record, finalizeStage, stage)
			},
		},
		{
			name: "equal finish",
			mutate: func(record *run.Record, stage string) {
				startFinalizeRelativeToStage(record, stage, "equal")
			},
		},
		{
			name: "overlap",
			mutate: func(record *run.Record, stage string) {
				startFinalizeRelativeToStage(record, stage, "overlap")
			},
		},
		{
			name: "reversed",
			mutate: func(record *run.Record, stage string) {
				startFinalizeRelativeToStage(record, stage, "reversed")
			},
		},
	}

	for _, fixtureTest := range fixtures {
		for _, mutation := range mutations {
			t.Run(fixtureTest.name+"/"+mutation.name, func(t *testing.T) {
				fixture := fixtureTest.build(t)
				record, err := fixture.runs.Load(context.Background(), "run-123")
				if err != nil {
					t.Fatal(err)
				}
				installUpstreamFinalizeFailure(&record)
				mutation.mutate(&record, fixtureTest.stage)
				input, err := validateRunBinding(record)
				if err != nil {
					t.Fatalf("validateRunBinding() error = %v", err)
				}
				if err := fixture.service.validateDurableEvidence(
					context.Background(), record, input,
				); err == nil {
					t.Fatal("validateDurableEvidence() error = nil")
				}
				if _, err := fixture.service.resultFromRecord(
					context.Background(), record,
				); err == nil {
					t.Fatal("resultFromRecord() error = nil")
				}
			})
		}
	}
}

func TestServiceValidateFinalizePreservesUpstreamFailure(t *testing.T) {
	fixtures := []struct {
		name  string
		stage string
		build func(*testing.T) *serviceFixture
	}{
		{name: "resolve", stage: "resolve", build: resolveFailedServiceFixture},
		{name: "execute", stage: "execute", build: executeFailedServiceFixture},
	}
	mutations := []struct {
		name        string
		mutate      func(*run.Record, string)
		checkClosed bool
	}{
		{
			name: "finalize replaces top failure",
			mutate: func(record *run.Record, _ string) {
				finalize := providerStagePointer(record, finalizeStage, 1)
				failure := finalizeTestFailure(false)
				finalize.Failure = &failure
				record.Failure = &failure
			},
			checkClosed: true,
		},
		{
			name: "upstream top failure is missing",
			mutate: func(record *run.Record, _ string) {
				record.Failure = nil
			},
		},
		{
			name: "upstream top failure differs from stage",
			mutate: func(record *run.Record, _ string) {
				failure := *record.Failure
				failure.CauseCode = "different_upstream_failure"
				record.Failure = &failure
			},
		},
		{
			name: "upstream failure stage is missing",
			mutate: func(record *run.Record, stage string) {
				record.Stages = withoutStage(record.Stages, stage)
			},
		},
		{
			name: "upstream failure stage is not failed",
			mutate: func(record *run.Record, stage string) {
				upstream := providerStagePointer(
					record, stage, stageAttempt(*record, stage),
				)
				upstream.Status = run.StageSucceeded
			},
		},
	}

	for _, fixtureTest := range fixtures {
		for _, mutation := range mutations {
			t.Run(fixtureTest.name+"/"+mutation.name, func(t *testing.T) {
				fixture := fixtureTest.build(t)
				record, err := fixture.runs.Load(context.Background(), "run-123")
				if err != nil {
					t.Fatal(err)
				}
				installUpstreamFinalizeFailure(&record)
				mutation.mutate(&record, fixtureTest.stage)
				input, err := validateRunBinding(record)
				if err != nil {
					t.Fatalf("validateRunBinding() error = %v", err)
				}
				if err := fixture.service.validateDurableEvidence(
					context.Background(), record, input,
				); err == nil {
					t.Fatal("validateDurableEvidence() error = nil")
				}
				if _, err := fixture.service.resultFromRecord(
					context.Background(), record,
				); err == nil {
					t.Fatal("resultFromRecord() error = nil")
				}
				if !mutation.checkClosed {
					return
				}
				outcomes := &outcomeMemory{}
				fixture.service.memory = outcomes
				fixture.runs.loadMutate = func(record run.Record) run.Record {
					installUpstreamFinalizeFailure(&record)
					mutation.mutate(&record, fixtureTest.stage)
					return record
				}
				if result, err := fixture.service.Finalize(
					context.Background(), "run-123",
				); err == nil || result.RunID != "run-123" ||
					outcomes.SaveCount() != 0 {
					t.Fatalf("Finalize(finalize-only failure) result=%#v error=%v saves=%d",
						result, err, outcomes.SaveCount())
				}
			})
		}
	}
}

func completedServiceFixture(t *testing.T, mode string) *serviceFixture {
	t.Helper()
	fixture := newServiceFixture(t)
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{
			Output: "agent-secret /tmp/workspace", Started: true, Completed: true,
		}, nil
	}
	raw := validRawInput("memory-secret change", "service-flow")
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	publication := map[string]any{"mode": mode}
	if mode == "pull_request" {
		publication["provider"] = "github"
		publication["target_branch"] = "main"
		publication["title"] = "Safe change"
		input["repository_uri"] = "https://github.com/araihu/paje"
		fixture.resolver.revision.RepositoryURI = "https://github.com/araihu/paje.git"
	}
	input["publication"] = publication
	raw, _ = json.Marshal(input)
	if _, err := fixture.service.Resolve(context.Background(), raw); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	fixture.capturer.capture = nil
	fixture.capturer.capture = func(_ context.Context, _ gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}
	if _, err := fixture.service.Execute(context.Background(), "run-123"); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var providerClockMu sync.Mutex
	providerNow := time.Unix(199, 0).UTC()
	fixture.service.clock = func() time.Time {
		providerClockMu.Lock()
		defer providerClockMu.Unlock()
		current := providerNow
		providerNow = providerNow.Add(time.Second)
		return current
	}
	if mode == "artifact" {
		if _, err := fixture.service.Approval(
			context.Background(), "run-123",
			approvalmock.NewGate(approval.Result{}, errors.New("must not be called")),
		); err != nil {
			t.Fatalf("Approval(artifact) error = %v", err)
		}
		if _, err := fixture.service.Publish(context.Background(), "run-123"); err != nil {
			t.Fatalf("Publish(artifact) error = %v", err)
		}
	}
	return fixture
}

func resolveFailedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := newServiceFixture(t)
	fixture.resolver.revision.SourceDirty = true
	if _, err := fixture.service.Resolve(
		context.Background(), validRawInput("change", "resolve-failure"),
	); err == nil {
		t.Fatal("Resolve(source dirty) error = nil")
	}
	record, err := fixture.runs.Load(context.Background(), "run-123")
	if err != nil || record.Status != run.StatusFailed ||
		record.Failure == nil || record.Failure.Stage != "resolve" ||
		record.Artifact != nil {
		t.Fatalf("resolve failure fixture = %#v error=%v", record, err)
	}
	return fixture
}

func executeFailedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := newServiceFixture(t)
	fixture.policy.decision = policy.Decision{
		Allowed: false,
		Findings: []policy.Finding{{
			RuleID: "change-denied", Path: "changed.txt",
		}},
	}
	if _, err := fixture.service.Resolve(
		context.Background(), validRawInput("change", "execute-failure"),
	); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if _, err := fixture.service.Execute(context.Background(), "run-123"); err == nil {
		t.Fatal("Execute(policy failure) error = nil")
	}
	record, err := fixture.runs.Load(context.Background(), "run-123")
	if err != nil || record.Status != run.StatusFailed ||
		record.Failure == nil || record.Failure.Stage != "execute" ||
		record.Artifact != nil {
		t.Fatalf("execute failure fixture = %#v error=%v", record, err)
	}
	return fixture
}

func approvedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := completedServiceFixture(t, "pull_request")
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	gate := approvalmock.NewGate(approval.Result{
		RunID: record.ID, ArtifactDigest: record.Artifact.Digest, Approved: true,
		Actor: "reviewer", DecidedAt: time.Unix(200, 0).UTC(),
	}, nil)
	if _, err := fixture.service.Approval(context.Background(), "run-123", gate); err != nil {
		t.Fatalf("Approval() error = %v", err)
	}
	return fixture
}

func publishedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := approvedServiceFixture(t)
	fixture.service.publisher = &sequencePublisher{results: []publisher.Result{{
		Provider: "github", Branch: "paje/code-change/run-123",
		CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
		PullRequestURL: "https://example.test/pull/42",
	}}}
	if _, err := fixture.service.Publish(context.Background(), "run-123"); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	return fixture
}

func artifactReadyServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	return completedServiceFixture(t, "artifact")
}

func declinedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := completedServiceFixture(t, "pull_request")
	record, err := fixture.runs.Load(context.Background(), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	gate := approvalmock.NewGate(approval.Result{
		RunID: record.ID, ArtifactDigest: record.Artifact.Digest,
		Actor: "reviewer", DecidedAt: time.Unix(200, 0).UTC(), Reason: "needs changes",
	}, nil)
	if _, err := fixture.service.Approval(context.Background(), "run-123", gate); err != nil {
		t.Fatalf("Approval() error = %v", err)
	}
	return fixture
}

func approvalFailedServiceFixture(t *testing.T, retryable bool) *serviceFixture {
	t.Helper()
	fixture := completedServiceFixture(t, "pull_request")
	providerErr := error(errors.New("approval unavailable"))
	if retryable {
		providerErr = retryableTestError{providerErr}
	}
	if _, err := fixture.service.Approval(
		context.Background(), "run-123",
		approvalmock.NewGate(approval.Result{}, providerErr),
	); err == nil {
		t.Fatal("Approval(failure) error = nil")
	}
	return fixture
}

func publishFailedServiceFixture(t *testing.T, retryable bool) *serviceFixture {
	t.Helper()
	fixture := approvedServiceFixture(t)
	providerErr := error(errors.New("publisher unavailable"))
	if retryable {
		providerErr = retryableTestError{providerErr}
	}
	fixture.service.publisher = &sequencePublisher{errors: []error{providerErr}}
	if _, err := fixture.service.Publish(context.Background(), "run-123"); err == nil {
		t.Fatal("Publish(failure) error = nil")
	}
	return fixture
}

func approvalRetriedPublishedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := approvalFailedServiceFixture(t, true)
	record, err := fixture.runs.Load(context.Background(), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	gate := gateFunc(func(context.Context, approval.Request) (approval.Result, error) {
		return approval.Result{
			RunID: record.ID, ArtifactDigest: record.Artifact.Digest, Approved: true,
			Actor: "reviewer", DecidedAt: fixture.service.clock(),
		}, nil
	})
	if _, err := fixture.service.Approval(context.Background(), "run-123", gate); err != nil {
		t.Fatalf("Approval(retry) error = %v", err)
	}
	fixture.service.publisher = &sequencePublisher{results: []publisher.Result{{
		Provider: "github", Branch: "paje/code-change/run-123",
		CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
		PullRequestURL: "https://example.test/pull/42",
	}}}
	if _, err := fixture.service.Publish(context.Background(), "run-123"); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	return fixture
}

func publishRetriedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := publishFailedServiceFixture(t, true)
	fixture.service.publisher = &sequencePublisher{results: []publisher.Result{{
		Provider: "github", Branch: "paje/code-change/run-123",
		CommitSHA: strings.Repeat("c", 40), PullRequestID: "42",
		PullRequestURL: "https://example.test/pull/42",
	}}}
	if _, err := fixture.service.Publish(context.Background(), "run-123"); err != nil {
		t.Fatalf("Publish(retry) error = %v", err)
	}
	return fixture
}

func withoutStage(stages []run.StageResult, name string) []run.StageResult {
	result := make([]run.StageResult, 0, len(stages))
	for _, stage := range stages {
		if stage.Name != name {
			result = append(result, stage)
		}
	}
	return result
}

func setLatestStageStatus(record *run.Record, name string, status run.StageStatus) {
	latest, found := latestStage(*record, name)
	if !found {
		return
	}
	for index := range record.Stages {
		if record.Stages[index].Name == name &&
			record.Stages[index].Attempts == latest.Attempts {
			record.Stages[index].Status = status
		}
	}
}

func providerStagePointer(record *run.Record, name string, attempt int) *run.StageResult {
	for index := range record.Stages {
		stage := &record.Stages[index]
		if stage.Name == name && stage.Attempts == attempt {
			return stage
		}
	}
	panic("provider stage not found")
}

func providerFailure(stage string, class run.FailureClass, retryable bool) run.Failure {
	return run.Failure{
		Stage: stage, Class: class, Retryable: retryable,
		Diagnostic: "provider failed", CauseCode: "provider_failed",
	}
}

func providerStages(record run.Record, name string) []run.StageResult {
	result := make([]run.StageResult, 0)
	for _, stage := range record.Stages {
		if stage.Name == name {
			result = append(result, stage)
		}
	}
	return result
}

func installRunningPublish(record *run.Record) {
	startedAt := record.UpdatedAt.Add(time.Second)
	record.Status = run.StatusPublishing
	record.Failure = nil
	record.Stages = append(record.Stages, run.StageResult{
		Name: publishStage, Status: run.StageRunning,
		StartedAt: startedAt, Attempts: stageAttempt(*record, publishStage) + 1,
	})
	record.UpdatedAt = startedAt
}

func installRunningFinalize(record *run.Record) {
	startedAt := record.UpdatedAt.Add(time.Second)
	record.Stages = append(record.Stages, run.StageResult{
		Name: finalizeStage, Status: run.StageRunning,
		StartedAt: startedAt, Attempts: stageAttempt(*record, finalizeStage) + 1,
	})
	record.UpdatedAt = startedAt
}

func installMalformedFinalizeFailure(record *run.Record) {
	startedAt := record.UpdatedAt.Add(time.Second)
	finishedAt := startedAt.Add(time.Second)
	failure := run.Failure{
		Stage: finalizeStage, Class: run.FailureInternal, Retryable: false,
		Diagnostic: "malformed finalize bookkeeping", CauseCode: "malformed_finalize",
	}
	record.Stages = append(record.Stages, run.StageResult{
		Name: finalizeStage, Status: run.StageFailed,
		StartedAt: startedAt, FinishedAt: finishedAt,
		Attempts: stageAttempt(*record, finalizeStage) + 1, Failure: &failure,
	})
	record.UpdatedAt = finishedAt
}

func installUpstreamFinalizeFailure(record *run.Record) {
	startedAt := record.UpdatedAt.Add(time.Second)
	finishedAt := startedAt.Add(time.Second)
	failure := finalizeMemoryFailure()
	record.Stages = append(record.Stages, run.StageResult{
		Name: finalizeStage, Status: run.StageFailed,
		StartedAt: startedAt, FinishedAt: finishedAt,
		Attempts: stageAttempt(*record, finalizeStage) + 1, Failure: &failure,
	})
	record.UpdatedAt = finishedAt
}

func moveLatestStageBefore(record *run.Record, name, boundary string) {
	stageIndex := -1
	boundaryIndex := -1
	highestBoundaryAttempt := 0
	for index, stage := range record.Stages {
		if stage.Name == name && (stageIndex == -1 ||
			stage.Attempts > record.Stages[stageIndex].Attempts) {
			stageIndex = index
		}
		if stage.Name == boundary && stage.Attempts > highestBoundaryAttempt {
			boundaryIndex = index
			highestBoundaryAttempt = stage.Attempts
		}
	}
	if stageIndex == -1 || boundaryIndex == -1 {
		panic("stage or boundary not found")
	}
	stage := record.Stages[stageIndex]
	without := append(
		append([]run.StageResult(nil), record.Stages[:stageIndex]...),
		record.Stages[stageIndex+1:]...,
	)
	if stageIndex < boundaryIndex {
		boundaryIndex--
	}
	reordered := make([]run.StageResult, 0, len(record.Stages))
	reordered = append(reordered, without[:boundaryIndex]...)
	reordered = append(reordered, stage)
	reordered = append(reordered, without[boundaryIndex:]...)
	record.Stages = reordered
}

func startFinalizeAtProviderFinish(record *run.Record, provider string) {
	providerStage, found := latestStage(*record, provider)
	if !found {
		panic("provider stage not found")
	}
	finalize := providerStagePointer(record, finalizeStage, 1)
	duration := finalize.FinishedAt.Sub(finalize.StartedAt)
	finalize.StartedAt = providerStage.FinishedAt
	if finalize.Status != run.StageRunning {
		finalize.FinishedAt = finalize.StartedAt.Add(max(duration, time.Second))
	}
}

func startFinalizeRelativeToStage(record *run.Record, stageName, relation string) {
	upstream, found := latestStage(*record, stageName)
	if !found {
		panic("upstream stage not found")
	}
	finalize := providerStagePointer(record, finalizeStage, 1)
	duration := finalize.FinishedAt.Sub(finalize.StartedAt)
	switch relation {
	case "equal":
		finalize.StartedAt = upstream.FinishedAt
	case "overlap":
		finalize.StartedAt = upstream.FinishedAt.Add(-time.Nanosecond)
	case "reversed":
		finalize.StartedAt = upstream.StartedAt.Add(-time.Second)
	default:
		panic("unknown finalize relation")
	}
	finalize.FinishedAt = finalize.StartedAt.Add(max(duration, time.Second))
}

func finalizeTestFailure(retryable bool) run.Failure {
	causeCode := "retries_exhausted"
	if retryable {
		causeCode = outcomeFailureReason
	}
	return run.Failure{
		Stage: finalizeStage, Class: run.FailureInternal, Retryable: retryable,
		Diagnostic: outcomeFailureReason, CauseCode: causeCode,
	}
}

func installFinalizeFailure(record *run.Record, status run.Status, retryable bool) {
	startedAt := record.UpdatedAt.Add(time.Second)
	finishedAt := startedAt.Add(time.Second)
	failure := finalizeTestFailure(retryable)
	record.Status = status
	record.Failure = &failure
	record.Stages = append(record.Stages, run.StageResult{
		Name: finalizeStage, Status: run.StageFailed,
		StartedAt: startedAt, FinishedAt: finishedAt,
		Attempts: stageAttempt(*record, finalizeStage) + 1, Failure: &failure,
	})
	record.UpdatedAt = finishedAt
}

func installFinalizeCancellation(record *run.Record, status run.Status) {
	startedAt := record.UpdatedAt.Add(time.Second)
	finishedAt := startedAt.Add(time.Second)
	failure := canceledFailure(finalizeStage)
	record.Status = status
	record.Failure = &failure
	record.Stages = append(record.Stages, run.StageResult{
		Name: finalizeStage, Status: run.StageFailed,
		StartedAt: startedAt, FinishedAt: finishedAt,
		Attempts: stageAttempt(*record, finalizeStage) + 1, Failure: &failure,
	})
	record.UpdatedAt = finishedAt
}

func installFinalizeSuccess(record *run.Record) {
	startedAt := record.UpdatedAt.Add(time.Second)
	finishedAt := startedAt.Add(time.Second)
	record.Stages = append(record.Stages, run.StageResult{
		Name: finalizeStage, Status: run.StageSucceeded,
		StartedAt: startedAt, FinishedAt: finishedAt,
		Attempts: stageAttempt(*record, finalizeStage) + 1,
		Evidence: map[string]string{"outcome_memory_saved": "true"},
	})
	record.UpdatedAt = finishedAt
}

func installEarlierFailure(record *run.Record, status run.Status) {
	startedAt := record.UpdatedAt.Add(time.Second)
	finishedAt := startedAt.Add(time.Second)
	failure := providerFailure("execute", run.FailureAgent, false)
	record.Status = status
	record.Failure = &failure
	record.Stages = append(record.Stages, run.StageResult{
		Name: "execute", Status: run.StageFailed,
		StartedAt: startedAt, FinishedAt: finishedAt,
		Attempts: stageAttempt(*record, "execute") + 1, Failure: &failure,
	})
	record.UpdatedAt = finishedAt
}

func (f *serviceFixture) publisherCalls() int {
	if pub, ok := f.service.publisher.(interface{ CallCount() int }); ok {
		return pub.CallCount()
	}
	return 0
}

func latestStageStatus(t *testing.T, store *runmock.Store, runID, name string) run.StageStatus {
	t.Helper()
	record, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := latestStage(record, name)
	if !found {
		t.Fatalf("stage %q not found", name)
	}
	return stage.Status
}

type storedOutcome struct {
	Content  string
	Metadata map[string]string
}

type outcomeMemory struct {
	mu         sync.Mutex
	items      []storedOutcome
	searchErr  error
	saveErrors []error
	saveCalls  int
}

func (m *outcomeMemory) Search(ctx context.Context, query string, limit int, tags map[string]string) ([]memory.Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.searchErr != nil {
		return nil, m.searchErr
	}
	var result []memory.Memory
	for index, item := range m.items {
		if !strings.Contains(item.Content, query) || !containsOutcomeTags(item.Metadata, tags) {
			continue
		}
		result = append(result, memory.Memory{
			ID: "outcome-" + string(rune(index+1)), Content: item.Content,
			Metadata: cloneOutcomeTags(item.Metadata),
		})
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (m *outcomeMemory) Save(ctx context.Context, content string, tags map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalls++
	if index := m.saveCalls - 1; index < len(m.saveErrors) && m.saveErrors[index] != nil {
		return m.saveErrors[index]
	}
	m.items = append(m.items, storedOutcome{Content: content, Metadata: cloneOutcomeTags(tags)})
	return nil
}

func (m *outcomeMemory) SaveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveCalls
}

func (m *outcomeMemory) Snapshot() []storedOutcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]storedOutcome, len(m.items))
	for index, item := range m.items {
		result[index] = storedOutcome{Content: item.Content, Metadata: cloneOutcomeTags(item.Metadata)}
	}
	return result
}

func containsOutcomeTags(metadata, tags map[string]string) bool {
	for key, value := range tags {
		if metadata[key] != value {
			return false
		}
	}
	return true
}

func cloneOutcomeTags(tags map[string]string) map[string]string {
	result := make(map[string]string, len(tags))
	for key, value := range tags {
		result[key] = value
	}
	return result
}

type sequencePublisher struct {
	mu       sync.Mutex
	results  []publisher.Result
	errors   []error
	requests []publisher.Request
}

func (p *sequencePublisher) Publish(ctx context.Context, request publisher.Request) (publisher.Result, error) {
	if err := ctx.Err(); err != nil {
		return publisher.Result{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, publisher.CloneRequest(request))
	index := len(p.requests) - 1
	var result publisher.Result
	var err error
	if index < len(p.results) {
		result = p.results[index]
	}
	if index < len(p.errors) {
		err = p.errors[index]
	}
	return result, err
}

func (p *sequencePublisher) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *sequencePublisher) Requests() []publisher.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]publisher.Request, len(p.requests))
	for index, request := range p.requests {
		result[index] = publisher.CloneRequest(request)
	}
	return result
}

type retryableTestError struct{ error }

func (retryableTestError) Retryable() bool { return true }

type cancelOnEvidenceStore struct {
	run.Store
	cancel   context.CancelFunc
	evidence string
	once     sync.Once
}

type mutateSaveResultStore struct {
	run.Store
	mutate func(run.Record) run.Record
}

func (s *mutateSaveResultStore) Save(
	ctx context.Context,
	record run.Record,
	expected uint64,
) (run.Record, error) {
	saved, err := s.Store.Save(ctx, record, expected)
	if err != nil {
		return saved, err
	}
	return s.mutate(saved), nil
}

func (s *cancelOnEvidenceStore) Save(
	ctx context.Context,
	record run.Record,
	expected uint64,
) (run.Record, error) {
	matches := (s.evidence == "approval" && record.Approval != nil) ||
		(s.evidence == "publication" && record.Publication != nil) ||
		(s.evidence == "outcome" && record.OutcomeMemorySaved)
	if matches {
		fired := false
		s.once.Do(func() {
			fired = true
			s.cancel()
		})
		if fired {
			if err := ctx.Err(); err != nil {
				return run.Record{}, err
			}
			return run.Record{}, context.Canceled
		}
	}
	return s.Store.Save(ctx, record, expected)
}

type memoryFunc struct {
	search func(context.Context, string, int, map[string]string) ([]memory.Memory, error)
	save   func(context.Context, string, map[string]string) error
}

func (m memoryFunc) Search(
	ctx context.Context,
	query string,
	limit int,
	tags map[string]string,
) ([]memory.Memory, error) {
	if m.search == nil {
		return nil, nil
	}
	return m.search(ctx, query, limit, tags)
}

func (m memoryFunc) Save(ctx context.Context, content string, tags map[string]string) error {
	if m.save == nil {
		return nil
	}
	return m.save(ctx, content, tags)
}

type blockingMemory struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockingMemory) Search(
	ctx context.Context,
	_ string,
	_ int,
	_ map[string]string,
) ([]memory.Memory, error) {
	m.once.Do(func() { close(m.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.release:
		return nil, nil
	}
}

func (*blockingMemory) Save(context.Context, string, map[string]string) error { return nil }

type blockingLoadArtifact struct {
	artifact.Store
	mu      sync.Mutex
	calls   int
	blockAt int
}

func (s *blockingLoadArtifact) Load(
	ctx context.Context,
	reference artifact.Reference,
) (artifact.Bundle, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call >= s.blockAt {
		<-ctx.Done()
		return artifact.Bundle{}, ctx.Err()
	}
	return s.Store.Load(ctx, reference)
}

type gateFunc func(context.Context, approval.Request) (approval.Result, error)

func (function gateFunc) RequestApproval(
	ctx context.Context,
	request approval.Request,
) (approval.Result, error) {
	return function(ctx, request)
}

type publisherFunc func(context.Context, publisher.Request) (publisher.Result, error)

func (function publisherFunc) Publish(
	ctx context.Context,
	request publisher.Request,
) (publisher.Result, error) {
	return function(ctx, request)
}

var _ = templatecodechange.Result{}
