package codechange

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	artifactmock "github.com/araihu/paje/internal/artifact/mock"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/publisher"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	runmock "github.com/araihu/paje/internal/run/mock"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workspace"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestExecuteUsesFreshRealWorktreeAndPersistsCompleteArtifact(t *testing.T) {
	source, baseSHA := createGitSource(t)
	managerRoot := t.TempDir()
	manager, err := gitworktree.New(managerRoot)
	if err != nil {
		t.Fatalf("gitworktree.New() error = %v", err)
	}
	runtimeRoot := t.TempDir()
	codexHome := t.TempDir()
	envPolicy, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: runtimeRoot,
		Source: map[string]string{
			"PATH": os.Getenv("PATH"), "HATCHET_CLIENT_TOKEN": "hatchet-secret",
			"MEM0_API_KEY": "mem0-secret", "GITHUB_TOKEN": "github-secret",
		},
		CodexHome: codexHome,
	})
	if err != nil {
		t.Fatalf("environment.NewPolicy() error = %v", err)
	}
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatalf("gitcapture.New() error = %v", err)
	}
	changePolicy, err := policy.NewChangePolicy(policy.Config{})
	if err != nil {
		t.Fatalf("policy.NewChangePolicy() error = %v", err)
	}
	registry, _ := template.NewRegistry(templatecodechange.Definition{})
	profile := &workspaceProfile{}
	agent := &writingAgent{}
	verifier := &recordingVerifier{}
	mem := &recordingMemory{result: []memory.Memory{{ID: "memory-1", Content: "Keep the public API stable"}}}
	runs := runmock.NewStore()
	artifacts := artifactmock.NewStore()
	service, err := New(Dependencies{
		Templates: registry, Runs: runs, Memory: mem, Resolver: manager,
		Workspaces: manager, Profiles: map[string]repository.Profile{
			"generic": profile, "go": &fakeProfile{name: "go"},
		},
		Environments: envPolicy, Agent: agent, Verifier: verifier,
		Capturer: capturer, Policy: changePolicy, Artifacts: artifacts,
		Publisher: publishermock.NewPublisher(structPublisherResult(), nil),
		Clock:     func() time.Time { return time.Unix(100, 0).UTC() },
		NewID:     func() string { return "run-123" },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	raw := rawForRepository(source)
	resolved, err := service.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if record, _ := runs.Load(context.Background(), resolved.RunID); record.BaseSHA != baseSHA {
		t.Fatalf("resolved BaseSHA = %q, want %q", record.BaseSHA, baseSHA)
	}

	result, err := service.Execute(context.Background(), resolved.RunID)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != run.StatusExecuting || result.Artifact == nil {
		t.Fatalf("Execute() result = %#v", result)
	}
	requests := agent.Requests()
	if len(requests) != 1 {
		t.Fatalf("agent requests = %d, want 1", len(requests))
	}
	for _, value := range []string{
		"Keep the public API stable", "alpha: first", "zeta: last",
		"Base SHA: " + baseSHA, "Profile: generic",
	} {
		if !strings.Contains(requests[0].TaskDescription, value) {
			t.Errorf("agent prompt does not contain %q:\n%s", value, requests[0].TaskDescription)
		}
	}
	if requests[0].Env["CODEX_HOME"] == "" {
		t.Error("agent environment lacks CODEX_HOME")
	}
	for _, denied := range []string{"HATCHET_CLIENT_TOKEN", "MEM0_API_KEY", "GITHUB_TOKEN", "GH_TOKEN"} {
		if _, exists := requests[0].Env[denied]; exists {
			t.Errorf("agent environment contains %s", denied)
		}
	}
	if got := verifier.Commands(); len(got) != 2 ||
		!strings.HasSuffix(got[0].Directory, "module-a") ||
		!strings.HasSuffix(got[1].Directory, "module-b") {
		t.Fatalf("verification commands = %#v", got)
	}

	snapshot := artifacts.Snapshot()
	if len(snapshot.Saves) != 1 {
		t.Fatalf("artifact saves = %d, want 1", len(snapshot.Saves))
	}
	bundle := snapshot.Bundles[result.Artifact.Digest]
	if len(bundle.ChangesPatch) == 0 || !strings.Contains(string(bundle.AgentOutput), "updated changed.txt") {
		t.Fatalf("artifact patch/output missing: %#v", bundle)
	}
	if bundle.Manifest.BaseSHA != baseSHA || bundle.Manifest.TreeSHA == "" ||
		bundle.Manifest.MemoryCount != 1 || len(bundle.Manifest.MemoryIDs) != 1 ||
		bundle.Manifest.MemoryIDs[0] != "memory-1" {
		t.Fatalf("artifact manifest = %#v", bundle.Manifest)
	}
	if bundle.Preflight["alpha"] != "first" || len(bundle.Verification) != 2 ||
		len(bundle.Warnings) != 1 || !strings.Contains(string(bundle.ExecutionMetadata), `"completed":true`) {
		t.Fatalf("artifact evidence incomplete: %#v", bundle)
	}
	if bundle.Verification[0].Command.Directory != "module-a" ||
		bundle.Verification[1].Command.Directory != "module-b" {
		t.Fatalf("durable verification directories = %#v, want workspace-relative", bundle.Verification)
	}
	record, _ := runs.Load(context.Background(), resolved.RunID)
	serializedBundle, _ := json.Marshal(bundle)
	serializedRecord, _ := json.Marshal(record)
	serialized := strings.Join([]string{
		string(serializedBundle), string(serializedRecord), string(bundle.AgentOutput),
		string(bundle.ChangesPatch),
	}, "\n")
	if strings.Contains(string(serializedBundle), "Keep the public API stable") {
		t.Fatal("artifact leaked memory content")
	}
	for _, ephemeral := range []string{requests[0].WorkspacePath, runtimeRoot, codexHome} {
		if strings.Contains(serialized, ephemeral) {
			t.Fatalf("durable evidence leaked ephemeral prefix %q:\n%s", ephemeral, serialized)
		}
	}
	if _, err := os.Stat(filepath.Join(source, "changed.txt")); !os.IsNotExist(err) {
		t.Fatalf("source repository was changed: %v", err)
	}
	assertDirectoryEmpty(t, filepath.Join(managerRoot, "worktrees"))
	if _, err := os.Stat(filepath.Join(runtimeRoot, "run-123")); !os.IsNotExist(err) {
		t.Fatalf("runtime directory remains: %v", err)
	}

	again, err := service.Execute(context.Background(), resolved.RunID)
	if err != nil {
		t.Fatalf("Execute(second) error = %v", err)
	}
	if again.Artifact == nil || *again.Artifact != *result.Artifact ||
		len(agent.Requests()) != 1 || len(artifacts.Snapshot().Saves) != 1 {
		t.Fatalf("second Execute was not idempotent: result=%#v calls=%d saves=%d", again, len(agent.Requests()), len(artifacts.Snapshot().Saves))
	}
}

func TestExecuteCompletedAgentFailureRetainsSafeArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{
			Output: "agent failed safely", Transcript: `{"type":"item.completed"}`,
			ExitCode: 7, Started: true, Completed: true,
		}, nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	if result.Status != run.StatusFailed || result.FailureClass != run.FailureAgent ||
		result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result = %#v", result)
	}
	bundle := fixture.artifacts.Snapshot().Bundles[result.Artifact.Digest]
	if string(bundle.AgentOutput) != "agent failed safely" {
		t.Fatalf("artifact agent output = %q", bundle.AgentOutput)
	}
}

