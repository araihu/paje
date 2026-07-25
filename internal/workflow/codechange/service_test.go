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
		CodexHome: t.TempDir(),
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
}

func TestServicePublishClassifiesConflictAndRetriesTypedOutage(t *testing.T) {
	t.Run("missing approval fails closed before provider", func(t *testing.T) {
		fixture := completedServiceFixture(t, "pull_request")
		pub := &sequencePublisher{}
		fixture.service.publisher = pub
		result, err := fixture.service.Publish(context.Background(), "run-123")
		if err == nil || result.Status != run.StatusFailed ||
			result.FailureClass != run.FailurePublication ||
			result.Retryable || pub.CallCount() != 0 {
			t.Fatalf("Publish() result=%#v error=%v calls=%d", result, err, pub.CallCount())
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

	t.Run("terminal upstream failure records finalize exhaustion bookkeeping", func(t *testing.T) {
		terminal := completedServiceFixture(t, "artifact")
		now := time.Unix(100, 0).UTC()
		terminal.service.clock = func() time.Time {
			now = now.Add(time.Second)
			return now
		}
		record, _ := terminal.runs.Load(context.Background(), "run-123")
		failure := run.Failure{
			Stage: "execute", Class: run.FailureAgent, Retryable: false,
			Diagnostic: "agent failed", CauseCode: "agent_exit",
		}
		record.Failure = &failure
		stage := run.StageResult{
			Name: "execute-failure", Status: run.StageFailed,
			StartedAt: terminal.service.clock(), FinishedAt: terminal.service.clock(),
			Attempts: 1, Failure: &failure,
		}
		stage.Name = failure.Stage
		stage.Attempts = stageAttempt(record, failure.Stage) + 1
		record, _ = run.UpsertStage(record, stage)
		record, _ = run.Transition(record, run.StatusFailed, terminal.service.clock())
		if _, err := terminal.runs.Save(context.Background(), record, record.Version); err != nil {
			t.Fatal(err)
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
			record.OutcomeMemorySaved || record.Failure.Class != run.FailureAgent {
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

type retryableTestError struct{ error }

func (retryableTestError) Retryable() bool { return true }

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