func TestExecuteRequiredVerificationFailureRetainsEvidenceArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.profile.result = repository.ProfileResult{
		Facts: map[string]string{"base_sha": fixture.resolver.revision.SHA},
		Commands: []verification.Command{{
			Name: "required", Directory: "/tmp/workspace", Executable: "git",
			Args: []string{"status"}, Timeout: time.Minute, Required: true,
		}},
	}
	fixture.verifier.run = func(_ context.Context, command verification.Command, _ map[string]string) verification.Result {
		return verification.Result{
			Command: command, ExitCode: 1, Output: "tests failed",
			FailureClass: "verification", CauseCode: "nonzero_exit",
		}
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureVerification ||
		result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	bundle := fixture.artifacts.Snapshot().Bundles[result.Artifact.Digest]
	if len(bundle.Verification) != 1 || bundle.Verification[0].CauseCode != "nonzero_exit" {
		t.Fatalf("artifact verification = %#v", bundle.Verification)
	}
}

func TestExecuteVerificationInternalFailureKeepsInternalClass(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.profile.result = repository.ProfileResult{
		Commands: []verification.Command{{
			Name: "invalid", Directory: "/tmp/workspace", Executable: "git",
			Args: []string{"status"}, Timeout: time.Minute, Required: true,
		}},
	}
	fixture.verifier.run = func(_ context.Context, command verification.Command, _ map[string]string) verification.Result {
		return verification.Result{
			Command: command, FailureClass: "internal", CauseCode: "invalid_command",
		}
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureInternal || result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
}

func TestExecuteMissingRequiredToolIsEnvironmentFailureAndDoesNotRunAgent(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.profile.err = &repository.EnvironmentError{Operation: "required tool"}
	calls := 0
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		calls++
		return runner.ExecutionResult{}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureEnvironment || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if calls != 0 {
		t.Fatalf("agent calls = %d, want 0", calls)
	}
}

func TestExecutePromptOverflowFailsAsInputBeforeAgent(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.mem.result = []memory.Memory{{ID: "oversized", Content: strings.Repeat("x", maxAgentPromptBytes)}}
	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "oversized-prompt")); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	calls := 0
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		calls++
		return runner.ExecutionResult{}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureInput || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if calls != 0 {
		t.Fatalf("agent calls = %d, want 0", calls)
	}
}

func TestExecuteRejectsRunBindingMismatchBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(run.Record) run.Record
	}{
		{"template", func(record run.Record) run.Record {
			record.Template.Version = 2
			return record
		}},
		{"repository", func(record run.Record) run.Record {
			record.RepositoryURI = "https://other.test/repository.git"
			return record
		}},
		{"base ref", func(record run.Record) run.Record {
			record.BaseRef = "refs/heads/other"
			return record
		}},
		{"publication", func(record run.Record) run.Record {
			record.PublicationMode = "pull_request"
			return record
		}},
		{"input hash", func(record run.Record) run.Record {
			record.InputHash = strings.Repeat("0", 64)
			return record
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			fixture.runs.loadMutate = test.mutate
			calls := 0
			fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
				calls++
				return runner.ExecutionResult{}, nil
			}

			_, err := fixture.service.Execute(context.Background(), "run-123")
			if !errors.Is(err, ErrRunBinding) {
				t.Fatalf("Execute() error = %v, want %v", err, ErrRunBinding)
			}
			if fixture.workspaces.calls != 0 || calls != 0 {
				t.Fatalf("side effects: workspaces=%d agent=%d", fixture.workspaces.calls, calls)
			}
		})
	}
}

func TestExhaustRejectsRunBindingMismatch(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{}, errors.New("agent unavailable")
	}
	if _, err := fixture.service.Execute(context.Background(), "run-123"); err == nil {
		t.Fatal("Execute() error = nil")
	}
	fixture.runs.loadMutate = func(record run.Record) run.Record {
		record.Template.Version = 2
		return record
	}

	if _, err := fixture.service.Exhaust(context.Background(), "run-123", "execute"); !errors.Is(err, ErrRunBinding) {
		t.Fatalf("Exhaust() error = %v, want %v", err, ErrRunBinding)
	}
}

func TestExecuteEnvironmentPolicyDenialPersistsNoPatchOrSecret(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.env.build = func(context.Context, environment.Request) (environment.Result, error) {
		return environment.Result{}, errors.New("requested GITHUB_TOKEN value super-secret is denied")
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailurePolicy || result.Retryable ||
		result.Artifact != nil || len(fixture.artifacts.Snapshot().Saves) != 0 {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "super-secret") || strings.Contains(string(encoded), "GITHUB_TOKEN") {
		t.Fatalf("run evidence leaked denial details: %s", encoded)
	}
}

func TestExecuteChangePolicyDenialPersistsOnlySafeFindings(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		result := capturedChange()
		result.Patch = []byte("TOP_SECRET=never-persist-this")
		return result, nil
	}
	fixture.policy.decision = policy.Decision{
		Allowed: false,
		Findings: []policy.Finding{{
			RuleID: "secret-assignment", Path: "/tmp/workspace/changed.txt", Line: 1,
		}},
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailurePolicy || result.Artifact != nil ||
		len(fixture.artifacts.Snapshot().Saves) != 0 {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "never-persist-this") ||
		strings.Contains(string(encoded), "/tmp/workspace") ||
		!strings.Contains(string(encoded), "secret-assignment") ||
		!strings.Contains(string(encoded), "changed.txt") {
		t.Fatalf("policy evidence = %s", encoded)
	}
}

func TestExecuteCancellationCleansWithNonCanceledContext(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	cleanWorkspace := false
	cleanRuntime := false
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatalf("workspace cleanup context canceled: %v", ctx.Err())
			}
			cleanWorkspace = true
			return nil
		}}, nil
	}}
	fixture.env.cleanup = func(ctx context.Context, _ string) error {
		if ctx.Err() != nil {
			t.Fatalf("runtime cleanup context canceled: %v", ctx.Err())
		}
		cleanRuntime = true
		return nil
	}
	started := make(chan struct{})
	fixture.agent.run = func(ctx context.Context, _ runner.RunRequest) (runner.ExecutionResult, error) {
		close(started)
		<-ctx.Done()
		return runner.ExecutionResult{Started: true}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if !cleanWorkspace || !cleanRuntime {
		t.Fatalf("cleanup workspace=%t runtime=%t", cleanWorkspace, cleanRuntime)
	}
}

func TestExecuteArtifactSaveCancellationPersistsTerminalCanceled(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.service.artifacts = artifactmock.NewStore(artifactmock.Config{SaveError: context.Canceled})
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
}

func TestExecuteCancellationAfterArtifactSaveCheckpointsArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.service.artifacts = &artifactStoreFunc{
		Store: fixture.artifacts,
		save: func(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
			reference, err := fixture.artifacts.Save(ctx, bundle)
			cancel()
			return reference, err
		},
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	if record.Artifact == nil || *record.Artifact != *result.Artifact {
		t.Fatalf("checkpointed artifact = %#v, result = %#v", record.Artifact, result.Artifact)
	}
}

func TestExecuteCancellationDuringCleanupPersistsTerminalCanceled(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.env.cleanup = func(context.Context, string) error {
		cancel()
		return nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
}

func TestExecuteCancellationAtFinalSaveIsCompensatedDurably(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.runs.saveCalls = 0
	fixture.runs.saveHook = func(call int, _ run.Record) {
		if call == 3 {
			cancel()
		}
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	if record.Status != run.StatusCanceled || record.Artifact == nil {
		t.Fatalf("durable record = %#v", record)
	}
}

func TestExecuteCleanupFailureJoinsPrimaryWithoutReplacingClass(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	primary := errors.New("agent transport failed")
	cleanup := errors.New("workspace cleanup failed")
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{}, primary
	}
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(context.Context) error { return cleanup }}, nil
	}}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("Execute() error = %v, want joined primary and cleanup", err)
	}
	if result.FailureClass != run.FailureAgent {
		t.Fatalf("Execute() failure class = %q, want agent", result.FailureClass)
	}
	if result.Retryable {
		t.Fatal("cleanup failure left primary failure retryable")
	}
}

func TestExecuteCleanupUsesIndependentBudgetsAndAlwaysAttemptsBoth(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.service.cleanupTimeout = 20 * time.Millisecond
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{}, errors.New("agent unavailable")
	}
	runtimeAttempted := false
	workspaceAttempted := false
	fixture.env.cleanup = func(ctx context.Context, _ string) error {
		runtimeAttempted = true
		<-ctx.Done()
		return ctx.Err()
	}
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(ctx context.Context) error {
			workspaceAttempted = true
			if ctx.Err() != nil {
				return errors.New("workspace cleanup received starved context")
			}
			return nil
		}}, nil
	}}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureAgent || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if !runtimeAttempted || !workspaceAttempted {
		t.Fatalf("cleanup attempts runtime=%t workspace=%t", runtimeAttempted, workspaceAttempted)
	}
	if strings.Contains(err.Error(), "starved") {
		t.Fatalf("workspace cleanup reused expired runtime budget: %v", err)
	}
}

func TestExecutePartialRuntimeCleanupStillCleansWorktree(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	runtimeErr := errors.New("partial runtime cleanup")
	worktreeCleaned := false
	fixture.env.cleanup = func(context.Context, string) error { return runtimeErr }
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(context.Context) error {
			worktreeCleaned = true
			return nil
		}}, nil
	}}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if !errors.Is(err, runtimeErr) || result.FailureClass != run.FailureCleanup || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if !worktreeCleaned {
		t.Fatal("worktree cleanup was not attempted")
	}
}

func TestExhaustMakesLatestRetryableFailureTerminalAndIsIdempotent(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{}, errors.New("agent unavailable")
	}
	first, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || !first.Retryable || first.Status != run.StatusExecuting {
		t.Fatalf("Execute() result=%#v error=%v", first, err)
	}

	exhausted, err := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if err != nil || exhausted.Status != run.StatusFailed || exhausted.Retryable {
		t.Fatalf("Exhaust() result=%#v error=%v", exhausted, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if record.Failure == nil || record.Failure.CauseCode != "retries_exhausted" {
		t.Fatalf("exhausted failure = %#v", record.Failure)
	}
	again, err := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if err != nil || again.Status != run.StatusFailed {
		t.Fatalf("Exhaust(second) result=%#v error=%v", again, err)
	}
}

func TestExecuteFinalizesExpiredRunningAttemptWithoutRerunningAgent(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	if _, started, err := fixture.service.beginStage(context.Background(), "run-123", "execute", run.StatusExecuting); err != nil || !started {
		t.Fatalf("beginStage() started=%t error=%v", started, err)
	}
	clock.Advance(2 * time.Minute)
	agentCalls := 0
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		agentCalls++
		return runner.ExecutionResult{}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.Status != run.StatusFailed || result.FailureClass != run.FailureInternal || result.Retryable {
		t.Fatalf("Execute(recovery) result=%#v error=%v", result, err)
	}
	if fixture.workspaces.calls != 0 || agentCalls != 0 {
		t.Fatalf("recovery side effects workspaces=%d agent=%d", fixture.workspaces.calls, agentCalls)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if record.Failure == nil || record.Failure.CauseCode != "ambiguous_attempt" ||
		record.Stages[len(record.Stages)-1].Status != run.StageFailed {
		t.Fatalf("recovery record = %#v", record)
	}
}

func TestExecuteExpiredRunningAttemptPreservesCheckpointedArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	if _, started, err := fixture.service.beginStage(context.Background(), "run-123", "execute", run.StatusExecuting); err != nil || !started {
		t.Fatalf("beginStage() started=%t error=%v", started, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	reference := artifact.Reference{RunID: "run-123", Digest: strings.Repeat("a", 64), Size: 1024}
	record.Artifact = &reference
	if _, err := fixture.runs.Save(context.Background(), record, record.Version); err != nil {
		t.Fatalf("checkpoint artifact: %v", err)
	}
	if _, err := fixture.service.Execute(context.Background(), "run-123"); !errors.Is(err, ErrPhaseInProgress) {
		t.Fatalf("Execute(fresh checkpoint) error = %v, want %v", err, ErrPhaseInProgress)
	}
	clock.Advance(2 * time.Minute)

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.Status != run.StatusFailed || result.Artifact == nil || *result.Artifact != reference {
		t.Fatalf("Execute(recovery) result=%#v error=%v", result, err)
	}
}

func TestExecuteArtifactCheckpointSurvivesFinalPersistenceCrash(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	agentCalls := 0
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		agentCalls++
		return runner.ExecutionResult{Output: "changed", ExitCode: 0, Started: true, Completed: true}, nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}
	fixture.runs.saveCalls = 0
	finalPersistErr := errors.New("worker lost before final run persistence")
	fixture.runs.saveErrors = map[int]error{3: finalPersistErr}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if !errors.Is(err, finalPersistErr) {
		t.Fatalf("Execute() result=%#v error=%v, want final persistence failure", result, err)
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	if record.Artifact == nil || record.Stages[len(record.Stages)-1].Status != run.StageRunning {
		t.Fatalf("crash checkpoint record = %#v", record)
	}
	checkpoint := *record.Artifact

	fixture.runs.saveErrors = nil
	clock.Advance(2 * time.Minute)
	recovered := *fixture.service
	recovery, err := recovered.Execute(context.Background(), "run-123")
	if err == nil || recovery.Status != run.StatusFailed || recovery.Artifact == nil ||
		*recovery.Artifact != checkpoint || agentCalls != 1 {
		t.Fatalf("Execute(recovery) result=%#v error=%v agentCalls=%d", recovery, err, agentCalls)
	}
}

func TestExhaustFinalizesRunningAttemptInsteadOfLeavingItStuck(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	if _, started, err := fixture.service.beginStage(context.Background(), "run-123", "execute", run.StatusExecuting); err != nil || !started {
		t.Fatalf("beginStage() started=%t error=%v", started, err)
	}

	result, err := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if err != nil || result.Status != run.StatusFailed || result.Retryable {
		t.Fatalf("Exhaust() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if record.Failure == nil || record.Failure.CauseCode != "ambiguous_attempt" {
		t.Fatalf("Exhaust() failure = %#v", record.Failure)
	}
}

func resolvedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := newServiceFixture(t)
	fixture.profile.result = repository.ProfileResult{Facts: map[string]string{"base_sha": fixture.resolver.revision.SHA}}
	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "execute")); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	return fixture
}

type workspaceProfile struct{}

func (*workspaceProfile) Name() string { return "generic" }
func (*workspaceProfile) Inspect(_ context.Context, request repository.ProfileRequest) (repository.ProfileResult, error) {
	return repository.ProfileResult{
		Facts: map[string]string{
			"zeta": "last", "alpha": "first",
			"workspace": request.Workspace, "runtime_home": request.Environment["HOME"],
			"runtime_run_root": filepath.Dir(filepath.Dir(request.Environment["HOME"])),
		},
		Warnings: []string{"optional dependency unavailable at " + request.Environment["TMPDIR"]},
		Modules:  []string{"module-a", "module-b"},
		Commands: []verification.Command{
			{Name: "module-a", Directory: filepath.Join(request.Workspace, "module-a"), Executable: "git", Args: []string{"status", "--short"}, Timeout: time.Minute, Required: true},
			{Name: "module-b", Directory: filepath.Join(request.Workspace, "module-b"), Executable: "git", Args: []string{"status", "--short"}, Timeout: time.Minute, Required: true},
		},
	}, nil
}

type writingAgent struct {
	mu       sync.Mutex
	requests []runner.RunRequest
}

func (a *writingAgent) Run(_ context.Context, request runner.RunRequest) (runner.ExecutionResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	if err := os.WriteFile(filepath.Join(request.WorkspacePath, "changed.txt"), []byte("changed\n"), 0o644); err != nil {
		return runner.ExecutionResult{}, err
	}
	return runner.ExecutionResult{
		Output: "updated changed.txt in " + request.WorkspacePath, Transcript: `{"type":"item.completed"}`,
		ExitCode: 0, Started: true, Completed: true,
	}, nil
}
func (a *writingAgent) Requests() []runner.RunRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]runner.RunRequest(nil), a.requests...)
}

type recordingVerifier struct {
	mu       sync.Mutex
	commands []verification.Command
}

func (v *recordingVerifier) Run(_ context.Context, command verification.Command, _ map[string]string) verification.Result {
	v.mu.Lock()
	v.commands = append(v.commands, command)
	v.mu.Unlock()
	return verification.Result{Command: command, Output: "checked " + command.Directory, Passed: true}
}
func (v *recordingVerifier) Commands() []verification.Command {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]verification.Command(nil), v.commands...)
}

func rawForRepository(repositoryURI string) json.RawMessage {
	value := map[string]any{
		"idempotency_key": "real-worktree", "task_description": "Add changed.txt",
		"repository_uri": repositoryURI, "base_ref": "HEAD",
		"tags":    map[string]string{"user_id": "guilhermecastro", "app_id": "araihu-paje"},
		"profile": "generic",
		"checks": []map[string]any{{
			"name": "git status", "directory": ".", "executable": "git",
			"args": []string{"status", "--short"}, "timeout": "1m", "required": true,
		}},
		"publication": map[string]any{"mode": "artifact"},
	}
	raw, _ := json.Marshal(value)
	return raw
}

func createGitSource(t *testing.T) (string, string) {
	t.Helper()
	source := t.TempDir()
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "paje@example.test")
	runGit(t, source, "config", "user.name", "Paje Test")
	if err := os.MkdirAll(filepath.Join(source, "module-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "module-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "module-a", "base.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "module-b", "base.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "base")
	return source, strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func assertDirectoryEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", directory, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%q entries = %#v, want empty", directory, entries)
	}
}

func capturedChange() gitcapture.Result {
	return gitcapture.Result{
		Patch:   []byte("diff --git a/changed.txt b/changed.txt\n"),
		Changes: []artifact.Change{{Path: "changed.txt", Status: "A", NewMode: "100644"}},
		TreeSHA: "tree-sha",
	}
}

type artifactStoreFunc struct {
	artifact.Store
	save func(context.Context, artifact.Bundle) (artifact.Reference, error)
}

func (s *artifactStoreFunc) Save(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
	return s.save(ctx, bundle)
}

func structPublisherResult() publisher.Result { return publisher.Result{} }
